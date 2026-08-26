//go:build integration

// The Accountant's money paths against a real Postgres: cache, quota, ledger.
// Run: UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/ai/...
package ai

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// stubLLM stands in for the provider: it counts calls (a cache hit must not
// reach it) and returns one scripted completion.
type stubLLM struct {
	calls int
	comp  Completion
}

func (s *stubLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	s.calls++
	return s.comp, nil
}

func (s *stubLLM) ID(context.Context) string { return "stub" }

// fakeLLM is a canned-JSON test double with a fixed brain identity, driving
// the Accountant's production path (cache, quota, ledger) without a network.
type fakeLLM struct {
	id  string
	raw string
}

// fakeAnswerJSON passes parseAnswer's strict gate: non-empty problem and
// cause, a legal confidence, one investigate step.
const fakeAnswerJSON = `{"problem":"test problem","cause":"test cause","confidence":"low","investigate":[{"step":"look"}]}`

func (f fakeLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	return Completion{RawJSON: []byte(f.raw), Model: f.id}, nil
}
func (f fakeLLM) ID(context.Context) string { return f.id }

const validAnswer = `{"problem":"the dependency refused the connection","cause":"the downstream is down or not listening on the expected port","confidence":"medium","fix":null,"investigate":[{"step":"Check the dependency is up and that the host:port in config match it.","command":null}]}`

func gptCompletion() Completion {
	return Completion{
		RawJSON:          []byte(validAnswer),
		Model:            "gpt-test",
		PromptTokens:     120,
		CompletionTokens: 45,
	}
}

// limitPtr is the test-side spelling of a metered plan (nil means unlimited).
func limitPtr(n int32) *int32 { return &n }

// openAccountantDB applies migrations and returns a pool plus a fresh tenant,
// so every test's usage/ledger reads see only its own rows.
func openAccountantDB(t *testing.T) (*pg.Pool, int64) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping accountant integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("ai-acct-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return pool, tenantID
}

func TestExplain_QuotaCacheAndLedger(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	usage := func() (used, prompt, completion int64) {
		if err := pool.Raw().QueryRow(ctx,
			`SELECT used, prompt_tokens, completion_tokens FROM ai_usage WHERE tenant_id = $1`,
			tenantID).Scan(&used, &prompt, &completion); err != nil {
			t.Fatalf("read ai_usage: %v", err)
		}
		return
	}
	ledger := func() (rows, prompt, completion int64) {
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*), coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0)
			 FROM ai_call WHERE tenant_id = $1`,
			tenantID).Scan(&rows, &prompt, &completion); err != nil {
			t.Fatalf("read ai_call: %v", err)
		}
		return
	}
	model := &stubLLM{comp: gptCompletion()}
	acct := New(pool, model, Prices{})
	cachedRows := func(input Input) int64 {
		var n int64
		h := hashInput(ExplainLogs, model.ID(ctx), input)
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM ai_explain_cache WHERE tenant_id = $1 AND input_hash = $2`,
			tenantID, h[:]).Scan(&n); err != nil {
			t.Fatalf("read ai_explain_cache: %v", err)
		}
		return n
	}

	first := Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}

	// 1. A real call: one ledger row, tokens land in ai_usage, answer cached.
	res, err := acct.Explain(ctx, tenantID, ExplainLogs, first, limitPtr(5))
	if err != nil {
		t.Fatalf("first explain: %v", err)
	}
	if res.Cached {
		t.Fatal("a fresh input must not be answered from the cache")
	}
	if res.Used != 1 || res.Limit != 5 {
		t.Fatalf("result counters = %d/%d, want 1/5", res.Used, res.Limit)
	}
	if used, prompt, completion := usage(); used != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_usage after a real call = %d calls, %d/%d tokens, want 1 call, 120/45 — IncrementAIUsage must carry the provider's tokens", used, prompt, completion)
	}
	if rows, prompt, completion := ledger(); rows != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_call after a real call = %d rows, %d/%d tokens, want exactly one row, 120/45", rows, prompt, completion)
	}
	if n := cachedRows(first); n != 1 {
		t.Fatalf("cache rows for the input = %d, want 1", n)
	}

	// 2. A cache hit charges nothing: a limit of 1 the gate would refuse
	// passes only because the cache sits in front; counters stay real.
	res, err = acct.Explain(ctx, tenantID, ExplainLogs, first, limitPtr(1))
	if err != nil {
		t.Fatalf("cached explain: %v", err)
	}
	if !res.Cached {
		t.Fatal("the same input must be answered from the cache")
	}
	if res.Used != 1 || res.Limit != 1 {
		t.Fatalf("cache-hit counters = %d/%d, want the real 1/1 — a cached answer still reports what the plan spent", res.Used, res.Limit)
	}
	if model.calls != 1 {
		t.Fatalf("the model was called %d times, want 1 — a cache hit must not reach the provider", model.calls)
	}
	if used, prompt, completion := usage(); used != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_usage after a cache hit = %d calls, %d/%d tokens, want unchanged 1/120/45", used, prompt, completion)
	}
	if rows, prompt, completion := ledger(); rows != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_call after a cache hit = %d rows, %d/%d tokens, want unchanged 1/120/45", rows, prompt, completion)
	}

	// 3. Invalid model JSON retries once, then fails: spend lands in the
	// ledger, `used` never moves, nothing is cached.
	broken := Input{Lines: []string{"write: no space left on device"}}
	model.comp = Completion{RawJSON: []byte(`{"problem": "trunc`), Model: "gpt-test"}
	if _, err := acct.Explain(ctx, tenantID, ExplainLogs, broken, limitPtr(5)); err == nil {
		t.Fatal("an answer that fails parseAnswer twice must fail the explain")
	}
	if model.calls != 3 {
		t.Fatalf("the model was called %d times, want 3 — one real call plus two attempts on the broken answer", model.calls)
	}
	if used, prompt, completion := usage(); used != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_usage after invalid model JSON = %d calls, %d/%d tokens, want unchanged 1/120/45 — the quota never moves on a dropped answer", used, prompt, completion)
	}
	if rows, prompt, completion := ledger(); rows != 3 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_call after invalid model JSON = %d rows, %d/%d tokens, want 3 rows (one real, two parse_failed at 0/0) with unchanged 120/45 — the provider bills the burned tokens either way", rows, prompt, completion)
	}
	if n := cachedRows(broken); n != 0 {
		t.Fatalf("cache rows for the broken input = %d, want 0", n)
	}

	// 4. A second brain is a different cache identity; its zero-token row
	// still lands in the ledger.
	fake := fakeLLM{id: "fake", raw: fakeAnswerJSON}
	res, err = New(pool, fake, Prices{}).Explain(ctx, tenantID, ExplainLogs,
		Input{Lines: []string{"FATAL: out of memory (oom-kill)"}}, limitPtr(5))
	if err != nil {
		t.Fatalf("fake-brain explain: %v", err)
	}
	if res.Used != 2 {
		t.Fatalf("used after the fake-brain call = %d, want 2 — the count quota applies to every brain", res.Used)
	}
	if used, prompt, completion := usage(); used != 2 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_usage after the fake-brain call = %d calls, %d/%d tokens, want 2 calls with unchanged 120/45", used, prompt, completion)
	}
	if rows, prompt, completion := ledger(); rows != 4 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_call after the fake-brain call = %d rows, %d/%d tokens, want 4 rows (its 0/0 row lands) with unchanged 120/45", rows, prompt, completion)
	}

	// 5. An unlimited plan (nil limit) skips the guard: the count still moves
	// (it feeds the Plan page), the gate never refuses.
	res, err = New(pool, fake, Prices{}).Explain(ctx, tenantID, ExplainLogs,
		Input{Lines: []string{"context deadline exceeded (Client.Timeout)"}}, nil)
	if err != nil {
		t.Fatalf("unlimited explain: %v", err)
	}
	if res.Used != 3 || res.Limit != 0 {
		t.Fatalf("unlimited-plan counters = %d/%d, want 3/0", res.Used, res.Limit)
	}
}

// barrierLLM holds every caller inside Complete until both racers arrive, so
// the outcome is decided by the guard, not goroutine scheduling.
type barrierLLM struct {
	gate *sync.WaitGroup
	comp Completion
}

func (barrierLLM) ID(context.Context) string { return "barrier" }

func (b barrierLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	b.gate.Done()
	b.gate.Wait()
	return b.comp, nil
}

// Two explains racing at used = limit-1 take exactly one slot: one answer,
// one 402. The loser's paid call is booked, not cached, and its retry refuses.
func TestExplain_LastSlotRace(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	// used = limit-1: one call already made against a limit of 2.
	if _, err := New(pool, &stubLLM{comp: gptCompletion()}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}, limitPtr(2)); err != nil {
		t.Fatalf("warm-up explain: %v", err)
	}

	// Two explains over different lines (the cache deduplicates identical
	// inputs), held at the model call until both pass the fast path.
	inputs := []Input{
		{Lines: []string{"worker 0: write: no space left on device"}},
		{Lines: []string{"worker 1: write: no space left on device"}},
	}
	gate := &sync.WaitGroup{}
	gate.Add(2)
	racer := barrierLLM{gate: gate, comp: gptCompletion()}
	type outcome struct {
		idx int
		res *explainResult
		err error
	}
	out := make(chan outcome, 2)
	for i, in := range inputs {
		go func(i int, in Input) {
			res, err := New(pool, racer, Prices{}).Explain(ctx, tenantID, ExplainLogs, in, limitPtr(2))
			out <- outcome{idx: i, res: res, err: err}
		}(i, in)
	}
	answered, refused, refusedIdx := 0, 0, -1
	for i := 0; i < 2; i++ {
		oc := <-out
		switch {
		case oc.err == nil:
			answered++
			if oc.res.Used != 2 {
				t.Errorf("the winner reports used = %d, want 2", oc.res.Used)
			}
		case errors.Is(oc.err, ErrOverQuota):
			refused++
			refusedIdx = oc.idx
		default:
			t.Errorf("unexpected error: %v", oc.err)
		}
	}
	if answered != 1 || refused != 1 {
		t.Fatalf("race at the last slot: %d answered, %d refused — want exactly one 200 and one 402 (the increment must be atomic)", answered, refused)
	}

	// The loser reached the accounting, so its spend is booked: warm-up +
	// winner + loser tokens, one ledger row each.
	var used, prompt, completion int64
	var ledgerRows int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT used, prompt_tokens, completion_tokens FROM ai_usage WHERE tenant_id = $1`,
		tenantID).Scan(&used, &prompt, &completion); err != nil {
		t.Fatalf("read ai_usage: %v", err)
	}
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM ai_call WHERE tenant_id = $1`, tenantID).Scan(&ledgerRows); err != nil {
		t.Fatalf("read ai_call: %v", err)
	}
	if used != 2 || prompt != 360 || completion != 135 || ledgerRows != 3 {
		t.Fatalf("after the race: ai_usage = %d calls %d/%d tokens, ai_call rows = %d — want 2 calls, 360/135 tokens (the loser's paid call is recorded), 3 rows", used, prompt, completion, ledgerRows)
	}

	// The winner's answer is cached; the loser's is not — caching it would
	// make the 402 a lie one retry later.
	for i, in := range inputs {
		var n int64
		want := int64(1)
		if i == refusedIdx {
			want = 0
		}
		h := hashInput(ExplainLogs, racer.ID(ctx), in)
		if err := pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM ai_explain_cache WHERE tenant_id = $1 AND input_hash = $2`,
			tenantID, h[:]).Scan(&n); err != nil {
			t.Fatalf("read ai_explain_cache: %v", err)
		}
		if n != want {
			t.Fatalf("cache rows for racer %d = %d, want %d — a refused answer must not be cached (its retry would be a free 200)", i, n, want)
		}
	}

	// The loser's own retry is refused again, before the provider: the 402
	// is not contradicted by the request it told to come back later.
	retry := &stubLLM{comp: gptCompletion()}
	if _, err := New(pool, retry, Prices{}).Explain(ctx, tenantID, ExplainLogs, inputs[refusedIdx], limitPtr(2)); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("retry after refusal: err = %v, want ErrOverQuota", err)
	}
	if retry.calls != 0 {
		t.Fatalf("the retry reached the model %d times, want 0 — an over-quota retry is refused before the provider call", retry.calls)
	}
}

// cancellingLLM cancels the request context before answering: the accounting
// writes must still land.
type cancellingLLM struct{ cancel context.CancelFunc }

func (cancellingLLM) ID(context.Context) string { return "cancelling" }

func (c cancellingLLM) Complete(context.Context, Scenario, Input) (Completion, error) {
	c.cancel()
	return gptCompletion(), nil
}

func TestExplain_AccountingSurvivesClientDisconnect(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := New(pool, cancellingLLM{cancel: cancel}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"pool exhausted: too many connections"}}, limitPtr(5)); err != nil {
		t.Fatalf("explain after a mid-call disconnect: %v", err)
	}
	// The verifying reads need a live context — ctx is cancelled by design.
	rctx := context.Background()
	var used, prompt, completion, ledgerRows int64
	if err := pool.Raw().QueryRow(rctx,
		`SELECT used, prompt_tokens, completion_tokens FROM ai_usage WHERE tenant_id = $1`,
		tenantID).Scan(&used, &prompt, &completion); err != nil {
		t.Fatalf("read ai_usage: %v", err)
	}
	if err := pool.Raw().QueryRow(rctx,
		`SELECT count(*) FROM ai_call WHERE tenant_id = $1`, tenantID).Scan(&ledgerRows); err != nil {
		t.Fatalf("read ai_call: %v", err)
	}
	// ctx is cancelled; the writes ran on context.WithoutCancel and landed.
	if used != 1 || prompt != 120 || completion != 45 || ledgerRows != 1 {
		t.Fatalf("after a client disconnect: ai_usage = %d calls %d/%d tokens, ai_call rows = %d — want 1, 120/45, 1 (a answered model call still bills and records)", used, prompt, completion, ledgerRows)
	}
}

// A quota counter that cannot be read fails the explain; it must never
// silently grant quota.
func TestExplain_QuotaReadFailsClosed(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	pool.Close() // the request's DB dies before the quota read; pgxpool.Close is idempotent for t.Cleanup
	model := &stubLLM{comp: gptCompletion()}
	_, err := New(pool, model, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"401 unauthorized: invalid token"}}, limitPtr(5))
	if err == nil {
		t.Fatal("an unreadable quota counter must fail the explain, not grant it")
	}
	if errors.Is(err, ErrOverQuota) {
		t.Fatalf("a read failure is a 500-path error, not a quota refusal: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("the model was called %d times, want 0 — the gate refuses before the provider spend", model.calls)
	}
}

// poolKillingLLM closes the pool before answering: the metering writes then
// run against a dead database.
type poolKillingLLM struct{ pool *pg.Pool }

func (poolKillingLLM) ID(context.Context) string { return "pool-killing" }

func (p poolKillingLLM) Complete(context.Context, Scenario, Input) (Completion, error) {
	p.pool.Close()
	return gptCompletion(), nil
}

// An increment that fails after the model answered fails closed: nothing
// lands and the answer is not cached.
func TestExplain_IncrementFailsAfterAnswer(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	// A second pool to the same database: the first dies mid-call, the
	// verification reads must not.
	verify, err := pg.Open(context.Background(), os.Getenv("UC_TEST_POSTGRES"))
	if err != nil {
		t.Fatalf("open verification pool: %v", err)
	}
	t.Cleanup(verify.Close)
	ctx := context.Background()

	in := Input{Lines: []string{"connection refused: postgres is gone"}}
	model := poolKillingLLM{pool: pool}
	_, err = New(pool, model, Prices{}).Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5))
	if err == nil {
		t.Fatal("an increment that fails after the model answered must fail the explain, not answer for free")
	}
	if errors.Is(err, ErrOverQuota) {
		t.Fatalf("a dead database is a 500-path error, not a quota refusal: %v", err)
	}

	h := hashInput(ExplainLogs, model.ID(ctx), in)
	var usageRows, ledgerRows, cacheRows int64
	if err := verify.Raw().QueryRow(ctx,
		`SELECT (SELECT count(*) FROM ai_usage WHERE tenant_id = $1),
		        (SELECT count(*) FROM ai_call WHERE tenant_id = $1),
		        (SELECT count(*) FROM ai_explain_cache WHERE tenant_id = $1 AND input_hash = $2)`,
		tenantID, h[:]).Scan(&usageRows, &ledgerRows, &cacheRows); err != nil {
		t.Fatalf("verify accounting: %v", err)
	}
	if usageRows != 0 || ledgerRows != 0 || cacheRows != 0 {
		t.Fatalf("after a failed increment: usage rows = %d, ledger rows = %d, cache rows = %d — want 0/0/0, an unmeterable answer is discarded whole", usageRows, ledgerRows, cacheRows)
	}
}

// failingLLM mimics a stream cut at max_tokens: model and usage carried out
// with the error, no answer bytes.
type failingLLM struct{}

func (failingLLM) ID(context.Context) string { return "failing" }

func (failingLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	return Completion{Model: "gpt-test", PromptTokens: 900, CompletionTokens: 400},
		errors.New(`ai: provider finish_reason "length", answer incomplete`)
}

// A stream cut at max_tokens still burns tokens: ledger row and token totals
// written, no answer, no slot, no cache row.
func TestExplain_FailedProviderCallStillRecordsSpend(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	in := Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}
	model := failingLLM{}
	if _, err := New(pool, model, Prices{}).Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err == nil {
		t.Fatal("a failed provider call must fail the explain")
	}

	var used, prompt, completion int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT used, prompt_tokens, completion_tokens FROM ai_usage WHERE tenant_id = $1`,
		tenantID).Scan(&used, &prompt, &completion); err != nil {
		t.Fatalf("read ai_usage: %v", err)
	}
	if used != 0 || prompt != 900 || completion != 400 {
		t.Fatalf("ai_usage = %d calls %d/%d tokens, want 0 calls (no answer was delivered) with the burned 900/400", used, prompt, completion)
	}
	var ledgerRows int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM ai_call WHERE tenant_id = $1`, tenantID).Scan(&ledgerRows); err != nil {
		t.Fatalf("read ai_call: %v", err)
	}
	if ledgerRows != 1 {
		t.Fatalf("ai_call rows = %d, want 1 — the paid call must be on the books even when it failed", ledgerRows)
	}
	h := hashInput(ExplainLogs, model.ID(ctx), in)
	var cacheRows int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM ai_explain_cache WHERE tenant_id = $1 AND input_hash = $2`,
		tenantID, h[:]).Scan(&cacheRows); err != nil {
		t.Fatalf("read ai_explain_cache: %v", err)
	}
	if cacheRows != 0 {
		t.Fatal("a failed call must not be cached — there is no answer to cache")
	}
}

// deadLLM mimics a call that never reached a chunk: the zero Completion,
// token fields 0, not the -1 sentinel.
type deadLLM struct{}

func (deadLLM) ID(context.Context) string { return "dead" }

func (deadLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	return Completion{}, errors.New("ai: provider request: dial tcp: connection refused")
}

// A call that never reached a chunk records nothing: no $0 ledger row, no
// ai_usage row. The guard compares <= 0 because the zero Completion carries 0s.
func TestExplain_ConnectionFailureRecordsNothing(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	if _, err := New(pool, deadLLM{}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}, limitPtr(5)); err == nil {
		t.Fatal("a connection-refused provider must fail the explain")
	}

	var usageRows, ledgerRows int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT (SELECT count(*) FROM ai_usage WHERE tenant_id = $1),
		        (SELECT count(*) FROM ai_call WHERE tenant_id = $1)`,
		tenantID).Scan(&usageRows, &ledgerRows); err != nil {
		t.Fatalf("verify accounting: %v", err)
	}
	if usageRows != 0 || ledgerRows != 0 {
		t.Fatalf("a call that never reached a chunk: usage rows = %d, ledger rows = %d — want 0/0, nothing was spent", usageRows, ledgerRows)
	}
}

// A gateway that never sends usage leaves the additive totals at zero, while
// the ledger row keeps -1/-1 so the unknown stays visible.
func TestExplain_UnknownTokensNeverDecrementTheTotals(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	unknown := gptCompletion()
	unknown.PromptTokens, unknown.CompletionTokens = -1, -1
	if _, err := New(pool, &stubLLM{comp: unknown}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}, limitPtr(5)); err != nil {
		t.Fatalf("explain with unknown usage: %v", err)
	}

	var used, prompt, completion int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT used, prompt_tokens, completion_tokens FROM ai_usage WHERE tenant_id = $1`,
		tenantID).Scan(&used, &prompt, &completion); err != nil {
		t.Fatalf("read ai_usage: %v", err)
	}
	if used != 1 || prompt != 0 || completion != 0 {
		t.Fatalf("ai_usage = %d calls %d/%d tokens, want 1 call with 0/0 — the sentinel must never reach the additive SQL", used, prompt, completion)
	}
	var ledgerRows, ledgerPrompt, ledgerCompletion int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*), coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0)
		 FROM ai_call WHERE tenant_id = $1`, tenantID).Scan(&ledgerRows, &ledgerPrompt, &ledgerCompletion); err != nil {
		t.Fatalf("read ai_call: %v", err)
	}
	if ledgerRows != 1 || ledgerPrompt != -1 || ledgerCompletion != -1 {
		t.Fatalf("ai_call = %d rows %d/%d tokens, want 1 row carrying the visible unknown -1/-1", ledgerRows, ledgerPrompt, ledgerCompletion)
	}
}

// The provider-streamed model string must never steer a gate: a stream
// naming itself anything still spent real tokens and still writes its row.
func TestExplain_StreamedModelNameDoesNotSteerTheLedger(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	impersonator := gptCompletion()
	impersonator.Model = "heuristic"
	if _, err := New(pool, &stubLLM{comp: impersonator}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}, limitPtr(5)); err != nil {
		t.Fatalf("explain: %v", err)
	}

	var ledgerRows, prompt, completion int64
	if err := pool.Raw().QueryRow(ctx,
		`SELECT count(*), coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0)
		 FROM ai_call WHERE tenant_id = $1`, tenantID).Scan(&ledgerRows, &prompt, &completion); err != nil {
		t.Fatalf("read ai_call: %v", err)
	}
	if ledgerRows != 1 || prompt != 120 || completion != 45 {
		t.Fatalf("ai_call = %d rows %d/%d tokens, want 1 row 120/45 — the model string must not steer the ledger gate", ledgerRows, prompt, completion)
	}
}

// Volatile context sits outside the hash, so a cached row stops serving after
// an hour; the fresh answer re-stamps the row on overwrite.
func TestExplain_CacheRowExpiresAfterAnHour(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	model := &stubLLM{comp: gptCompletion()}
	acct := New(pool, model, Prices{})
	in := Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}
	if _, err := acct.Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err != nil {
		t.Fatalf("first explain: %v", err)
	}
	if res, err := acct.Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err != nil || !res.Cached {
		t.Fatalf("a fresh row must serve: cached = %v, err = %v", res.Cached, err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls after the cached hit = %d, want 1", model.calls)
	}

	// Age the row past the hour: the same input must now be a miss that
	// re-calls the model.
	if _, err := pool.Raw().Exec(ctx,
		`UPDATE ai_explain_cache SET created_at = now() - interval '2 hours' WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatalf("age the cache row: %v", err)
	}
	if res, err := acct.Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err != nil || res.Cached {
		t.Fatalf("an expired row must be a miss: cached = %v, err = %v", res.Cached, err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls after expiry = %d, want 2 — the provider must be re-consulted", model.calls)
	}

	// The upsert must re-stamp created_at, or the fresh answer would itself
	// already be expired.
	if res, err := acct.Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err != nil || !res.Cached {
		t.Fatalf("the re-written row must serve again: cached = %v, err = %v — the upsert must refresh created_at", res.Cached, err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls after the re-cached hit = %d, want 2", model.calls)
	}
}
