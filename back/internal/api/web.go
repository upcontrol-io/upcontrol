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
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	AddHeat(ctx context.Context, tenantID, projectID int64, day time.Time, path, device string, cells []pgstore.HeatAdd) error
	DailySalt(ctx context.Context, day time.Time) ([]byte, error)
	PublicOrigins(ctx context.Context, tenantID, projectID int64) ([]string, error)
	MintHeatmapLink(ctx context.Context, tokenHash []byte, tenantID, projectID int64, expires time.Time) error
	ResolveHeatmapLink(ctx context.Context, tokenHash []byte) (tenantID, projectID int64, ok bool, err error)
	PageviewCount(ctx context.Context, tenantID, projectID int64, path, device string, from time.Time) (int64, error)
	HeatCells(ctx context.Context, tenantID, projectID int64, path, device, kind string, fromDay time.Time, limit int) ([]pgstore.HeatCell, error)
	ScrollReach(ctx context.Context, tenantID, projectID int64, path, device string, fromDay time.Time) ([21]int64, error)
	HistoryRefusal(ctx context.Context, tenantID int64, days int) (msg, plan string, err error)
}

// webDoor is the /w handler pair. The day's visitor salt is cached per UTC
// date: one database read per day, not one per beacon.
type webDoor struct {
	ing      *ingest.Ingester
	store    webStore
	describe func(ip, ua string) (country, os, browser string)
	scrubOff bool
	now      func() time.Time

	mu      sync.Mutex
	saltDay string
	salt    []byte
}

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
	maxClickCells   = 300
	maxMoveCells    = 600
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
		v := vals.Get(key)
		if v == "" {
			continue
		}
		if !scrubOff {
			v = scrub.Scrub(v).Cleaned
		}
		if len(v) > 100 {
			v = v[:100]
		}
		if v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = v
		}
	}
	return out
}

// rangeDaysWeb normalises the heatmap range enum to whole days, the unit the
// history axis sells; "" is valid and resolved by deepestRange.
var rangeDaysWeb = map[string]int{"": 0, "24h": 1, "7d": 7, "31d": 31}

// beaconBody is what uc.js sends. The cell entries ride json.RawMessage so a
// malformed one is skipped, never a 400 for the whole batch.
type beaconBody struct {
	V int                 `json:"v"`
	T string              `json:"t"`
	P string              `json:"p"`
	W int                 `json:"w"`
	R string              `json:"r"`
	Q string              `json:"q"`
	S *float64            `json:"s"`
	C [][]json.RawMessage `json:"c"`
	M [][]json.RawMessage `json:"m"`
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
	if !ok || (b.T != "view" && b.T != "heat") || b.W <= 0 {
		writeAPIErr(w, http.StatusBadRequest, "bad_beacon")
		return
	}
	device := deviceFor(min(b.W, 10000))
	now := d.now().UTC()
	ip, ua := analytics.ClientIP(r), r.UserAgent()

	if b.T == "view" {
		labels := map[string]string{"path": path, "device": device}
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
		if i >= maxClickCells {
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
	for i, entry := range b.M {
		if i >= maxMoveCells {
			break
		}
		if len(entry) != 4 {
			continue
		}
		sel, okSel := rawCellString(entry[0])
		fx, okFX := rawCellInt(entry[1])
		fy, okFY := rawCellInt(entry[2])
		samples, okSamples := rawCellInt(entry[3])
		if !okSel || !okFX || !okFY || !okSamples || sel == "" || len(sel) > maxHeatSelector {
			continue
		}
		add("move", sel, int16(min(max(fx, 0), 63)), int16(min(max(fy, 0), 63)), min(max(samples, 1), 1000))
	}
	if b.S != nil {
		// fy = int(s * 20): a full-page reach lands on 20, the scroll fold's
		// 21st bucket.
		add("scroll", "", 0, int16(min(max(int64(*b.S*20), 0), 20)), 1)
	}
	list := make([]pgstore.HeatAdd, 0, len(cells))
	for k, n := range cells {
		list = append(list, pgstore.HeatAdd{Kind: k.kind, Selector: k.sel, FX: k.fx, FY: k.fy, N: n})
	}
	if err := d.store.AddHeat(ctx, t.TenantID, t.ProjectID, now.Truncate(24*time.Hour), path, device, list); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
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
// them tomorrow.
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
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// heatOut is the /w/heatmap answer, the Heatmap contract's shape; empty lists
// are [], never null, because HeatCells answers [] for no rows.
type heatOut struct {
	Path   string             `json:"path"`
	Device string             `json:"device"`
	Range  string             `json:"range"`
	Views  int64              `json:"views"`
	Clicks []pgstore.HeatCell `json:"clicks"`
	Rage   []pgstore.HeatCell `json:"rage"`
	Moves  []pgstore.HeatCell `json:"moves"`
	Scroll []int64            `json:"scroll"`
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
	if !okPath || errWidth != nil || width <= 0 || !okRange {
		writeAPIErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if rng == "" {
		// No range asked is the overlay's first look: the deepest one the plan reaches, so
		// opening a map is never a wall. The wall stays where the reader asks for more.
		var err error
		if rng, days, err = d.deepestRange(ctx, tenantID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	} else if msg, plan, err := d.store.HistoryRefusal(ctx, tenantID, days); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	} else if msg != "" {
		writeUpgradeRequired(w, msg, plan)
		return
	}
	device := deviceFor(width)
	fromDay := d.now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -days)
	views, err := d.store.PageviewCount(ctx, tenantID, projectID, path, device, fromDay)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	read := func(kind string) ([]pgstore.HeatCell, bool) {
		cells, err := d.store.HeatCells(ctx, tenantID, projectID, path, device, kind, fromDay, maxHeatRead)
		return cells, err == nil
	}
	clicks, okClicks := read("click")
	rage, okRage := read("rage")
	moves, okMoves := read("move")
	if !okClicks || !okRage || !okMoves {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	scroll, err := d.store.ScrollReach(ctx, tenantID, projectID, path, device, fromDay)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusOK, heatOut{
		Path:   path,
		Device: device,
		Range:  rng,
		Views:  views,
		Clicks: clicks,
		Rage:   rage,
		Moves:  moves,
		Scroll: scroll[:],
	})
}

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
