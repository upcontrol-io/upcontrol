// Package ingest is the POST /i coordinator (decode → scrub → normalize → seq
// → cardinality → batcher → receipt); sane input always gets 2xx.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"go.upcontrol.io/back/internal/ingest/cardinality"
	"go.upcontrol.io/back/internal/ingest/decode"
	"go.upcontrol.io/back/internal/ingest/normalize"
	"go.upcontrol.io/back/internal/ingest/scrub"
)

// MaxBodyBytes caps a single POST /i body before auth; oversized never reach decode.
const MaxBodyBytes = 16 << 20

// Attribute caps. Bytes, not runes: the limit is what ClickHouse stores. Over
// the cap the value is trimmed and tallied, never a reason to refuse a batch.
const (
	MaxAttrKeys     = 64
	MaxAttrKeyBytes = 256
	MaxAttrValBytes = 8192
)

// Tenant identifies the authenticated source of a batch.
type Tenant struct {
	TenantID  int64
	ProjectID int64
}

// KeyResolver turns a presented API key into a tenant+project. 401 maps to
// ErrBadKey.
type KeyResolver interface {
	Resolve(ctx context.Context, key string) (Tenant, error)
}

// SeqAllocator hands out the next sequence number for the given project. A
// lease against the wrong project stamps seq 0 and collapses ring ordering.
type SeqAllocator interface {
	Next(ctx context.Context, projectID int64) (int64, error)
}

// BatchSink is where decoded rows land (the batcher); one Add per row.
type BatchSink interface {
	Add(ctx context.Context, table string, row []byte) error
}

// Idempotency records a batch so a replay does not double-write; Claim
// returns replay=true plus the first accept's count for the receipt.
type Idempotency interface {
	Claim(ctx context.Context, batchKey string, bodyHash []byte, accepted int) (replay bool, storedAccepted int, err error)
}

// SpoolFiller reports the ingest spool's fill percentage (0–100), the
// overload steps' input.
type SpoolFiller interface {
	FillPercent(ctx context.Context) (int, error)
}

// Deps bundles the coordinator's dependencies. A nil member is a programmer
// error caught at Handle time (the handler degrades to 503 rather than panic).
type Deps struct {
	Keys  KeyResolver
	Seq   SeqAllocator
	Sink  BatchSink
	Idem  Idempotency
	Spool SpoolFiller
	Card  *cardinality.Limiter // per-request is also fine; shared is normal
	// ScrubOff disables the secret scrubber (UC_SCRUB=0, self-host only).
	// Negative on purpose: the zero value must scrub, so a Deps literal that
	// omits it redacts rather than stores tokens verbatim.
	ScrubOff bool
}

// Ingester is the POST /i handler. It is safe for concurrent use.
type Ingester struct {
	d Deps
}

// New builds an Ingester. card may be nil (cardinality capping is skipped).
func New(d Deps) *Ingester { return &Ingester{d: d} }

// Receipt is the structured acknowledgement; empty fields stay absent
// (zero is silence).
type Receipt struct {
	Accepted int        `json:"accepted"`
	Metrics  int        `json:"metrics,omitempty"` // lines routed to the metrics table, not logs
	Warnings []ReceiptW `json:"warnings,omitempty"`
	Sampling *Sampling  `json:"sampling,omitempty"`
}

// ReceiptW is one warning tally.
type ReceiptW struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// Sampling is the overload instruction: keep this fraction of level.
type Sampling struct {
	Level string  `json:"level"`
	Keep  float64 `json:"keep"`
}

// ErrBadKey is the 401 sentinel.
var ErrBadKey = errors.New("ingest: invalid or unknown api key")

// Handle is POST /i, hand-written (not generated): the openapi spec marks /i
// x-hand-written so generator validation never runs here.
func (h *Ingester) Handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil || len(body) > MaxBodyBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, receiptErr("body_too_large"))
		return
	}

	tenant, kw, err := h.authenticate(r, body)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, receiptErr("bad_key"))
		return
	}

	// Overload check: a full-enough spool refuses with 503 + Retry-After.
	fill, _ := h.d.Spool.FillPercent(ctx)
	decision := computeOverload(fill)

	// Idempotency: content-address the body. A replay returns the first receipt.
	batchKey := sha256.Sum256(body)
	hash := batchKey[:]
	// First decode to know the accepted count, then claim. On replay we short-
	// circuit with the stored count.
	dec := decode.Decode(body, r.Header.Get("Content-Type"))

	if decision.rejectAll {
		// Spool at 100%: nothing is accepted. Tell the client to retry.
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, receiptErr("spool_full"))
		return
	}

	// Build the per-row pipeline outputs, applying scrub/normalize/cardinality
	// and the overload shed mask.
	ws := newWarningAccumulator()
	if kw.inBody {
		ws.add("key_in_body", 1)
	}
	ws.merge(dec.Warnings)

	// Metric lines leave the log path entirely: a JSON line with both `metric`
	// and `value` is a reading, never a seq number or level shed.
	logRecs, metricRows := h.splitMetrics(tenant, dec.Records)
	rows := h.buildRows(ctx, tenant, logRecs, decision, ws)

	// Claim: an already-accepted body returns the stored count without
	// re-writing; accepted counts every line that landed (logs + metrics).
	if h.d.Idem != nil {
		replay, stored, err := h.d.Idem.Claim(ctx, hex.EncodeToString(hash), hash, len(rows)+len(metricRows))
		if err == nil && replay {
			writeJSON(w, http.StatusOK, Receipt{Accepted: stored})
			return
		}
	}

	// Push rows to the batcher; the sink routes by table key, so metrics land
	// in the metrics table.
	for _, row := range rows {
		_ = h.d.Sink.Add(ctx, "logs", row)
	}
	for _, row := range metricRows {
		_ = h.d.Sink.Add(ctx, "metrics", row)
	}

	rec := Receipt{Accepted: len(rows) + len(metricRows), Metrics: len(metricRows), Warnings: ws.slice()}
	if decision.sampling != nil {
		rec.Sampling = decision.sampling
	}
	writeJSON(w, http.StatusOK, rec)
}

// keyFound is the result of the four-place key search.
type keyFound struct {
	key    string
	inBody bool
}

func (h *Ingester) authenticate(r *http.Request, body []byte) (Tenant, keyFound, error) {
	var kf keyFound
	if k := r.Header.Get("X-Upcontrol-Key"); k != "" {
		kf.key = strings.TrimSpace(k)
	} else if k := r.Header.Get("Authorization"); strings.HasPrefix(k, "Bearer ") {
		kf.key = strings.TrimSpace(strings.TrimPrefix(k, "Bearer "))
	} else if k := r.URL.Query().Get("key"); k != "" {
		kf.key = k
	} else if k := keyFromBody(body); k != "" {
		kf.key = k
		kf.inBody = true
	}
	if kf.key == "" || h.d.Keys == nil {
		return Tenant{}, kf, ErrBadKey
	}
	t, err := h.d.Keys.Resolve(r.Context(), kf.key)
	if err != nil {
		return Tenant{}, kf, ErrBadKey
	}
	return t, kf, nil
}

// keyFromBody extracts a top-level "key" string field if the body is a JSON
// object, without fully decoding it (the sniffer handles the real decode).
func keyFromBody(body []byte) string {
	var probe map[string]any
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	if k, ok := probe["key"].(string); ok {
		return k
	}
	return ""
}

// RowEnvelope is the per-row payload handed to the batcher; the CH sink
// decodes it back into native column form on flush.
type RowEnvelope struct {
	TenantID    int64             `json:"tenant_id"`
	ProjectID   int64             `json:"project_id"`
	Seq         int64             `json:"seq,omitempty"`
	TS          string            `json:"ts,omitempty"`
	Level       string            `json:"level"`
	LevelRaw    string            `json:"level_raw,omitempty"`
	Service     string            `json:"service,omitempty"`
	Host        string            `json:"host,omitempty"`
	Message     string            `json:"message"`
	Fingerprint uint64            `json:"fingerprint,omitempty"`
	Attrs       map[string]string `json:"attrs,omitempty"`
	Event       string            `json:"event,omitempty"` // non-empty → wire.go also writes an events row
}

func (h *Ingester) buildRows(ctx context.Context, t Tenant, recs []decode.Record, d overloadDecision, ws *warningAccumulator) [][]byte {
	out := make([][]byte, 0, len(recs))
	for _, rec := range recs {
		// Scrub the message and attribute values (defense in depth). An operator
		// on their own box may turn this off; the hosted service may not, and
		// config refuses the switch there.
		message := rec.Message
		if !h.d.ScrubOff {
			scrubbed := scrub.Scrub(rec.Message)
			message = scrubbed.Cleaned
			if len(scrubbed.Counts) > 0 {
				ws.add("scrubbed", sumCounts(scrubbed.Counts))
			}
			for k, v := range rec.Attrs {
				if s := scrub.Scrub(v); len(s.Counts) > 0 {
					rec.Attrs[k] = s.Cleaned
					ws.add("scrubbed", sumCounts(s.Counts))
				}
			}
		}
		cappedAttrs, keysCapped, valsCapped := capAttrs(rec.Attrs)
		if keysCapped > 0 {
			ws.add("attr_key_capped", keysCapped)
		}
		if valsCapped > 0 {
			ws.add("field_cap_exceeded", valsCapped)
		}
		env := RowEnvelope{
			TenantID:    t.TenantID,
			ProjectID:   t.ProjectID,
			Message:     message,
			Fingerprint: Fingerprint(message),
			Level:       rec.Level,
			LevelRaw:    rec.LevelRaw,
			Service:     rec.Service,
			Host:        rec.Host,
			Attrs:       cappedAttrs,
		}
		if !rec.Time.IsZero() {
			env.TS = rec.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		}
		// A message that names an event something queries is also stored as an
		// event row; the reserved uc.* prefix warns and stores no name.
		if ev := normalize.Classify(rec.Message); ev.Reserved {
			ws.add("reserved_prefix", 1)
		} else if ev.Name != "" {
			env.Event = ev.Name
		}
		// Cardinality cap on host/service (a runaway field would blow up the CH
		// LowCardinality dictionaries).
		if h.d.Card != nil {
			if env.Host, _ = h.d.Card.Add("host", env.Host); env.Host == cardinality.Sentinel {
				ws.add("cardinality_capped", 1)
			}
			env.Service, _ = h.d.Card.Add("service", env.Service)
		}
		// Overload shed mask: drop rows below the kept level.
		if d.shed[env.Level] {
			ws.add("class_shed", 1)
			continue
		}
		// Seq allocation: one per accepted row, carried on the envelope to CH.
		if h.d.Seq != nil {
			if seq, err := h.d.Seq.Next(ctx, t.ProjectID); err == nil {
				env.Seq = seq
			}
		}
		b, _ := json.Marshal(env)
		out = append(out, b)
	}
	return out
}

// capAttrs bounds a record's attributes and reports what it trimmed. Keys are
// kept in sorted order so the same record always keeps the same 64: Go map
// iteration is randomised, and a non-deterministic cap would make an
// idempotency replay disagree with the original write.
func capAttrs(attrs map[string]string) (out map[string]string, keysCapped, valsCapped int) {
	if len(attrs) == 0 {
		return attrs, 0, 0
	}
	over := len(attrs) > MaxAttrKeys
	if !over {
		for k, v := range attrs {
			if len(k) > MaxAttrKeyBytes || len(v) > MaxAttrValBytes {
				over = true
				break
			}
		}
	}
	if !over {
		return attrs, 0, 0
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out = make(map[string]string, len(keys))
	for i, k := range keys {
		if i >= MaxAttrKeys {
			keysCapped++
			continue
		}
		if len(k) > MaxAttrKeyBytes {
			k = k[:MaxAttrKeyBytes]
			keysCapped++
		}
		v := attrs[keys[i]]
		if len(v) > MaxAttrValBytes {
			v = v[:MaxAttrValBytes]
			valsCapped++
		}
		out[k] = v
	}
	return out, keysCapped, valsCapped
}

// overloadDecision is the stepped response to spool fill.
type overloadDecision struct {
	shed      map[string]bool // levels to drop
	sampling  *Sampling       // instruction returned in the receipt
	rejectAll bool            // spool at 100%: refuse entirely
}

// computeOverload maps a fill percentage to the 60/75/90/100% steps; the
// sampling field tells the client what fraction to keep going forward.
func computeOverload(fillPct int) overloadDecision {
	d := overloadDecision{shed: map[string]bool{}}
	switch {
	case fillPct >= 100:
		d.rejectAll = true
	case fillPct >= 90:
		d.shed["debug"] = true
		d.shed["info"] = true
		d.shed["warn"] = true
		d.sampling = &Sampling{Level: "warn", Keep: 0.0}
	case fillPct >= 75:
		d.shed["debug"] = true
		d.shed["info"] = true
		d.sampling = &Sampling{Level: "info", Keep: 0.1}
	case fillPct >= 60:
		d.shed["debug"] = true
		d.sampling = &Sampling{Level: "debug", Keep: 0.1}
	}
	return d
}

type warningAccumulator struct {
	counts map[string]int
}

func newWarningAccumulator() *warningAccumulator {
	return &warningAccumulator{counts: map[string]int{}}
}

func (a *warningAccumulator) add(code string, n int) { a.counts[code] += n }

func (a *warningAccumulator) merge(ws []decode.Warning) {
	for _, w := range ws {
		a.counts[string(w.Code)] += w.Count
	}
}

func (a *warningAccumulator) slice() []ReceiptW {
	if len(a.counts) == 0 {
		return nil
	}
	out := make([]ReceiptW, 0, len(a.counts))
	for c, n := range a.counts {
		out = append(out, ReceiptW{Code: c, Count: n})
	}
	return out
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func receiptErr(code string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code}}
}
