// Package ai tracks per-tenant AI explain usage against the plan's monthly quota.
// The count is idempotent by input hash and cached by fingerprint.
package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

var ErrOverQuota = errors.New("ai: monthly quota exceeded")

// ErrNotConfigured means no API key resolves anywhere. Explain is OFF, not
// degraded: no fallback brain, and the check precedes cache and quota.
var ErrNotConfigured = errors.New("ai: no API key configured")

// Input is one explain invocation: lines plus server-known context. MetaLine is
// hashed; Context is volatile, sent to the model but excluded from the hash.
type Input struct {
	Lines    []string
	MetaLine string
	Context  []string
}

// fenceNeutralizer rewrites prompt-fence tags inside a part so no stored spec,
// context entry or log line can forge a fence boundary; brackets stay legible.
var fenceNeutralizer = strings.NewReplacer(
	"</log-lines>", "[/log-lines]",
	"<log-lines>", "[log-lines]",
	"</project-spec>", "[/project-spec]",
	"<project-spec>", "[project-spec]",
	"</context>", "[/context]",
	"<context>", "[context]",
)

// UserMessage assembles the fenced prompt: spec, context, lines. Every part is
// customer-authored, so all pass fenceNeutralizer; only this framing writes markers.
func (in Input) UserMessage() string {
	var b strings.Builder
	if in.MetaLine != "" {
		b.WriteString("<project-spec>\n")
		b.WriteString(fenceNeutralizer.Replace(in.MetaLine))
		b.WriteString("\n</project-spec>\n\n")
	}
	if len(in.Context) > 0 {
		b.WriteString("<context>\n")
		for _, c := range in.Context {
			b.WriteString(fenceNeutralizer.Replace(c))
			b.WriteByte('\n')
		}
		b.WriteString("</context>\n\n")
	}
	b.WriteString("<log-lines>\n")
	for _, l := range in.Lines {
		b.WriteString(fenceNeutralizer.Replace(l))
		b.WriteByte('\n')
	}
	b.WriteString("</log-lines>\n")
	return b.String()
}

// Completion is one model answer. Token count -1 is the unknown sentinel: Usage
// clamps it to 0; the ledger row keeps -1. On truncation, usage travels with the error.
type Completion struct {
	RawJSON          []byte
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Usage returns counts for the additive SQL paths; the -1 sentinel clamps to 0
// so it can never decrement the tenant's spend. The ledger takes the raw fields.
func (c Completion) Usage() (prompt, completion int64) {
	return int64(max(c.PromptTokens, 0)), int64(max(c.CompletionTokens, 0))
}

// explainResult carries the validated answer plus the quota counters.
// Used/Limit are the monthly call count, never tokens.
type explainResult struct {
	Answer explainAnswer
	Cached bool
	Used   int
	Limit  int
	// Prompt is the exact user-message bytes sent; the system prompt lives in
	// the registry. Empty on a cache hit: no call was made.
	Prompt string
}

type llm interface {
	Complete(ctx context.Context, sc Scenario, input Input) (Completion, error)
	// ID names the answering brain for the cache hash: a cached answer is
	// never served across brains. Settings can change at runtime, so it takes a ctx.
	ID(ctx context.Context) string
}

// explainAnswer is the strict triage shape every explain answer must match.
type explainAnswer struct {
	Problem     string            `json:"problem"`
	Cause       string            `json:"cause"`
	Confidence  string            `json:"confidence"` // high|medium|low
	Severity    *string           `json:"severity"`
	Area        *string           `json:"area"`
	Fix         *string           `json:"fix"`
	Investigate []investigateStep `json:"investigate"`
}

// investigateStep is one ordered next step; Command is null when no runnable
// command exists.
type investigateStep struct {
	Step    string  `json:"step"`
	Command *string `json:"command"`
}

// parseAnswer gates model output against the strict answer shape. It runs
// before every quota/ledger write, so a failing answer charges nothing.
func parseAnswer(raw []byte) (explainAnswer, error) {
	var a explainAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return explainAnswer{}, fmt.Errorf("ai: answer is not the strict JSON shape: %w", err)
	}
	if strings.TrimSpace(a.Problem) == "" {
		return explainAnswer{}, errors.New("ai: answer problem is empty")
	}
	if strings.TrimSpace(a.Cause) == "" {
		return explainAnswer{}, errors.New("ai: answer cause is empty")
	}
	if a.Confidence != "high" && a.Confidence != "medium" && a.Confidence != "low" {
		return explainAnswer{}, fmt.Errorf("ai: answer confidence %q is not high, medium or low", a.Confidence)
	}
	if a.Severity != nil && *a.Severity != "critical" && *a.Severity != "major" && *a.Severity != "minor" {
		return explainAnswer{}, fmt.Errorf("ai: answer severity %q is not critical, major or minor", *a.Severity)
	}
	if n := len(a.Investigate); n < 1 || n > 5 {
		return explainAnswer{}, fmt.Errorf("ai: answer has %d investigate steps, want 1 to 5", n)
	}
	return a, nil
}

// Prices are the per-1M-token USD prices parsed once by config.Load.
// Zero means unset: the cost line prices the call at $0.00.
type Prices struct {
	InputPer1M  float64
	OutputPer1M float64
}

type Accountant struct {
	pool   *pg.Pool
	llm    llm
	prices Prices
}

// BrainID exposes the wired brain's identity so the preview can surface it
// without the handler reaching into the transport.
func (a *Accountant) BrainID(ctx context.Context) string { return a.llm.ID(ctx) }

// Configured says whether the wired brain can answer (a resolving key, for
// OpenAI). Fakes without the method count as configured.
func (a *Accountant) Configured(ctx context.Context) bool {
	if r, ok := a.llm.(interface{ Configured(context.Context) bool }); ok {
		return r.Configured(ctx)
	}
	return true
}

func New(pool *pg.Pool, llm llm, prices Prices) *Accountant {
	return &Accountant{pool: pool, llm: llm, prices: prices}
}

// Explain meters one explain against the plan's monthly quota. A nil limit is
// the only unlimited sentinel; a real limit, 0 included, gates the call count.
func (a *Accountant) Explain(ctx context.Context, tenantID int64, sc Scenario, input Input, limit *int32) (*explainResult, error) {
	// No key = the feature is off, said plainly before anything is read or
	// spent. "Nothing was spent." holds trivially: no cache row, no counter.
	if !a.Configured(ctx) {
		return nil, ErrNotConfigured
	}
	q := a.pool.Queries()
	month := currentMonth()
	hash := hashInput(sc, a.llm.ID(ctx), input)

	// 1. Cache hit = no LLM call, no quota. A row that no longer parses is a
	// miss the fresh answer overwrites; read errors are logged, not swallowed.
	cached, err := q.GetCachedExplain(ctx, sqlc.GetCachedExplainParams{
		TenantID:  tenantID,
		InputHash: hash[:],
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("ai: cache read failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
	}
	if err == nil {
		if ans, perr := parseAnswer([]byte(cached)); perr == nil {
			// The answer consults no quota, so a counter blip serves it with
			// used=0 rather than failing a free answer into a 500.
			used, uerr := usedThisMonth(ctx, q, tenantID, month)
			if uerr != nil {
				slog.Error("ai: usage read failed on cache hit", "err", uerr, "tenant_id", tenantID, "scenario", sc.Key)
			}
			return &explainResult{Answer: ans, Cached: true, Used: used, Limit: limitOf(limit)}, nil
		}
	}

	// 2. Quota fast path, fail closed: an unreadable counter errors, never a
	// silent zero. The authoritative gate is the increment at step 4.
	if limit != nil {
		used, uerr := usedThisMonth(ctx, q, tenantID, month)
		if uerr != nil {
			return nil, uerr
		}
		if used >= int(*limit) {
			return nil, ErrOverQuota
		}
	}

	// 3. Call the model. Nothing is charged until the output parses; an
	// unusable body is retried once, two failures surface as a provider problem.
	var comp Completion
	var ans explainAnswer
	for attempt := 1; ; attempt++ {
		var cerr error
		comp, cerr = a.llm.Complete(ctx, sc, input)
		if cerr != nil {
			// A failed call can still have burned tokens; the spend is recorded
			// with no answer: no slot, no cache row, no retry bill.
			wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			a.recordUnansweredCall(wctx, q, tenantID, month, sc, comp, "provider_error")
			wcancel()
			return nil, cerr
		}
		var perr error
		ans, perr = parseAnswer(comp.RawJSON)
		if perr == nil {
			break
		}
		// The provider answered but the JSON did not parse: burned tokens go
		// to the ledger, the customer's quota stays put.
		slog.Error("ai: answer dropped, parse failed",
			"err", perr, "scenario", sc.Key, "model", comp.Model,
			"raw_bytes", len(comp.RawJSON), "attempt", attempt)
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		a.recordUnansweredCall(wctx, q, tenantID, month, sc, comp, "parse_failed")
		wcancel()
		if attempt >= 2 {
			return nil, perr
		}
	}

	// 4. Atomic quota increment, then cache + ledger row. Detached on purpose:
	// WithoutCancel bills a client that hung up; the timeout frees a wedged DB.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer wcancel()
	prompt, completion := comp.Usage()
	used, uerr := q.IncrementAIUsage(wctx, sqlc.IncrementAIUsageParams{
		TenantID:         tenantID,
		Month:            pgtype.Date{Time: month, Valid: true},
		PromptTokens:     prompt,
		CompletionTokens: completion,
		QuotaLimit:       limit,
	})
	if errors.Is(uerr, pgx.ErrNoRows) {
		// The guard refused a request that raced past step 2: the spend is
		// recorded, used stays at the limit, and the answer is not cached.
		a.recordUnansweredCall(wctx, q, tenantID, month, sc, comp, "quota_refused")
		return nil, ErrOverQuota
	}
	if uerr != nil {
		// Fail closed: an unmeterable call is an error and the answer is never
		// cached; a cached-but-unmetered answer would serve free on retry.
		slog.Error("ai: usage increment failed", "err", uerr, "tenant_id", tenantID, "scenario", sc.Key)
		return nil, uerr
	}
	if err := q.CacheExplain(wctx, sqlc.CacheExplainParams{
		TenantID:  tenantID,
		InputHash: hash[:],
		Text:      string(comp.RawJSON),
	}); err != nil {
		slog.Error("ai: cache write failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
	}
	{
		// The ledger row keeps the raw counts: -1 stays, so an unknown spend
		// is visible as unknown, never priced as free.
		if err := q.InsertAICall(wctx, sqlc.InsertAICallParams{
			TenantID:         tenantID,
			Scenario:         sc.Key,
			Model:            comp.Model,
			PromptTokens:     int64(comp.PromptTokens),
			CompletionTokens: int64(comp.CompletionTokens),
		}); err != nil {
			slog.Error("ai: ledger write failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
		}
	}
	slog.Info("ai: call", a.callLogAttrs(sc, comp)...)
	// The increment returned the post-write counter; no confirming read.
	return &explainResult{Answer: ans, Cached: false, Used: int(used), Limit: limitOf(limit), Prompt: input.UserMessage()}, nil
}

// limitOf renders the response-side limit: a number for metered plans, 0 for unlimited.
func limitOf(limit *int32) int {
	if limit == nil {
		return 0
	}
	return int(*limit)
}

// recordUnansweredCall records provider spend that returned no answer; `used`
// never moves. No model and usage <= 0 means no spend signal: record nothing.
func (a *Accountant) recordUnansweredCall(ctx context.Context, q *sqlc.Queries, tenantID int64, month time.Time, sc Scenario, comp Completion, reason string) {
	if comp.Model == "" && comp.PromptTokens <= 0 && comp.CompletionTokens <= 0 {
		return
	}
	prompt, completion := comp.Usage()
	if err := q.AccumulateAITokens(ctx, sqlc.AccumulateAITokensParams{
		TenantID:         tenantID,
		Month:            pgtype.Date{Time: month, Valid: true},
		PromptTokens:     prompt,
		CompletionTokens: completion,
	}); err != nil {
		slog.Error("ai: token accumulation for unanswered call failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
	}
	// The ledger row keeps the raw counts: -1 stays, so an unknown spend is
	// visible as unknown, never priced as free.
	if err := q.InsertAICall(ctx, sqlc.InsertAICallParams{
		TenantID:         tenantID,
		Scenario:         sc.Key,
		Model:            comp.Model,
		PromptTokens:     int64(comp.PromptTokens),
		CompletionTokens: int64(comp.CompletionTokens),
	}); err != nil {
		slog.Error("ai: ledger write for unanswered call failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
	}
	slog.Info("ai: call", append(a.callLogAttrs(sc, comp), "answer_dropped", reason)...)
}

// usedThisMonth reads the monthly counter; no row means zero spent. It takes
// the caller's Queries so a transaction reads the counters it writes.
func usedThisMonth(ctx context.Context, q *sqlc.Queries, tenantID int64, month time.Time) (int, error) {
	used, err := q.GetAIUsage(ctx, sqlc.GetAIUsageParams{
		TenantID: tenantID,
		Month:    pgtype.Date{Time: month, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(used), nil
}

// hashInput fingerprints scenario, version, brain, meta and lines; volatile
// Context is excluded. Length-prefixing keeps different inputs from colliding.
func hashInput(sc Scenario, brainID string, input Input) [32]byte {
	h := sha256.New()
	writeHashPart(h, sc.Key)
	writeHashPart(h, strconv.Itoa(sc.Version))
	writeHashPart(h, brainID)
	writeHashPart(h, input.MetaLine)
	for _, l := range input.Lines {
		writeHashPart(h, l)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// writeHashPart appends one part as "<length>:<bytes>"; the declared length
// keeps the framing unambiguous, empty string included.
func writeHashPart(h hash.Hash, s string) {
	h.Write([]byte(strconv.Itoa(len(s))))
	h.Write([]byte{':'})
	h.Write([]byte(s))
}

// callLogAttrs builds the per-call cost line. An unknown token count is never
// priced: the line says tokens=unknown instead of a made-up number.
func (a *Accountant) callLogAttrs(sc Scenario, comp Completion) []any {
	attrs := []any{"scenario", sc.Key, "model", comp.Model}
	if comp.PromptTokens < 0 || comp.CompletionTokens < 0 {
		return append(attrs, "tokens", "unknown")
	}
	return append(attrs,
		"prompt_tokens", comp.PromptTokens,
		"completion_tokens", comp.CompletionTokens,
		"cost_usd", a.callCostUSD(comp.PromptTokens, comp.CompletionTokens),
	)
}

// callCostUSD prices one call from the per-1M prices; zero prices mean $0.00,
// never a failed call. Unknown counts clamp to 0.
func (a *Accountant) callCostUSD(promptTokens, completionTokens int) float64 {
	return (float64(max(promptTokens, 0))*a.prices.InputPer1M + float64(max(completionTokens, 0))*a.prices.OutputPer1M) / 1e6
}

func currentMonth() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
