// The web door: POST /w (what uc.js sends) and GET /w/heatmap (what the
// overlay draws), plus POST /v1/heatmap/link, the session door that mints the
// overlay's read token. The collect gate is /i's public-key gate exactly; the
// writes never touch the log ring.

package api

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/ingest"
	"go.upcontrol.io/back/internal/ingest/scrub"
	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// webStore is the door's Postgres half plus the history wall, so the tests
// run the whole door against a fake.
type webStore interface {
	InsertPageview(ctx context.Context, tenantID, projectID int64, ts time.Time, labels map[string]string, actor string) error
	InsertWebEvent(ctx context.Context, tenantID, projectID int64, ts time.Time, name string, labels map[string]string, actor string) error
	AddHeat(ctx context.Context, tenantID, projectID int64, day time.Time, path, device, segment, source string, cells []pgstore.HeatAdd) error
	AddEngage(ctx context.Context, tenantID, projectID int64, hour time.Time, path, device, segment, source string, add pgstore.EngageAdd) error
	AdmitSegment(ctx context.Context, projectID int64, name string) (bool, error)
	Segments(ctx context.Context, projectID int64) ([]string, error)
	DailySalt(ctx context.Context, day time.Time) ([]byte, error)
	PublicOrigins(ctx context.Context, tenantID, projectID int64) ([]string, error)
	MintHeatmapLink(ctx context.Context, tokenHash []byte, tenantID, projectID int64, expires time.Time) error
	ResolveHeatmapLink(ctx context.Context, tokenHash []byte) (tenantID, projectID int64, ok bool, err error)
	PageviewCount(ctx context.Context, tenantID, projectID int64, path, device, segment, source string, from, toDay time.Time) (int64, error)
	HeatCells(ctx context.Context, tenantID, projectID int64, path, device, segment, source, kind string, fromDay, toDay time.Time, limit int) ([]pgstore.HeatCell, error)
	ScrollReach(ctx context.Context, tenantID, projectID int64, path, device, segment, source string, fromDay, toDay time.Time) ([21]int64, error)
	FormStats(ctx context.Context, tenantID, projectID int64, path, device, segment, source string, fromDay, toDay time.Time) ([]pgstore.FormStat, error)
	RecentDeploys(ctx context.Context, tenantID, projectID int64, from, to time.Time) (deploys, releases []pgstore.MarkRow, err error)
	HistoryRefusal(ctx context.Context, tenantID int64, days int) (msg, plan string, err error)
	WebQuota(ctx context.Context, tenantID int64, now time.Time) (pgstore.WebQuota, error)
}

// webDoor is the /w handler pair. The day's visitor salt is cached per UTC
// date: one database read per day, not one per beacon. Segment admissions are
// cached per project and name for segmentTTL, refusals included, so a site
// marking every beacon costs one query per name per ten minutes.
type webDoor struct {
	ing      *ingest.Ingester
	store    webStore
	describe func(ip, ua string) (country, os, browser string)
	scrubOff bool
	now      func() time.Time

	mu       sync.Mutex
	saltDay  string
	salt     []byte
	segments map[segmentKey]segmentAnswer
}

type segmentKey struct {
	project int64
	name    string
}

type segmentAnswer struct {
	ok    bool
	until time.Time
}

const (
	segmentTTL       = 10 * time.Minute
	maxSegmentCached = 10000
)

// webStorePool is the production webStore: the pgstore methods plus the
// history wall, read from the same pool every other door reads.
type webStorePool struct {
	*pgstore.Store
	pool *pg.Pool
}

// HistoryRefusal is the history axis's wall for a heatmap read — the same
// predicate POST /v1/series applies, with the range's whole days in place of
// the batch's widest query.
func (s webStorePool) HistoryRefusal(ctx context.Context, tenantID int64, days int) (string, string, error) {
	return historyRefusalDays(ctx, s.pool, tenantID, days)
}

// RecentDeploys is the board's deploy and release mark reads over [from, to).
func (s webStorePool) RecentDeploys(ctx context.Context, tenantID, projectID int64, from, to time.Time) ([]pgstore.MarkRow, []pgstore.MarkRow, error) {
	deploys, err := s.DeployMarks(ctx, tenantID, projectID, from, to)
	if err != nil {
		return nil, nil, err
	}
	releases, err := s.ReleaseMarks(ctx, tenantID, projectID, from, to)
	return deploys, releases, err
}

// NewWebDoor wires the door. describe is the recorder's Describe; scrubOff is
// the deployment's UC_SCRUB switch.
func NewWebDoor(ing *ingest.Ingester, pgs *pgstore.Store, pool *pg.Pool, describe func(ip, ua string) (string, string, string), scrubOff bool) *webDoor {
	return &webDoor{
		ing:      ing,
		store:    webStorePool{Store: pgs, pool: pool},
		describe: describe,
		scrubOff: scrubOff,
		now:      time.Now,
	}
}

// The shared path caps. Bytes, not runes: the cap is on what gets stored.
// maxWebPath fits a percent-encoded non-ASCII slug, and with maxHeatSelector
// keeps a web_heat key well under the btree row limit.
const (
	maxWebPath      = 1024
	maxHeatSelector = 256
	maxHeatRead     = 2000
	webBodyBytes    = 64 << 10
)

// deviceFor buckets a viewport width the way the beacon's w is bucketed: the
// layout, and so the map, follows the width.
func deviceFor(w int) string {
	switch {
	case w < 768:
		return "mobile"
	case w < 1024:
		return "tablet"
	default:
		return "desktop"
	}
}

// webPath validates and scrubs a page path. The collect AND the read both run
// it, so a scrubbed path keys the same rows on both sides. The cap is measured
// after the scrub, because a redaction marker can outgrow what it replaced.
// A path is the beacon's key, not a cell, so an over-long one is refused
// rather than cut: two cut paths would share one map.
func webPath(p string, scrubOff bool) (string, bool) {
	if !scrubOff {
		p = scrub.Scrub(p).Cleaned
	}
	if !strings.HasPrefix(p, "/") || len(p) > maxWebPath {
		return "", false
	}
	return p, true
}

// referrerHost reduces a referrer URL to its host, lowercased without a
// leading "www.". "" when unparsable, empty, or the page's own host — a visit
// the page itself sent is not a source.
func referrerHost(ref, pageHost string) string {
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if pageHost != "" && host == strings.TrimPrefix(strings.ToLower(pageHost), "www.") {
		return ""
	}
	return host
}

// utmLabels keeps the three UTM parameters a page view stores, scrubbed and
// capped like every stored string. Nil when none is worth keeping.
func utmLabels(q string, scrubOff bool) map[string]string {
	vals, err := url.ParseQuery(strings.TrimPrefix(q, "?"))
	if err != nil {
		return nil
	}
	var out map[string]string
	for _, key := range [...]string{"utm_source", "utm_medium", "utm_campaign"} {
		if v := pageValue(vals.Get(key), scrubOff); v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = v
		}
	}
	return out
}

// pageValue is a page-sent label value as stored: scrubbed, then cut to 100
// bytes.
func pageValue(v string, scrubOff bool) string {
	if !scrubOff {
		v = scrub.Scrub(v).Cleaned
	}
	if len(v) > 100 {
		v = v[:100]
	}
	return v
}

// eventName is an event beacon's n as stored: trimmed and lowercased, 1-40
// bytes, no control characters, never one of the server's own uc. names.
func eventName(n string) (string, bool) {
	n = strings.ToLower(strings.TrimSpace(n))
	if n == "" || len(n) > 40 || strings.HasPrefix(n, "uc.") || strings.ContainsFunc(n, unicode.IsControl) {
		return "", false
	}
	return n, true
}

// webServerLabels are the labels the door writes on an event itself; a page
// field of the same name is dropped.
var webServerLabels = map[string]bool{"path": true, "device": true, "segment": true, "source": true}

// eventFields keeps an event beacon's string fields: keys 1-60 bytes, never
// uc. or a server label, values through pageValue, the first 10 keys in sorted
// order. Anything else in l, l itself included, is ignored, never a 400.
func eventFields(raw json.RawMessage, scrubOff bool) map[string]string {
	// raw parsed with the body, so the one error left is l not being an
	// object, which leaves in empty.
	var in map[string]json.RawMessage
	_ = json.Unmarshal(raw, &in)
	out := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(in)) {
		if len(out) == 10 {
			break
		}
		v, ok := rawCellString(in[k])
		if !ok || k == "" || len(k) > 60 || strings.HasPrefix(k, "uc.") || webServerLabels[k] {
			continue
		}
		if v = pageValue(v, scrubOff); v != "" {
			out[k] = v
		}
	}
	return out
}

// webSources are the source classes uc.js works out for a page load.
var webSources = map[string]bool{"search": true, "social": true, "campaign": true, "referral": true, "direct": true}

// validSegment is a segment's shape: 1-24 of a-z, 0-9, _ and -, never four
// digits in a row, so it can name a group of visitors and never carry an id.
func validSegment(s string) bool {
	return segmentShape.MatchString(s) && !fourDigits.MatchString(s)
}

var (
	segmentShape = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	fourDigits   = regexp.MustCompile(`[0-9]{4}`)
)

// segment answers the beacon's g as stored: itself when valid and one of the
// project's segments, empty otherwise. The store is asked outside the lock.
func (d *webDoor) segment(ctx context.Context, now time.Time, projectID int64, g string) (string, error) {
	if !validSegment(g) {
		return "", nil
	}
	key := segmentKey{projectID, g}
	d.mu.Lock()
	a, hit := d.segments[key]
	d.mu.Unlock()
	if !hit || now.After(a.until) {
		ok, err := d.store.AdmitSegment(ctx, projectID, g)
		if err != nil {
			return "", err
		}
		a = segmentAnswer{ok: ok, until: now.Add(segmentTTL)}
		d.mu.Lock()
		// ponytail: the whole cache drops at the bound; per-entry eviction if
		// a deployment ever holds that many live names.
		if d.segments == nil || len(d.segments) >= maxSegmentCached {
			d.segments = map[segmentKey]segmentAnswer{}
		}
		d.segments[key] = a
		d.mu.Unlock()
	}
	if !a.ok {
		return "", nil
	}
	return g, nil
}

// rangeDaysWeb normalises the heatmap range enum to whole days, the unit the
// history axis sells; "" is valid and resolved by deepestRange.
var rangeDaysWeb = map[string]int{"": 0, "24h": 1, "7d": 7, "31d": 31}

// formKinds maps a beacon's form item kind to its web_heat kind; maxFormItems
// caps the items one beacon sends.
var formKinds = map[string]string{"focus": "ffocus", "fill": "ffill", "submit": "fsubmit", "left": "fleft"}

const maxFormItems = 100

// beaconBody is what uc.js sends. The cell entries ride json.RawMessage so a
// malformed one is skipped, never a 400 for the whole batch; e and a are raw
// whole, so a wrong-typed one is ignored.
type beaconBody struct {
	V int                 `json:"v"`
	T string              `json:"t"`
	P string              `json:"p"`
	W int                 `json:"w"`
	R string              `json:"r"`
	Q string              `json:"q"`
	F string              `json:"f"`
	G string              `json:"g"`
	O string              `json:"o"`
	N string              `json:"n"`
	L json.RawMessage     `json:"l"`
	S *float64            `json:"s"`
	E json.RawMessage     `json:"e"`
	A json.RawMessage     `json:"a"`
	H []json.RawMessage   `json:"h"`
	C [][]json.RawMessage `json:"c"`
	M [][]json.RawMessage `json:"m"`
	D [][]json.RawMessage `json:"d"`
	// Fm is [kind, selector] pairs: the form items the view touched.
	Fm [][]json.RawMessage `json:"fm"`
}

// rawCellString and rawCellInt decode one beacon cell element; false when the
// element is not of that type.
func rawCellString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func rawCellInt(raw json.RawMessage) (int64, bool) {
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return int64(n), true
}

// Collect is POST /w: one beacon, public keys only. The gates are /i's
// (GatePublic); a secret key here is a 401, because it would be sitting in a
// page. Every list is capped and every number clamped rather than refused.
//
// ponytail: one synchronous INSERT per beacon; batch through the spool if a
// site's traffic makes it show.
func (d *webDoor) Collect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := r.URL.Query().Get("key")
	t, err := d.ing.ResolveKey(ctx, key)
	if err != nil || t.Kind != ingest.KeyKindPublic {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return
	}
	if !d.ing.GatePublic(w, r, t, key) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, webBodyBytes+1))
	if err != nil || len(body) > webBodyBytes {
		writeAPIErr(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	// The script sends text/plain so the browser posts without a preflight,
	// so the Content-Type is ignored here.
	var b beaconBody
	path, ok := "", false
	if json.Unmarshal(body, &b) == nil {
		path, ok = webPath(b.P, d.scrubOff)
	}
	// An event's bad name is the page's own bug, so it is a 400 the page's
	// network tab shows.
	name, okName := eventName(b.N)
	if !ok || (b.T != "view" && b.T != "heat" && (b.T != "event" || !okName)) || b.W <= 0 {
		writeAPIErr(w, http.StatusBadRequest, "bad_beacon")
		return
	}
	device := deviceFor(min(b.W, 10000))
	now := d.now().UTC()
	// Past the plan's visits this month nothing is stored, and the answer is
	// an accepted beacon's: the visitor's browser has nothing to be told.
	quota, err := d.store.WebQuota(ctx, t.TenantID, now)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if quota.MaxVisits != nil && quota.Visits >= int64(*quota.MaxVisits) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip, ua := analytics.ClientIP(r), r.UserAgent()
	// A segment or source the door cannot keep is stored as none, never a 400:
	// the visitor's browser has nothing to be told.
	segment, err := d.segment(ctx, now, t.ProjectID, b.G)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	source := b.O
	if !webSources[source] {
		source = ""
	}

	if b.T == "event" {
		labels := eventFields(b.L, d.scrubOff)
		labels["path"], labels["device"] = path, device
		if segment != "" {
			labels["segment"] = segment
		}
		if source != "" {
			labels["source"] = source
		}
		actor, err := d.visitor(ctx, now, t.ProjectID, ip, ua)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := d.store.InsertWebEvent(ctx, t.TenantID, t.ProjectID, now, name, labels, actor); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if b.T == "view" {
		labels := map[string]string{"path": path, "device": device}
		if segment != "" {
			labels["segment"] = segment
		}
		if source != "" {
			labels["source"] = source
		}
		// The gate passed, so the Origin header IS the matched origin; its
		// host is what a self-referral is dropped against.
		if b.R != "" {
			if origin, err := url.Parse(r.Header.Get("Origin")); err == nil {
				if host := referrerHost(b.R, origin.Hostname()); host != "" {
					labels["referrer"] = host
				}
			}
		}
		for k, v := range utmLabels(b.Q, d.scrubOff) {
			labels[k] = v
		}
		if from, ok := webPath(b.F, d.scrubOff); ok {
			labels["from"] = from
		}
		country, osName, browser := d.describe(ip, ua)
		if country != "" {
			labels["country"] = country
		}
		if osName != "" {
			labels["os"] = osName
		}
		if browser != "" {
			labels["browser"] = browser
		}
		actor, err := d.visitor(ctx, now, t.ProjectID, ip, ua)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		if err := d.store.InsertPageview(ctx, t.TenantID, t.ProjectID, now, labels, actor); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// heat: everything one page view did, merged by cell so one statement
	// adds it. Duplicate entries fold into one row with the summed n.
	type cellKey struct {
		kind, sel string
		fx, fy    int16
	}
	cells := map[cellKey]int64{}
	add := func(kind, sel string, fx, fy int16, n int64) {
		cells[cellKey{kind, sel, fx, fy}] += n
	}
	for i, entry := range b.C {
		if i >= pgstore.MaxClickCells {
			break
		}
		if len(entry) != 5 {
			continue
		}
		sel, okSel := rawCellString(entry[0])
		fx, okFX := rawCellInt(entry[1])
		fy, okFY := rawCellInt(entry[2])
		clicks, okClicks := rawCellInt(entry[3])
		rage, okRage := rawCellInt(entry[4])
		if !okSel || !okFX || !okFY || !okClicks || !okRage || sel == "" || len(sel) > maxHeatSelector {
			continue
		}
		fx, fy = min(max(fx, 0), 63), min(max(fy, 0), 63)
		clicks = min(max(clicks, 1), 1000)
		rage = min(max(rage, 0), clicks)
		add("click", sel, int16(fx), int16(fy), clicks)
		if rage > 0 {
			add("rage", sel, int16(fx), int16(fy), rage)
		}
	}
	// Moves and dead clicks share one cell shape: [sel, fx, fy, n].
	for _, set := range [...]struct {
		kind    string
		entries [][]json.RawMessage
		cap     int
	}{{"move", b.M, pgstore.MaxMoveCells}, {"dead", b.D, pgstore.MaxClickCells}} {
		for i, entry := range set.entries {
			if i >= set.cap {
				break
			}
			if len(entry) != 4 {
				continue
			}
			sel, okSel := rawCellString(entry[0])
			fx, okFX := rawCellInt(entry[1])
			fy, okFY := rawCellInt(entry[2])
			n, okN := rawCellInt(entry[3])
			if !okSel || !okFX || !okFY || !okN || sel == "" || len(sel) > maxHeatSelector {
				continue
			}
			add(set.kind, sel, int16(min(max(fx, 0), 63)), int16(min(max(fy, 0), 63)), min(max(n, 1), 1000))
		}
	}
	// A hit is one view that clicked an element: one per selector a beacon.
	hits := map[string]bool{}
	for _, raw := range b.H {
		if len(hits) >= pgstore.MaxClickCells {
			break
		}
		sel, ok := rawCellString(raw)
		if !ok || sel == "" || len(sel) > maxHeatSelector || hits[sel] {
			continue
		}
		hits[sel] = true
		add("hit", sel, 0, 0, 1)
	}
	// A form item is one view that touched a field or form: one per kind and
	// selector a beacon, like a hit.
	forms := map[[2]string]bool{}
	for i, entry := range b.Fm {
		if i >= maxFormItems {
			break
		}
		if len(entry) != 2 {
			continue
		}
		k, okKind := rawCellString(entry[0])
		sel, okSel := rawCellString(entry[1])
		kind := formKinds[k]
		if !okKind || !okSel || kind == "" || sel == "" || len(sel) > maxHeatSelector || forms[[2]string{kind, sel}] {
			continue
		}
		forms[[2]string{kind, sel}] = true
		add(kind, sel, 0, 0, 1)
	}
	if b.S != nil {
		// fy = int(s * 20): a full-page reach lands on 20, the scroll fold's
		// 21st bucket.
		add("scroll", "", 0, int16(min(max(int64(*b.S*20), 0), 20)), 1)
	}
	var engage pgstore.EngageAdd
	if ms, ok := rawCellInt(b.E); ok {
		engage.MS = min(max(ms, 0), 1_800_000)
	}
	var reached []json.RawMessage
	if json.Unmarshal(b.A, &reached) == nil {
		thresholds := [...]*int64{&engage.R10, &engage.R30, &engage.R120, &engage.R300}
		for _, raw := range reached {
			if i, ok := rawCellInt(raw); ok && i >= 0 && i < int64(len(thresholds)) {
				*thresholds[i] = 1
			}
		}
	}
	if len(cells) > 0 {
		list := make([]pgstore.HeatAdd, 0, len(cells))
		for k, n := range cells {
			list = append(list, pgstore.HeatAdd{Kind: k.kind, Selector: k.sel, FX: k.fx, FY: k.fy, N: n})
		}
		if err := d.store.AddHeat(ctx, t.TenantID, t.ProjectID, now.Truncate(24*time.Hour), path, device, segment, source, list); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if engage != (pgstore.EngageAdd{}) {
		// ponytail: the beacon's own hour, which may be the hour after its
		// view; the reads divide by views across that edge.
		if err := d.store.AddEngage(ctx, t.TenantID, t.ProjectID, now.Truncate(time.Hour), path, device, segment, source, engage); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// deepestRange is the widest heatmap range the tenant's plan reaches. Every plan
// reaches a day, so 24h is the floor rather than a refusal.
func (d *webDoor) deepestRange(ctx context.Context, tenantID int64) (string, int, error) {
	for _, r := range [...]string{"31d", "7d"} {
		days := rangeDaysWeb[r]
		msg, _, err := d.store.HistoryRefusal(ctx, tenantID, days)
		if err != nil {
			return "", 0, err
		}
		if msg == "" {
			return r, days, nil
		}
	}
	return "24h", 1, nil
}

// visitor hashes a page view's visitor for one UTC day: the day's salt, the
// project and the address+browser pair. A visitor is countable without a
// cookie, and the salt's daily death makes the hash useless for following
// them tomorrow. The "w:" prefix marks that daily identity, so a read across
// days (retention) can leave it out.
func (d *webDoor) visitor(ctx context.Context, now time.Time, projectID int64, ip, ua string) (string, error) {
	day := now.Truncate(24 * time.Hour)
	d.mu.Lock()
	defer d.mu.Unlock()
	if key := day.Format("2006-01-02"); d.saltDay != key {
		salt, err := d.store.DailySalt(ctx, day)
		if err != nil {
			return "", err
		}
		d.saltDay, d.salt = key, salt
	}
	var project [8]byte
	binary.BigEndian.PutUint64(project[:], uint64(projectID))
	h := sha256.New()
	h.Write(d.salt)
	h.Write(project[:])
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return "w:" + hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// heatOut is the /w/heatmap answer, the Heatmap contract's shape; empty lists
// are [], never null, because HeatCells answers [] for no rows.
type heatOut struct {
	Path   string `json:"path"`
	Device string `json:"device"`
	Range  string `json:"range"`
	heatWindow
	// The project's latest deploys and releases inside the web depth.
	Deploys []heatDeploy `json:"deploys"`
	// With a split: the window before it.
	Before *heatWindow `json:"before,omitempty"`
	// The project's segments, oldest first: what the overlay filters by.
	Segments []string `json:"segments"`
	// The plan's heatmap pages; absent when unlimited.
	HeatPages *int32 `json:"heatPages,omitempty"`
}

// heatWindow is one window's numbers. From is set with a split, To and a nil
// Moves only on the before window (HeatmapWindow carries no moves).
type heatWindow struct {
	From   string             `json:"from,omitzero"`
	To     string             `json:"to,omitzero"`
	Views  int64              `json:"views"`
	Clicks []pgstore.HeatCell `json:"clicks"`
	Rage   []pgstore.HeatCell `json:"rage"`
	Moves  []pgstore.HeatCell `json:"moves,omitzero"`
	Hits   []pgstore.HeatCell `json:"hits"`
	Dead   []pgstore.HeatCell `json:"dead"`
	Scroll []int64            `json:"scroll"`
	Forms  []pgstore.FormStat `json:"forms"`
}

// heatDeploy is one entry of the answer's deploys.
type heatDeploy struct {
	At    time.Time `json:"at"`
	Label string    `json:"label"`
}

// Heatmap is GET /w/heatmap: what the overlay draws. Authorised by the uch_
// token a heatmap link carries AND the page's own public key (the tag's
// data-key), which must be the token's project's: anyone may list a stranger's
// origin on a key of their own, so without it their token would draw their
// data on the stranger's page. The CORS headers are present only for an
// Origin listed on one of the project's live public keys.
func (d *webDoor) Heatmap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	token := q.Get("token")
	ok := strings.HasPrefix(token, "uch_")
	var tenantID, projectID int64
	if ok {
		sum := sha256.Sum256([]byte(token))
		var err error
		tenantID, projectID, ok, err = d.store.ResolveHeatmapLink(ctx, sum[:])
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	if ok {
		t, err := d.ing.ResolveKey(ctx, q.Get("key"))
		ok = err == nil && t.Kind == ingest.KeyKindPublic && t.TenantID == tenantID && t.ProjectID == projectID
	}
	if !ok {
		// Echo the Origin so the overlay can read the refusal at all; the
		// answer carries nothing a stranger does not already know.
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		writeAPIErrMsg(w, http.StatusUnauthorized, "bad_token", "This heatmap link has expired. Open it again from UpControl.")
		return
	}
	origins, err := d.store.PublicOrigins(ctx, tenantID, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// From here on every answer is readable from the page's own origin. No
	// Allow-Credentials: the overlay's read sends none.
	if origin := r.Header.Get("Origin"); origin != "" && slices.Contains(origins, origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	path, okPath := webPath(q.Get("path"), d.scrubOff)
	width, errWidth := strconv.Atoi(q.Get("w"))
	rng := q.Get("range")
	days, okRange := rangeDaysWeb[rng]
	// A reader's typo is not data: an invalid filter is a 400 here, where a
	// beacon's would be stored as none.
	segment, source := q.Get("segment"), q.Get("source")
	okFilter := (segment == "" || validSegment(segment)) && (source == "" || webSources[source])
	var split time.Time
	var errSplit error
	if s := q.Get("split"); s != "" {
		split, errSplit = time.Parse(time.RFC3339, s)
	}
	if !okPath || errWidth != nil || width <= 0 || !okRange || !okFilter || errSplit != nil {
		writeAPIErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	// The web depth: how far back this plan's heat reaches, and so which
	// deploys the overlay may offer and which splits it may ask for.
	deepRng, deepDays, err := d.deepestRange(ctx, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	now := d.now().UTC()
	today := now.Truncate(24 * time.Hour)
	var splitDay, beforeFrom time.Time
	wall := days // the oldest day read, in days back from today; 0 is no wall
	switch {
	case !split.IsZero():
		// After the split: its UTC day to today, n days. Before it: the n days
		// before that. `range` is ignored; the answer names the deepest.
		splitDay = split.UTC().Truncate(24 * time.Hour)
		if splitDay.After(today) || splitDay.Before(today.AddDate(0, 0, -deepDays)) {
			writeAPIErr(w, http.StatusBadRequest, "bad_request")
			return
		}
		days = int(today.Sub(splitDay) / (24 * time.Hour)) // back to the split's day: n is days+1
		beforeFrom = splitDay.AddDate(0, 0, -(days + 1))
		// The wall is the before window's first day, capped at 31: no web
		// data is kept past them whatever the plan.
		rng, wall = deepRng, min(2*days+1, rangeDaysWeb["31d"])
	case rng == "":
		// No range asked is the overlay's first look: the deepest one the plan reaches, so
		// opening a map is never a wall. The wall stays where the reader asks for more.
		rng, days, wall = deepRng, deepDays, 0
	}
	if wall > 0 {
		if msg, plan, err := d.store.HistoryRefusal(ctx, tenantID, wall); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		} else if msg != "" {
			writeUpgradeRequired(w, msg, plan)
			return
		}
	}
	fromDay := today.AddDate(0, 0, -days)
	device := deviceFor(width)
	quota, err := d.store.WebQuota(ctx, tenantID, now)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	after, err := d.heatRead(ctx, tenantID, projectID, path, device, segment, source, today, fromDay, time.Time{})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	var before *heatWindow
	if !split.IsZero() {
		after.From = splitDay.Format(time.DateOnly)
		b, err := d.heatRead(ctx, tenantID, projectID, path, device, segment, source, today, beforeFrom, splitDay)
		if err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		b.From, b.To, b.Moves = beforeFrom.Format(time.DateOnly), splitDay.Format(time.DateOnly), nil
		before = &b
	}
	deploys, releases, err := d.store.RecentDeploys(ctx, tenantID, projectID, today.AddDate(0, 0, -deepDays), today.AddDate(0, 0, 1))
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	segments, err := d.store.Segments(ctx, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, heatOut{
		Path:       path,
		Device:     device,
		Range:      rng,
		heatWindow: after,
		Deploys:    heatDeploys(deploys, releases),
		Before:     before,
		Segments:   segments,
		HeatPages:  quota.HeatPages,
	})
}

// heatRead reads one window's numbers: page views over [from, to), and the
// heat over the same days with each bound snapped by weekStart. A zero to
// reads to now.
func (d *webDoor) heatRead(ctx context.Context, tenantID, projectID int64, path, device, segment, source string, today, from, to time.Time) (heatWindow, error) {
	var out heatWindow
	var err error
	if out.Views, err = d.store.PageviewCount(ctx, tenantID, projectID, path, device, segment, source, from, to); err != nil {
		return out, err
	}
	from, to = weekStart(from, today), weekStart(to, today)
	read := func(kind string) []pgstore.HeatCell {
		if err != nil {
			return nil
		}
		var cells []pgstore.HeatCell
		cells, err = d.store.HeatCells(ctx, tenantID, projectID, path, device, segment, source, kind, from, to, maxHeatRead)
		return cells
	}
	out.Clicks, out.Rage, out.Moves, out.Hits, out.Dead = read("click"), read("rage"), read("move"), read("hit"), read("dead")
	if err != nil {
		return out, err
	}
	scroll, err := d.store.ScrollReach(ctx, tenantID, projectID, path, device, segment, source, from, to)
	if err != nil {
		return out, err
	}
	out.Scroll = scroll[:]
	out.Forms, err = d.store.FormStats(ctx, tenantID, projectID, path, device, segment, source, from, to)
	return out, err
}

// weekStart snaps a heat bound older than a week back to its ISO Monday: past
// a week the heat is in weekly rows on their Monday, so a bound on any other
// day would cut that week in half. A zero bound stays zero.
// ponytail: past a week, a comparison is by whole weeks: the split's week
// counts on the after side, whole, and a before window reaches back to its
// first day's Monday. Day-exact old heat would need daily rows kept a month.
func weekStart(day, today time.Time) time.Time {
	if day.IsZero() || today.Sub(day) <= 7*24*time.Hour {
		return day
	}
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

// heatDeploys words the deploy and release rows the way the board's marks do,
// a burst of deploy events folded into one, and keeps the latest
// maxHeatDeploys, newest first.
func heatDeploys(deploys, releases []pgstore.MarkRow) []heatDeploy {
	marks := buildMarks(deploys, releases, nil)
	out := []heatDeploy{}
	for i := len(marks) - 1; i >= 0 && len(out) < maxHeatDeploys; i-- {
		out = append(out, heatDeploy{At: marks[i].At, Label: marks[i].Label})
	}
	return out
}

const maxHeatDeploys = 10

// postHeatmapLink is POST /v1/heatmap/link: mint the overlay's one-hour read
// token. Any member may — the link reads a page the project's own public key
// already reports on.
func (h *writeAPI) postHeatmapLink(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	var req struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// The writeAPI has no scrub switch: the hosted door always scrubs.
	path, ok := webPath(req.Path, false)
	if !ok {
		writeAPIErr(w, http.StatusBadRequest, "bad_path")
		return
	}
	project := h.currentProject(ctx, r, tenantID)
	origins, err := h.pgs.PublicOrigins(ctx, tenantID, project)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if len(origins) == 0 {
		writeAPIErrMsg(w, http.StatusConflict, "no_public_key", "Add the script to your site first: Sources, Add it to a website.")
		return
	}
	// The page's own spelling: https first, the newest key's first word
	// otherwise.
	origin := origins[0]
	for _, o := range origins {
		if strings.HasPrefix(o, "https://") {
			origin = o
			break
		}
	}
	raw := make([]byte, 32)
	if _, err := cryptorand.Read(raw); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	token := "uch_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().UTC().Add(time.Hour)
	if err := h.pgs.MintHeatmapLink(ctx, sum[:], tenantID, project, expires); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"url":       origin + path + "#uc-heatmap=" + token,
		"expiresAt": expires.Format(time.RFC3339),
	})
}
