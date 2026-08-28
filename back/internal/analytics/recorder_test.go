package analytics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/pgstore"
)

// fakeStore records every visitorStore call; VisitorIDByToken hands out
// sequential ids per distinct token hash, like the bigserial would.
type fakeStore struct {
	mu        sync.Mutex
	nextID    int64
	ids       map[string]int64
	firstSeen []firstTouch
	upserts   []firstTouch   // every VisitorIDByToken call, in order
	isBot     map[int64]bool // the row's is_bot, applying the SQL conflict clause
	resolves  int
	emails    []string
	persons   []int64
	accounts  int
	touches   []touchCall
	errOn     map[string]error // method name -> forced failure
}

type touchCall struct {
	id              int64
	country, device string
	n               int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{ids: map[string]int64{}, isBot: map[int64]bool{}, errOn: map[string]error{}}
}

func (f *fakeStore) VisitorIDByToken(_ context.Context, tokenHash []byte, first firstTouch, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errOn["resolve"]; err != nil {
		return 0, err
	}
	key := string(tokenHash)
	f.upserts = append(f.upserts, first)
	if id, ok := f.ids[key]; ok {
		// The conflict branch: is_bot = old AND new; a human event
		// un-flags a previously-bot-flagged row.
		f.isBot[id] = f.isBot[id] && first.IsBot
		return id, nil
	}
	f.nextID++
	f.ids[key] = f.nextID
	f.isBot[f.nextID] = first.IsBot
	f.firstSeen = append(f.firstSeen, first)
	f.resolves++
	return f.nextID, nil
}

func (f *fakeStore) LinkVisitorEmail(_ context.Context, id int64, email string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails = append(f.emails, email)
	return nil
}

func (f *fakeStore) LinkVisitorPerson(_ context.Context, id, personID, _ int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persons = append(f.persons, personID)
	return nil
}

func (f *fakeStore) MarkVisitorAccountCreated(_ context.Context, id int64, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts++
	return nil
}

func (f *fakeStore) TouchVisitorLastSeen(_ context.Context, id int64, _ time.Time, country, device string, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, touchCall{id: id, country: country, device: device, n: n})
	return nil
}

// fakeSink signals every received batch on a channel so tests can wait for
// the async flush without sleeping.
type fakeSink struct {
	mu    sync.Mutex
	rows  []pgstore.WebEventRow
	batch chan []pgstore.WebEventRow
}

func newFakeSink() *fakeSink {
	return &fakeSink{batch: make(chan []pgstore.WebEventRow, 8)}
}

func (f *fakeSink) InsertWebEvents(_ context.Context, rows []pgstore.WebEventRow) error {
	f.mu.Lock()
	f.rows = append(f.rows, rows...)
	f.mu.Unlock()
	f.batch <- rows
	return nil
}

func newTestRecorder(t *testing.T) (*Recorder, *fakeStore, *fakeSink) {
	t.Helper()
	store, sink := newFakeStore(), newFakeSink()
	r := NewRecorder(store, sink, nil)
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r, store, sink
}

func waitBatch(t *testing.T, sink *fakeSink) []pgstore.WebEventRow {
	t.Helper()
	select {
	case rows := <-sink.batch:
		return rows
	case <-time.After(5 * time.Second):
		t.Fatal("no flush within 5s")
		return nil
	}
}

func scopedCtx(token string) context.Context {
	// 8.8.8.8 resolves in the country database (US); the TEST-NET ranges do
	// not, and the country assertions below need a real hit.
	ip := "8.8.8.8"
	return WithScope(context.Background(), &scope{
		Token: token, IP: ip,
		UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	})
}

func TestRecorderTrackResolvesVisitorAndWritesRows(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()

	events := []event{
		{Name: "page_view", Path: "/", Referrer: "https://news.ycombinator.com/", UTMSource: "hn"},
		{Name: "cta_click", Path: "/", Props: map[string]string{"which": "header"}},
	}
	r.Track(scopedCtx(MintVisitorToken()), events, 0, 0)

	rows := waitBatch(t, sink)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].VisitorID != 1 {
		t.Errorf("visitor_id = %d, want 1 (first visitor)", rows[0].VisitorID)
	}
	if rows[0].Name != "page_view" || rows[0].Referrer != "https://news.ycombinator.com/" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[0].Device != "desktop" || rows[0].OS != "windows" || rows[0].Browser != "chrome" {
		t.Errorf("UA not stamped: %+v", rows[0])
	}
	// Country only when a database is installed; the recorder holds it.
	if r.geo != nil && rows[0].Country == "" {
		t.Error("country must be resolved from the scope IP")
	}
	if rows[0].PersonID != 0 || rows[0].TenantID != 0 {
		t.Errorf("anonymous track must stamp person/tenant 0: %+v", rows[0])
	}
	if rows[1].Props["which"] != "header" {
		t.Errorf("props not carried: %+v", rows[1])
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolves != 1 {
		t.Errorf("resolves = %d, want 1", store.resolves)
	}
	if got := store.firstSeen[0]; got.Referrer != "https://news.ycombinator.com/" || got.UTMSource != "hn" || got.Path != "/" || got.Device != "desktop" || got.IsBot {
		t.Errorf("first-touch = %+v", got)
	}
	if len(store.touches) != 1 || store.touches[0].n != 2 {
		t.Errorf("touches = %+v, want one touch with n=2", store.touches)
	}
}

func TestRecorderTrackStampsPersonFromSession(t *testing.T) {
	r, _, sink := newTestRecorder(t)
	r.Start()
	r.Track(scopedCtx(MintVisitorToken()), []event{{Name: "page_view", Path: "/app"}}, 7, 11)
	rows := waitBatch(t, sink)
	if rows[0].PersonID != 7 || rows[0].TenantID != 11 {
		t.Errorf("person/tenant not stamped: %+v", rows[0])
	}
}

func TestRecorderServerEventWithoutCookieIsVisitorZero(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()

	// No cookie on the scope: the event still lands, with visitor_id 0, and
	// nothing is resolved or linked in Postgres.
	r.ServerEvent(WithScope(context.Background(), &scope{IP: "8.8.8.8", UA: "curl/8.8.0"}),
		"public_check_run", 0, 0, map[string]string{"host": "example.com", "cached": "false"})

	rows := waitBatch(t, sink)
	if len(rows) != 1 || rows[0].Name != "public_check_run" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].VisitorID != 0 {
		t.Errorf("visitor_id = %d, want 0", rows[0].VisitorID)
	}
	if rows[0].Props["host"] != "example.com" || rows[0].Props["cached"] != "false" {
		t.Errorf("props = %+v", rows[0].Props)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolves != 0 || len(store.touches) != 0 {
		t.Errorf("anonymous event must not touch Postgres: %+v", store)
	}
}

func TestRecorderLinkEmailAndPersonApplyToTheVisitorRow(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	tok := MintVisitorToken()

	r.Track(scopedCtx(tok), []event{{Name: "watch_modal_open", Path: "/"}}, 0, 0)
	r.ServerEvent(scopedCtx(tok), "watch_signup", 0, 0, map[string]string{"host": "example.com"})
	r.LinkEmail(scopedCtx(tok), "founder@example.com")
	waitBatch(t, sink)

	r.LinkPerson(scopedCtx(tok), 42, 99)
	waitBatch(t, sink) // the link flush (Track-equivalent batch, zero events)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.emails) != 1 || store.emails[0] != "founder@example.com" {
		t.Errorf("emails = %v", store.emails)
	}
	if len(store.persons) != 1 || store.persons[0] != 42 {
		t.Errorf("persons = %v", store.persons)
	}
}

func TestRecorderFlushMergesFirstTouchAcrossTracks(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	tok := MintVisitorToken()

	// The link arrives BEFORE the first page_view; the one-shot first-touch
	// insert must still see the page's attribution.
	r.LinkEmail(scopedCtx(tok), "founder@example.com")
	r.Track(scopedCtx(tok), []event{{Name: "page_view", Path: "/", Referrer: "https://producthunt.com/", UTMMedium: "referral"}}, 0, 0)

	waitBatch(t, sink)
	store.mu.Lock()
	defer store.mu.Unlock()
	if got := store.firstSeen[0]; got.Referrer != "https://producthunt.com/" || got.UTMMedium != "referral" || got.Path != "/" {
		t.Errorf("first-touch = %+v, want the page_view attribution", got)
	}
}

func TestRecorderBotIsFlaggedOnFirstTouch(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	ctx := WithScope(context.Background(), &scope{Token: MintVisitorToken(), IP: "8.8.8.8", UA: "Mozilla/5.0 (compatible; Googlebot/2.1)"})
	r.Track(ctx, []event{{Name: "page_view", Path: "/"}}, 0, 0)
	rows := waitBatch(t, sink)
	if rows[0].Device != "bot" {
		t.Errorf("device = %q, want bot", rows[0].Device)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.firstSeen[0].IsBot {
		t.Error("is_bot must be set on the visitor row")
	}
}

func TestRecorderHumanEventUnFlagsBotVisitor(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	tok := MintVisitorToken()

	// Flush 1: the crawler creates the row with is_bot=true.
	botCtx := WithScope(context.Background(), &scope{Token: tok, IP: "8.8.8.8", UA: "Mozilla/5.0 (compatible; Googlebot/2.1)"})
	r.Track(botCtx, []event{{Name: "page_view", Path: "/"}}, 0, 0)
	waitBatch(t, sink)

	// Flush 2: the same cookie on a human browser; the sticky-AND conflict
	// branch flips the row to is_bot=false.
	r.Track(scopedCtx(tok), []event{{Name: "page_view", Path: "/app"}}, 0, 0)
	waitBatch(t, sink)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.upserts) != 2 || !store.upserts[0].IsBot || store.upserts[1].IsBot {
		t.Errorf("upserts = %+v, want is_bot true then false", store.upserts)
	}
	if store.isBot[1] {
		t.Error("sticky-AND must un-flag the visitor: is_bot still true after the human flush")
	}
	// The human flush is not bot-only, so it touches as usual.
	if len(store.touches) != 1 || store.touches[0].n != 1 {
		t.Errorf("touches = %+v, want one touch with n=1 for the human flush", store.touches)
	}
}

// The fake emulates the sticky-AND clause; this test pins the SQL itself, so
// reverting analytics.sql to sticky-OR would fail here even with the fake.
func TestUpsertFirstTouchStickyAndClauseIsInTheSQL(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "storage", "pg", "queries", "analytics.sql"))
	if err != nil {
		t.Fatalf("read analytics.sql: %v", err)
	}
	// is_bot appears only in UpsertWebVisitorFirst, so the clause alone
	// identifies the query; whitespace is tolerated, sticky-OR is not.
	stickyAnd := regexp.MustCompile(`is_bot\s*=\s*web_visitor\.is_bot\s+AND\s+EXCLUDED\.is_bot`)
	if !stickyAnd.Match(body) {
		t.Error("UpsertWebVisitorFirst must keep is_bot = web_visitor.is_bot AND EXCLUDED.is_bot (sticky-AND, §Decision 14)")
	}
}

func TestRecorderBotOnlyBatchSkipsLastSeenTouch(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	ctx := WithScope(context.Background(), &scope{Token: MintVisitorToken(), IP: "8.8.8.8", UA: "Mozilla/5.0 (compatible; Googlebot/2.1)"})
	r.Track(ctx, []event{{Name: "page_view", Path: "/"}, {Name: "page_view", Path: "/pricing"}}, 0, 0)
	rows := waitBatch(t, sink)

	// Bot events still land in ClickHouse and create the directory row; only
	// the touch is skipped, so last_seen never advances on bot traffic.
	if len(rows) != 2 || rows[0].VisitorID != 1 {
		t.Fatalf("rows = %+v, want 2 bot rows under visitor 1", rows)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolves != 1 || !store.firstSeen[0].IsBot {
		t.Errorf("resolves = %d, firstSeen = %+v, want the bot row created", store.resolves, store.firstSeen)
	}
	if len(store.touches) != 0 {
		t.Errorf("touches = %+v, want none for a bot-only batch", store.touches)
	}
}

func TestRecorderResolvesOncePerTokenPerFlush(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	r.Start()
	tok := MintVisitorToken()
	// Three same-visitor batches inside one flush window: one resolve.
	r.Track(scopedCtx(tok), []event{{Name: "a"}}, 0, 0)
	r.Track(scopedCtx(tok), []event{{Name: "b"}}, 0, 0)
	r.Track(scopedCtx(tok), []event{{Name: "c"}}, 0, 0)

	rows := waitBatch(t, sink)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.resolves != 1 {
		t.Errorf("resolves = %d, want 1 (deduped per flush)", store.resolves)
	}
	if len(store.touches) != 1 || store.touches[0].n != 3 {
		t.Errorf("touches = %+v, want one aggregated touch with n=3", store.touches)
	}
}

func TestRecorderFlushesAtHundredRows(t *testing.T) {
	r, _, sink := newTestRecorder(t)
	r.Start()
	// 10 batches of 10 events: the 10th crosses flushRows and the batch goes
	// out without waiting for the ticker or a Stop.
	for i := 0; i < 10; i++ {
		batch := make([]event, 10)
		for j := range batch {
			batch[j] = event{Name: "evt"}
		}
		r.Track(scopedCtx(MintVisitorToken()), batch, 0, 0)
	}
	rows := waitBatch(t, sink)
	if len(rows) != 100 {
		t.Fatalf("rows = %d, want 100", len(rows))
	}
}

func TestRecorderStopDrainsPending(t *testing.T) {
	store, sink := newFakeStore(), newFakeSink()
	r := NewRecorder(store, sink, nil)
	r.Start()
	r.Track(scopedCtx(MintVisitorToken()), []event{{Name: "page_view", Path: "/"}}, 0, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.rows) != 1 {
		t.Fatalf("rows after Stop = %d, want 1 (graceful drain)", len(sink.rows))
	}
	// Stop is idempotent and safe to call again.
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestRecorderResolveFailureKeepsEventsAtVisitorZero(t *testing.T) {
	r, store, sink := newTestRecorder(t)
	store.errOn["resolve"] = errors.New("pg down")
	r.Start()
	r.Track(scopedCtx(MintVisitorToken()), []event{{Name: "page_view"}}, 0, 0)
	rows := waitBatch(t, sink)
	if len(rows) != 1 || rows[0].VisitorID != 0 {
		t.Fatalf("rows = %+v, want the event at visitor_id 0", rows)
	}
}

func TestRecorderDropsWhenBufferFull(t *testing.T) {
	store, sink := newFakeStore(), newFakeSink()
	r := NewRecorder(store, sink, nil)
	// Deliberately NOT started: nothing drains, the buffer fills, and every
	// batch past bufferSize is dropped and counted — the WARN-line contract.
	for i := 0; i < bufferSize+50; i++ {
		r.Track(context.Background(), []event{{Name: "page_view"}}, 0, 0)
	}
	if n := r.dropped.Load(); n != 50 {
		t.Errorf("dropped = %d, want 50", n)
	}
}

func TestRecorderCountsInvalidEvents(t *testing.T) {
	r, _, sink := newTestRecorder(t)
	r.Start()
	r.CountInvalid(3)
	r.Track(scopedCtx(MintVisitorToken()), []event{{Name: "page_view"}}, 0, 0) // forces a flush
	waitBatch(t, sink)
	// The flush swaps the counter back to zero after logging it.
	if n := r.invalid.Load(); n != 0 {
		t.Errorf("invalid counter after flush = %d, want 0", n)
	}
}

func TestNilRecorderIsANoOp(t *testing.T) {
	// An unwired deployment (tests, dev without analytics) holds a nil
	// Recorder: every entry point must return, none may panic.
	var r *Recorder
	r.Start()
	r.Track(context.Background(), []event{{Name: "page_view"}}, 0, 0)
	r.ServerEvent(context.Background(), "signed_in", 1, 1, nil)
	r.LinkEmail(context.Background(), "a@b.c")
	r.LinkPerson(context.Background(), 1, 1)
	r.MarkAccountCreated(context.Background(), 1, 1)
	r.CountInvalid(1)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("nil Stop: %v", err)
	}
}
