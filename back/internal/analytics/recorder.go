package analytics

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/ch"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Flush policy (§Decision 2): the write is asynchronous and never blocks an
// HTTP response. Events accumulate in a buffered channel; the loop flushes
// every flushEvery or at flushRows events, whichever comes first. Stop drains
// the channel and performs one final flush.
const (
	flushEvery = time.Second
	flushRows  = 100
	bufferSize = 1024 // queued request-batches before dropping
)

// VisitorStore is the Postgres half: the web_visitor directory. The recorder
// speaks to this interface, not to pg.Pool, so the recorder test runs against
// a fake instead of a database.
type VisitorStore interface {
	// VisitorIDByToken resolves the visitor id for a token hash, creating the
	// row (with first-touch attribution) on first sight.
	VisitorIDByToken(ctx context.Context, tokenHash []byte, first FirstTouch, at time.Time) (int64, error)
	LinkVisitorEmail(ctx context.Context, visitorID int64, email string) error
	LinkVisitorPerson(ctx context.Context, visitorID, personID, tenantID int64, at time.Time) error
	MarkVisitorAccountCreated(ctx context.Context, visitorID int64, at time.Time) error
	TouchVisitorLastSeen(ctx context.Context, visitorID int64, at time.Time, country, device string, nEvents int64) error
}

// EventSink is the ClickHouse half; *ch.Conn satisfies it.
type EventSink interface {
	InsertWebEvents(ctx context.Context, rows []ch.WebEventRow) error
}

// PoolStore adapts the sqlc queries to VisitorStore over a pg.Pool.
type PoolStore struct{ Pool *pg.Pool }

func ts(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at.UTC(), Valid: true}
}

func (s PoolStore) VisitorIDByToken(ctx context.Context, tokenHash []byte, first FirstTouch, at time.Time) (int64, error) {
	// First-touch upsert: first non-empty value wins on every field (§Decision
	// 9), including across flush batches — a server event can create the row
	// before the referrer-carrying page_view arrives — and RETURNING id covers
	// both the insert and the conflict branch (no read-back round trip).
	return s.Pool.Queries().UpsertWebVisitorFirst(ctx, sqlc.UpsertWebVisitorFirstParams{
		TokenHash:        tokenHash,
		FirstSeenAt:      ts(at),
		FirstReferrer:    first.Referrer,
		FirstUtmSource:   first.UTMSource,
		FirstUtmMedium:   first.UTMMedium,
		FirstUtmCampaign: first.UTMCampaign,
		FirstCountry:     first.Country,
		FirstDevice:      first.Device,
		FirstPath:        first.Path,
		IsBot:            first.IsBot,
	})
}

func (s PoolStore) LinkVisitorEmail(ctx context.Context, visitorID int64, email string) error {
	return s.Pool.Queries().LinkVisitorEmail(ctx, sqlc.LinkVisitorEmailParams{Email: email, ID: visitorID})
}

func (s PoolStore) LinkVisitorPerson(ctx context.Context, visitorID, personID, tenantID int64, at time.Time) error {
	return s.Pool.Queries().LinkVisitorPerson(ctx, sqlc.LinkVisitorPersonParams{
		PersonID: &personID, TenantID: &tenantID, SignedInAt: ts(at), ID: visitorID,
	})
}

func (s PoolStore) MarkVisitorAccountCreated(ctx context.Context, visitorID int64, at time.Time) error {
	return s.Pool.Queries().MarkVisitorAccountCreated(ctx, sqlc.MarkVisitorAccountCreatedParams{
		AccountCreatedAt: ts(at), ID: visitorID,
	})
}

func (s PoolStore) TouchVisitorLastSeen(ctx context.Context, visitorID int64, at time.Time, country, device string, nEvents int64) error {
	return s.Pool.Queries().TouchVisitorLastSeen(ctx, sqlc.TouchVisitorLastSeenParams{
		LastSeenAt: ts(at), LastCountry: country, LastDevice: device, NEvents: nEvents, ID: visitorID,
	})
}

// FirstTouch is the one-shot attribution written when a visitor row is
// created (first non-empty value wins; later values never overwrite).
type FirstTouch struct {
	Referrer    string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	Country     string
	Device      string
	Path        string
	IsBot       bool
}

// track is one enqueued request-batch: everything the flush needs, with the
// raw IP and User-Agent already reduced to country/device/os/browser.
type track struct {
	at             time.Time
	tokenHash      []byte // nil = anonymous request (no uc_vid): visitor_id 0
	first          FirstTouch
	personID       int64
	tenantID       int64
	country        string
	ipHash         [8]byte
	ua             UA
	email          string
	linkPerson     bool
	accountCreated bool
	events         []Event
}

// Recorder is the async analytics writer. A nil *Recorder is a valid no-op:
// every entry point checks for nil, so tests and any unwired deployment get
// silence instead of a panic.
type Recorder struct {
	store VisitorStore
	sink  EventSink
	geo   *Geo
	log   *slog.Logger

	in      chan track
	stopCh  chan struct{}
	done    chan struct{}
	startO  sync.Once
	stopO   sync.Once
	dropped atomic.Uint64 // batches dropped on a full buffer (the one defect a monitoring tool may not hide)
	invalid atomic.Uint64 // events dropped by validation, logged once per flush
}

// NewRecorder builds a Recorder around the two stores. The GeoIP database is
// parsed here from the embedded bytes; a parse failure downgrades to
// country="" rather than failing startup.
func NewRecorder(store VisitorStore, sink EventSink, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}
	geo, err := OpenGeo()
	if err != nil {
		log.Warn("analytics: geoip database failed to parse; country unknown", "err", err)
		geo = nil
	}
	return &Recorder{
		store: store, sink: sink, geo: geo, log: log,
		in: make(chan track, bufferSize), stopCh: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start launches the flush loop. Idempotent.
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	r.startO.Do(func() { go r.run() })
}

// Stop signals the loop to drain and flush, then waits. Idempotent and safe
// to call from multiple shutdown tasks concurrently (they all wait on the
// same done channel). The ctx bounds the wait, not the final flush itself.
func (r *Recorder) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.stopO.Do(func() { close(r.stopCh) })
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Track enqueues a validated event batch from POST /public/track.
// personID/tenantID are 0 unless a live session stamped them.
func (r *Recorder) Track(ctx context.Context, events []Event, personID, tenantID int64) {
	if r == nil || len(events) == 0 {
		return
	}
	t := r.baseTrack(ctx)
	t.personID, t.tenantID = personID, tenantID
	t.events = events
	for _, e := range events {
		mergeFirst(&t.first, FirstTouch{
			Path:        e.Path,
			Referrer:    e.Referrer,
			UTMSource:   e.UTMSource,
			UTMMedium:   e.UTMMedium,
			UTMCampaign: e.UTMCampaign,
		})
	}
	r.enqueue(t)
}

// ServerEvent enqueues a single server-side event (public_check_run,
// watch_signup, magic_link_requested, signed_in, account_created). The
// visitor is resolved from the scope's uc_vid; no cookie means visitor_id 0.
func (r *Recorder) ServerEvent(ctx context.Context, name string, personID, tenantID int64, props map[string]string) {
	if r == nil {
		return
	}
	t := r.baseTrack(ctx)
	t.personID, t.tenantID = personID, tenantID
	t.events = []Event{{Name: name, Props: props}}
	r.enqueue(t)
}

// LinkEmail records an email on the visitor row (watch_signup). The email
// goes ONLY to Postgres, never into ClickHouse props (§Decision 7).
func (r *Recorder) LinkEmail(ctx context.Context, email string) {
	if r == nil || email == "" {
		return
	}
	t := r.baseTrack(ctx)
	t.email = email
	r.enqueue(t)
}

// LinkPerson stitches person/tenant onto the visitor row (signed_in).
func (r *Recorder) LinkPerson(ctx context.Context, personID, tenantID int64) {
	if r == nil || personID == 0 {
		return
	}
	t := r.baseTrack(ctx)
	t.personID, t.tenantID = personID, tenantID
	t.linkPerson = true
	r.enqueue(t)
}

// MarkAccountCreated stamps account_created_at on the visitor row.
func (r *Recorder) MarkAccountCreated(ctx context.Context, personID, tenantID int64) {
	if r == nil || personID == 0 {
		return
	}
	t := r.baseTrack(ctx)
	t.personID, t.tenantID = personID, tenantID
	t.accountCreated = true
	r.enqueue(t)
}

// CountInvalid records how many events validation dropped; the flush loop
// logs the running total once per flush instead of per request.
func (r *Recorder) CountInvalid(n int) {
	if r == nil || n == 0 {
		return
	}
	r.invalid.Add(uint64(n))
}

// baseTrack reduces the request scope to what is stored: country from the
// raw IP (used once here, then discarded), the truncated IP hash, and the
// parsed UA. This is the ONLY place the raw IP is read.
func (r *Recorder) baseTrack(ctx context.Context) track {
	t := track{at: time.Now()}
	if s := ScopeFrom(ctx); s != nil {
		if s.Token != "" {
			h := sha256.Sum256([]byte(s.Token))
			t.tokenHash = h[:]
		}
		t.country = r.geo.Country(s.IP)
		t.ipHash = IPHash(s.IP)
		t.ua = ParseUA(s.UA)
		t.first = FirstTouch{Country: t.country, Device: t.ua.Device, IsBot: t.ua.Device == "bot"}
	}
	return t
}

// enqueue never blocks: a full buffer means the flush loop cannot keep up,
// and stalling an HTTP response over analytics is forbidden (§Decision 2).
// The drop is WARN-logged and counted — silent loss is the one defect a
// monitoring tool may not have.
func (r *Recorder) enqueue(t track) {
	select {
	case r.in <- t:
	default:
		n := r.dropped.Add(1)
		r.log.Warn("analytics: buffer full; dropped a batch", "dropped_total", n)
	}
}

func (r *Recorder) run() {
	defer close(r.done)
	// (The embedded memory-reader releases nothing on Close, so no defer.)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	var pending []track
	rows := 0
	flush := func() {
		if len(pending) == 0 {
			return
		}
		r.flush(pending)
		pending = pending[:0]
		rows = 0
	}
	for {
		select {
		case t := <-r.in:
			pending = append(pending, t)
			rows += len(t.events)
			if rows >= flushRows {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-r.stopCh:
			// Drain whatever is already queued, then flush once and exit.
			for {
				select {
				case t := <-r.in:
					pending = append(pending, t)
				default:
					flush()
					return
				}
			}
		}
	}
}

// flush groups the batch by visitor token, resolves each visitor exactly
// once in Postgres, applies identity links and the last_seen touch, and
// writes all event rows to ClickHouse in one batch INSERT.
func (r *Recorder) flush(list []track) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if n := r.invalid.Swap(0); n > 0 {
		r.log.Warn("analytics: dropped invalid events", "count", n)
	}

	// Group by token BEFORE resolving: a link-only track (e.g. LinkEmail)
	// can arrive ahead of the same visitor's first page_view, and merging the
	// candidates first keeps one flush from splitting attribution across two
	// upserts. The upsert itself is first-non-empty, so a server event that
	// created the row in an earlier flush still picks up the referrer that
	// arrives late — attribution survives the cross-flush race too.
	type group struct {
		tokenHash []byte
		first     FirstTouch
		id        int64
		tracks    []track
	}
	order := make([]*group, 0, len(list))
	byToken := make(map[string]*group, len(list))
	for _, t := range list {
		key := ""
		if t.tokenHash != nil {
			key = string(t.tokenHash)
		}
		g := byToken[key]
		if g == nil {
			g = &group{tokenHash: t.tokenHash}
			byToken[key] = g
			order = append(order, g)
		}
		g.tracks = append(g.tracks, t)
		mergeFirst(&g.first, t.first)
	}

	rows := make([]ch.WebEventRow, 0, len(list))
	for _, g := range order {
		if g.tokenHash != nil {
			id, err := r.store.VisitorIDByToken(ctx, g.tokenHash, g.first, g.tracks[0].at)
			if err != nil {
				// Resolve failed: the events still go to ClickHouse with
				// visitor_id 0 (an unresolved visitor is not an unknown event),
				// and the link/touch updates are skipped — there is no row.
				r.log.Warn("analytics: visitor resolve failed", "err", err)
				id = 0
			}
			g.id = id
		}
		touchAt := time.Time{}
		var touchCountry, touchDevice string
		var nEvents int64
		botOnly := true
		for _, t := range g.tracks {
			if t.ua.Device != "bot" {
				botOnly = false
			}
			if g.id != 0 {
				var err error
				switch {
				case t.email != "":
					err = r.store.LinkVisitorEmail(ctx, g.id, t.email)
				case t.linkPerson:
					err = r.store.LinkVisitorPerson(ctx, g.id, t.personID, t.tenantID, t.at)
				case t.accountCreated:
					err = r.store.MarkVisitorAccountCreated(ctx, g.id, t.at)
				}
				if err != nil {
					r.log.Warn("analytics: visitor link failed", "err", err)
				}
			}
			if len(t.events) > 0 {
				if t.at.After(touchAt) {
					touchAt, touchCountry, touchDevice = t.at, t.country, t.ua.Device
				}
				nEvents += int64(len(t.events))
			}
			for _, e := range t.events {
				rows = append(rows, ch.WebEventRow{
					TS: t.at, VisitorID: uint64(g.id), PersonID: uint64(t.personID), TenantID: uint64(t.tenantID),
					Name: e.Name, Path: e.Path, Title: e.Title, Referrer: e.Referrer,
					UTMSource: e.UTMSource, UTMMedium: e.UTMMedium, UTMCampaign: e.UTMCampaign,
					Country: t.country, IPHash: t.ipHash, Device: t.ua.Device, OS: t.ua.OS, Browser: t.ua.Browser,
					Props: e.Props,
				})
			}
		}
		// Bot-only batches never advance last_seen or events_count (§Decision
		// 14): the directory must not rank a crawler as the most recent visitor.
		if g.id != 0 && nEvents > 0 && !botOnly {
			if err := r.store.TouchVisitorLastSeen(ctx, g.id, touchAt, touchCountry, touchDevice, nEvents); err != nil {
				r.log.Warn("analytics: visitor touch failed", "err", err)
			}
		}
	}

	// Always hand the sink the batch, even when the link-only tracks left it
	// empty: the native insert no-ops on zero rows, and callers (tests, the
	// drain path) get one completion signal per flush.
	if err := r.sink.InsertWebEvents(ctx, rows); err != nil {
		// The batch is lost (no retry queue): analytics is lossy-by-design
		// under ClickHouse outage, and the WARN is the honest record of it.
		r.log.Warn("analytics: clickhouse insert failed", "err", err, "rows", len(rows))
	}
}

// mergeFirst keeps the first non-empty value per field (first-touch wins);
// IsBot is sticky-true across the merge.
func mergeFirst(dst *FirstTouch, src FirstTouch) {
	if dst.Referrer == "" {
		dst.Referrer = src.Referrer
	}
	if dst.UTMSource == "" {
		dst.UTMSource = src.UTMSource
	}
	if dst.UTMMedium == "" {
		dst.UTMMedium = src.UTMMedium
	}
	if dst.UTMCampaign == "" {
		dst.UTMCampaign = src.UTMCampaign
	}
	if dst.Country == "" {
		dst.Country = src.Country
	}
	if dst.Device == "" {
		dst.Device = src.Device
	}
	if dst.Path == "" {
		dst.Path = src.Path
	}
	dst.IsBot = dst.IsBot || src.IsBot
}
