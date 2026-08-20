// Package ai tracks per-tenant AI explain usage against the plan's monthly
// quota (plan §6.6). The count is idempotent by input hash (re-opening the
// same log selection does not deplete the quota) and cached by fingerprint (the
// same question returns the same answer without an LLM call).
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

// ErrNotConfigured: no API key is available anywhere (env, secret file, the
// instance_setting row the Settings screen writes). Explain is OFF, not
// degraded — there is no fallback brain (the heuristic was removed by owner
// decision, 2026-08-20: a canned pattern-match in the answer slot violated
// "the guess is the model's or does not exist"). Nothing is spent: the check
// runs before the cache and the quota.
var ErrNotConfigured = errors.New("ai: no API key configured")

// Input is one scenario invocation: the selected log lines plus the context
// the server already knows. MetaLine is the installer-collected project
// spec — the one stable context entry, and the only context the cache hash
// covers. Context carries the volatile half (services in window, monitors,
// open incident): it is sent to the model but excluded from the hash,
// because it changes with the room and hashing it would bust the cache on
// every incident flap (Decision 7). The cost of the split is bounded the
// other way: a cached row stops being served after one hour
// (GetCachedExplain), so an answer that quotes the unhashed context cannot
// outlive the incident it describes.
type Input struct {
	Lines    []string
	MetaLine string
	Context  []string
}

// fenceNeutralizer rewrites every prompt-fence tag that appears inside a
// part, so no stored spec value, context entry or log line can forge a
// fence boundary: after it runs, the only place a fence tag can appear is
// the framing in UserMessage itself. Square brackets keep the text legible
// to the model while not being the tag it was taught to read as a boundary.
// This — not the store-time newline strip — is the injection boundary
// (Decision 9): it is applied at render time because the strip is younger
// than the rows already in the database and never sees log lines or
// context entries at all.
var fenceNeutralizer = strings.NewReplacer(
	"</log-lines>", "[/log-lines]",
	"<log-lines>", "[log-lines]",
	"</project-spec>", "[/project-spec]",
	"<project-spec>", "[project-spec]",
	"</context>", "[/context]",
	"<context>", "[context]",
)

// UserMessage assembles the user-message content every provider sends: the
// project spec fenced in <project-spec> markers (omitted when there is
// none), the volatile server-known context fenced in <context> markers
// (omitted when there is none), then the lines fenced in <log-lines>
// markers. Every part is customer-authored somewhere down the line — the
// spec and context carry names the customer typed, the lines carry whatever
// their app logged, third-party input included — so all three regions are
// fenced and every part runs through fenceNeutralizer: a closing marker can
// only ever be written by this framing, whatever the parts contain. The
// framing lives here, beside the type that defines it, so prompts stay in
// the registry instead of being rebuilt inside each provider transport
// (Decision 4).
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

// Completion is one model answer: the raw JSON bytes exactly as the model
// produced them, the model name for the ledger, and the usage numbers the
// provider reported. A token count of -1 is the unknown sentinel — the
// stream never carried usage, which is what every gateway that ignores
// stream_options does on every call — and it is not a number: additive
// totals clamp it to 0 through Usage, the ai_call row keeps it as -1 so an
// unknown spend stays visible, and the cost line prices nothing it cannot
// count (callLogAttrs).
// On the truncation error path the Model and usage fields are populated
// even though RawJSON is nil: the provider was paid, so the spend travels
// out with the error.
type Completion struct {
	RawJSON          []byte
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Usage returns the token counts for the additive accounting paths: the SQL
// behind IncrementAIUsage and AccumulateAITokens adds these values, so the
// unknown sentinel clamps to 0 — fed in raw, it would decrement the
// tenant's monthly spend. The ledger row takes the raw fields instead
// (unknown stays -1 there, visible as unknown).
func (c Completion) Usage() (prompt, completion int64) {
	return int64(max(c.PromptTokens, 0)), int64(max(c.CompletionTokens, 0))
}

// ExplainResult carries the validated answer plus the quota counters.
// Used/Limit are the monthly call count, never tokens (Decision 8).
type ExplainResult struct {
	Answer ExplainAnswer
	Cached bool
	Used   int
	Limit  int
	// Prompt is the exact user-message bytes sent to the model (system
	// prompt lives in the registry, scenario.go). Dev observability: the
	// front echoes it to the browser console so a prompt-editing loop can
	// see what the model received without docker logs. Empty on a cache hit
	// (no call was made, nothing was sent).
	Prompt string
}

// LLM is the interface to the language model.
type LLM interface {
	Complete(ctx context.Context, sc Scenario, input Input) (Completion, error)
	// ID names the answering brain — "openai:<base>:<model>" — for the
	// cache hash: the same question gets a different answer from a different
	// brain, so a cached answer is never served across brains (Decision 7).
	// The model here is the operator-configured one, never a name streamed
	// by the provider. It takes a context because the settings (model, base
	// URL) can arrive from the database at runtime — a changed model IS a
	// changed brain, and the cache splits with it.
	ID(ctx context.Context) string
}

// ExplainAnswer is the strict triage shape every explain answer must
// match, for both scenarios: explain_logs and explain_incident
// (Decision 5).
type ExplainAnswer struct {
	Problem     string            `json:"problem"`
	Cause       string            `json:"cause"`
	Confidence  string            `json:"confidence"` // high|medium|low
	Severity    *string           `json:"severity"`
	Area        *string           `json:"area"`
	Fix         *string           `json:"fix"`
	Investigate []InvestigateStep `json:"investigate"`
}

// InvestigateStep is one ordered next step; Command is null when no runnable
// command exists.
type InvestigateStep struct {
	Step    string  `json:"step"`
	Command *string `json:"command"`
}

// ParseAnswer gates model output against the strict answer shape. It runs
// between the LLM call and every quota/ledger write, so an answer that fails
// it charges nothing (Decision 10).
func ParseAnswer(raw []byte) (ExplainAnswer, error) {
	var a ExplainAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return ExplainAnswer{}, fmt.Errorf("ai: answer is not the strict JSON shape: %w", err)
	}
	if strings.TrimSpace(a.Problem) == "" {
		return ExplainAnswer{}, errors.New("ai: answer problem is empty")
	}
	if strings.TrimSpace(a.Cause) == "" {
		return ExplainAnswer{}, errors.New("ai: answer cause is empty")
	}
	if a.Confidence != "high" && a.Confidence != "medium" && a.Confidence != "low" {
		return ExplainAnswer{}, fmt.Errorf("ai: answer confidence %q is not high, medium or low", a.Confidence)
	}
	if a.Severity != nil && *a.Severity != "critical" && *a.Severity != "major" && *a.Severity != "minor" {
		return ExplainAnswer{}, fmt.Errorf("ai: answer severity %q is not critical, major or minor", *a.Severity)
	}
	if n := len(a.Investigate); n < 1 || n > 5 {
		return ExplainAnswer{}, fmt.Errorf("ai: answer has %d investigate steps, want 1 to 5", n)
	}
	return a, nil
}

// Prices are the per-1M-token USD prices parsed once by config.Load — the
// single parser for these env vars. Zero means unset: the cost log line then
// prices the call at $0.00, which is why config refuses garbage values.
type Prices struct {
	InputPer1M  float64
	OutputPer1M float64
}

type Accountant struct {
	pool   *pg.Pool
	llm    LLM
	prices Prices
}

// BrainID names the wired brain (the LLM interface's own identity spelling —
// "openai:<base>:<model>"). The explain preview surfaces it in the browser
// console so the prompt-editing loop sees which brain answered, without the
// handler reaching into the transport.
func (a *Accountant) BrainID(ctx context.Context) string { return a.llm.ID(ctx) }

// Configured says whether the wired brain can actually answer — for the
// OpenAI client, whether a key resolves right now (env or the Settings-set
// instance_setting row). Test fakes without the method count as configured.
func (a *Accountant) Configured(ctx context.Context) bool {
	if r, ok := a.llm.(interface{ Configured(context.Context) bool }); ok {
		return r.Configured(ctx)
	}
	return true
}

func New(pool *pg.Pool, llm LLM, prices Prices) *Accountant {
	return &Accountant{pool: pool, llm: llm, prices: prices}
}

// Explain meters one explain against the plan's monthly quota. A nil limit is
// the only unlimited sentinel (a plan's ai_explains NULL); a real limit — 0
// included, a plan row that says zero AI explains — gates the call count.
func (a *Accountant) Explain(ctx context.Context, tenantID int64, sc Scenario, input Input, limit *int32) (*ExplainResult, error) {
	// No key = the feature is off, said plainly before anything is read or
	// spent. "Nothing was spent." holds trivially: no cache row, no counter.
	if !a.Configured(ctx) {
		return nil, ErrNotConfigured
	}
	q := a.pool.Queries()
	month := currentMonth()
	hash := HashInput(sc, a.llm.ID(ctx), input)

	// 1. Cache hit = no LLM call, no quota. A row that no longer parses is
	// treated as a miss; the fresh answer below overwrites it. A cache read
	// that fails (anything but no row) is logged, not swallowed.
	cached, err := q.GetCachedExplain(ctx, sqlc.GetCachedExplainParams{
		TenantID:  tenantID,
		InputHash: hash[:],
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("ai: cache read failed", "err", err, "tenant_id", tenantID, "scenario", sc.Key)
	}
	if err == nil {
		if ans, perr := ParseAnswer([]byte(cached)); perr == nil {
			// The answer is already in hand and consults no quota, so the
			// counter read may only degrade the two counters it feeds — a
			// blip serves the answer with used=0, it never turns a free
			// answer into a 500. The hard fail lives in the fast path below.
			used, uerr := usedThisMonth(ctx, q, tenantID, month)
			if uerr != nil {
				slog.Error("ai: usage read failed on cache hit", "err", uerr, "tenant_id", tenantID, "scenario", sc.Key)
			}
			return &ExplainResult{Answer: ans, Cached: true, Used: used, Limit: limitOf(limit)}, nil
		}
	}

	// 2. Quota fast path. Fail closed: an unreadable counter is returned as
	// an error, never a silent zero that grants quota. This read only spares
	// the provider call on an already-spent plan — the authoritative gate is
	// the guarded increment after the model answers (step 4). A limit of 0
	// refuses here: nothing was spent and the plan allows nothing.
	if limit != nil {
		used, uerr := usedThisMonth(ctx, q, tenantID, month)
		if uerr != nil {
			return nil, uerr
		}
		if used >= int(*limit) {
			return nil, ErrOverQuota
		}
	}

	// 3. Call the model. Nothing is charged until the output parses. A model
	// that answers with an unusable body (DeepSeek via OpenRouter
	// occasionally returns a bare {}) is retried ONCE: the empty answer is
	// recorded as spend, then a fresh call gets a second chance — two flakes
	// in a row is a provider problem the operator should see, not smooth over.
	var comp Completion
	var ans ExplainAnswer
	for attempt := 1; ; attempt++ {
		var cerr error
		comp, cerr = a.llm.Complete(ctx, sc, input)
		if cerr != nil {
			// A failed call can still have burned tokens — a stream cut at
			// max_tokens carries its usage out with the error. The spend is
			// recorded with no answer: no slot, no cache row, no retry bill.
			wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			a.recordUnansweredCall(wctx, q, tenantID, month, sc, comp, "provider_error")
			wcancel()
			return nil, cerr
		}
		var perr error
		ans, perr = ParseAnswer(comp.RawJSON)
		if perr == nil {
			break
		}
		// Console-first diagnosis: the provider answered, but the fenced JSON
		// did not parse. The burned tokens still belong in the ledger (the
		// provider bills them either way); the customer's quota stays put.
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

	// 4. Quota increment (the atomic gate), then cache + ledger row (real
	// models only). Detached from the request context on purpose: the model
	// has answered, so a client hanging up at this instant still gets billed
	// and recorded — WithoutCancel keeps the values and drops the
	// cancellation, and the timeout keeps a wedged database from pinning a
	// pool connection forever.
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
		// The guard refused the last slot to a request that raced past step
		// 2. The provider was called and paid either way, so the spend is
		// recorded — ledger row, cost line, token totals — while `used`
		// stays at the limit and the answer is NOT cached: caching it would
		// make this refusal a lie, answered by the client's own retry.
		a.recordUnansweredCall(wctx, q, tenantID, month, sc, comp, "quota_refused")
		return nil, ErrOverQuota
	}
	if uerr != nil {
		// Fail closed on the money path: a call that cannot be metered is an
		// error, not a free answer — the handler 500s and the cause is here.
		// The answer is deliberately dropped before it is ever cached: a
		// cached-but-unmetered answer would serve free on retry and never
		// count against the quota.
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
	return &ExplainResult{Answer: ans, Cached: false, Used: int(used), Limit: limitOf(limit), Prompt: input.UserMessage()}, nil
}

// limitOf is the response-side rendering of the limit: a number for metered
// plans, 0 for unlimited (the Plan page's row, not a track — usage.ts).
func limitOf(limit *int32) int {
	if limit == nil {
		return 0
	}
	return int(*limit)
}

// recordUnansweredCall keeps the money record honest when the provider was
// paid but no answer came back — the quota guard refused the last slot after
// the model answered, or the stream failed after tokens burned. The token
// totals and the ledger row are written, `used` never moves (no answer was
// delivered against the quota), and the cost line says why through
// answer_dropped. A completion carrying neither a model name nor usage (a
// connection failure, an HTTP error before any chunk) holds no spend signal
// and records nothing: that shape is the zero Completion — tokens 0, not the
// -1 sentinel — which is what every transport error path but truncation
// returns, so the guard compares <= 0.
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

// usedThisMonth reads the monthly call counter for both read sites: the quota
// fast path and the cache-hit response (which reports the real used/limit,
// not 0/0). No row yet means nothing was spent this month; any other read
// error is the caller's to fail closed on. It takes the caller's Queries so a
// transaction-scoped q reads the same counters it writes.
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

// HashInput fingerprints the input that decides the answer: the scenario key
// and version (Decision 14 — a prompt or schema bump self-invalidates the
// cache), the answering brain's ID (Decision 7 — one model name
// behind two gateways is two brains), the project-meta line, then every log
// line. The volatile Context is deliberately excluded: it is sent to the
// model but changes with the room, and hashing it would bust the cache on
// every incident flap. Every part is length-prefixed so no two different
// inputs can serialize to the same bytes — with bare separators, Lines
// ["a","b"] and ["a\nb"] collide. The brain arrives as a plain string (the
// caller passes LLM.ID()) so the fingerprint is a pure function: nothing to
// interface-assert, nothing to nil-panic on.
func HashInput(sc Scenario, brainID string, input Input) [32]byte {
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

// writeHashPart appends one part as "<byte length>:<bytes>". The declared
// length makes the framing unambiguous whatever the part contains — the
// empty string writes "0:" and still counts as a part.
func writeHashPart(h hash.Hash, s string) {
	h.Write([]byte(strconv.Itoa(len(s))))
	h.Write([]byte{':'})
	h.Write([]byte(s))
}

// callLogAttrs builds the slog attributes for the per-call cost line. An
// unknown token count (the -1 sentinel, a gateway that ignored
// stream_options) is never priced: the line says tokens=unknown instead of
// publishing a made-up number, and in particular never a negative one.
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

// callCostUSD prices one call from the per-1M prices parsed at boot
// (Decision 11); zero prices mean $0.00, never a failed call. Unknown counts
// clamp to 0 — callers that cannot count the spend say so through
// callLogAttrs rather than pricing it.
func (a *Accountant) callCostUSD(promptTokens, completionTokens int) float64 {
	return (float64(max(promptTokens, 0))*a.prices.InputPer1M + float64(max(completionTokens, 0))*a.prices.OutputPer1M) / 1e6
}

func currentMonth() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}
