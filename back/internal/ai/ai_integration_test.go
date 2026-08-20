//go:build integration

// The Accountant's money paths against a real Postgres: a real call writes
// one ai_call row and adds its tokens to ai_usage (Decision 9 + Review 1's
// dead-columns guard), a cache hit charges nothing and never re-calls the
// model, an answer that fails ParseAnswer twice still lands its burned
// tokens in the ledger while the quota's used count never moves, and every
// brain's parsed answer writes its ledger row — zero-token ones included.
// The quota itself is atomic (Decision 4): a race for the last slot answers
// exactly one request — while still recording the loser's paid provider call
// (Review 23) — the accounting survives a client disconnect after the model
// answered (Decision 5), and an unreadable counter fails closed instead of
// granting quota (Decision 3).
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/ai/...
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
		h := HashInput(ExplainLogs, model.ID(ctx), input)
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

	// 2. A cache hit charges nothing: same input, and a limit of 1 that the
	// quota gate would refuse — only the cache check sitting in front of it
	// can let this through. The model must not be called again, and the
	// counters are the real ones, not 0/0 (Decision 6).
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

	// 3. Invalid model JSON retries once, then fails: the provider is called
	// twice for this input (plus section 1's call = 3 total), each attempt's
	// spend goes to the ledger with its raw token counts, but the quota's
	// `used` never moves and nothing is cached (no answer was delivered).
	broken := Input{Lines: []string{"write: no space left on device"}}
	model.comp = Completion{RawJSON: []byte(`{"problem": "trunc`), Model: "gpt-test"}
	if _, err := acct.Explain(ctx, tenantID, ExplainLogs, broken, limitPtr(5)); err == nil {
		t.Fatal("an answer that fails ParseAnswer twice must fail the explain")
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

	// 4. A second brain (different ID) is a different cache identity, its
	// call bumps the count quota, and every parsed answer writes its ledger
	// row — zero-token brains included, so an unknown-usage gateway is still
	// visible in the ledger.
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

// barrierLLM holds every caller inside Complete until both racers have
// arrived, then releases them together: both are past the quota fast path
// (step 2) before either reaches the guarded increment, so the outcome is
// decided by the guard the race test exists for, not by goroutine scheduling.
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

// TestExplain_LastSlotRace pins the atomic gate (Decision 4): two explains
// racing at used = limit-1 take exactly one slot — one answer, one 402 — the
// counter never overshoots the limit, the loser's paid provider call is still
// on the books (tokens and ledger; Review 23), its answer is NOT cached, and
// its own retry is refused again rather than answered for free.
func TestExplain_LastSlotRace(t *testing.T) {
	pool, tenantID := openAccountantDB(t)
	ctx := context.Background()

	// used = limit-1: one call already made against a limit of 2.
	if _, err := New(pool, &stubLLM{comp: gptCompletion()}, Prices{}).Explain(ctx, tenantID,
		ExplainLogs, Input{Lines: []string{"connect ECONNREFUSED 10.0.0.1:5432"}}, limitPtr(2)); err != nil {
		t.Fatalf("warm-up explain: %v", err)
	}

	// Two explains over different lines (the cache deduplicates identical
	// inputs, so the gate is the only thing that can separate them) racing
	// for the last slot, held at the model call until both are through the
	// fast path.
	inputs := []Input{
		{Lines: []string{"worker 0: write: no space left on device"}},
		{Lines: []string{"worker 1: write: no space left on device"}},
	}
	gate := &sync.WaitGroup{}
	gate.Add(2)
	racer := barrierLLM{gate: gate, comp: gptCompletion()}
	type outcome struct {
		idx int
		res *ExplainResult
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

	// The loser reached step 4 (past the model, into the accounting) — the
	// barrier guarantees it — so its spend must be on the books even though
	// its answer is not: warm-up + winner + loser tokens in ai_usage, one
	// ledger row each. A loser refused at the fast path would leave 240/90
	// and 2 rows; this is what tells the two refusals apart.
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
		h := HashInput(ExplainLogs, racer.ID(ctx), in)
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

// cancellingLLM answers validly but cancels the request context first: the
// model has answered, so the accounting writes must still land (Decision 5).
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

// TestExplain_QuotaReadFailsClosed pins Decision 3: a quota counter that
// cannot be read fails the explain — it must never silently grant quota.
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

// poolKillingLLM answers validly but closes the pool first: the model has
// answered, and the metering writes are about to run against a dead database.
type poolKillingLLM struct{ pool *pg.Pool }

func (poolKillingLLM) ID(context.Context) string { return "pool-killing" }

func (p poolKillingLLM) Complete(context.Context, Scenario, Input) (Completion, error) {
	p.pool.Close()
	return gptCompletion(), nil
}

// TestExplain_IncrementFailsAfterAnswer pins the branch where the increment
// fails for a real reason (not the guard) after the model answered: the call
// fails closed — a paid-for answer that cannot be metered is an error, not a
// free one — and nothing lands. In particular the answer is not cached: a
// cached-but-unmetered answer would serve free on retry and never count
// against the quota (Review 23).
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

	h := HashInput(ExplainLogs, model.ID(ctx), in)
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

// failingLLM mimics a provider call that burned tokens and then failed — the
// shape Complete returns for a stream cut at max_tokens: model and usage
// carried out with the error, no answer bytes.
type failingLLM struct{}

func (failingLLM) ID(context.Context) string { return "failing" }

func (failingLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	return Completion{Model: "gpt-test", PromptTokens: 900, CompletionTokens: 400},
		errors.New(`ai: provider finish_reason "length", answer incomplete`)
}

// TestExplain_FailedProviderCallStillRecordsSpend pins Review 25's truncation
// finding: a stream cut at max_tokens burns a full prompt plus the output
// cap, so the ledger row, the token totals and the cost line are written
// even though nothing comes back — no answer, no slot, no cache row.
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
	h := HashInput(ExplainLogs, model.ID(ctx), in)
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

// deadLLM mimics a provider call that never reached a chunk — the shape every
// openai.go error path but truncation returns: the zero Completion, whose
// token fields are 0, not the -1 sentinel (a connection refusal, a non-2xx, a
// contentless stream, the garbage caps).
type deadLLM struct{}

func (deadLLM) ID(context.Context) string { return "dead" }

func (deadLLM) Complete(_ context.Context, _ Scenario, _ Input) (Completion, error) {
	return Completion{}, errors.New("ai: provider request: dial tcp: connection refused")
}

// TestExplain_ConnectionFailureRecordsNothing pins the phantom-row guard: a
// call that never reached a chunk holds no spend signal, so a down gateway
// must record nothing — no $0 ledger row, no ai_usage row for a month with no
// usage. The guard compares <= 0 precisely because the zero Completion carries
// 0s; testing for the -1 sentinel (as an earlier version did) never fired and
// a dead provider grew one phantom row per attempt, up to the throttle's
// 6/min/tenant.
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

// TestExplain_UnknownTokensNeverDecrementTheTotals pins the -1 sentinel end
// to end through the real SQL: a gateway that never sends usage must leave
// the additive monthly totals at zero — not at -1, decrementing spend —
// while the ledger row keeps -1/-1 so the unknown stays visible.
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

// TestExplain_StreamedModelNameDoesNotSteerTheLedger pins Review 25's
// impersonation finding, which outlives the heuristic it was found against:
// the model string arrives on the provider's stream and must never steer an
// internal gate — a stream naming itself anything at all has still spent
// real tokens and still writes its row.
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

// TestExplain_CacheRowExpiresAfterAnHour pins the bound that makes Decision
// 7's split honest: the volatile context shapes the answer but sits outside
// the hash, so a cached row must stop being served after an hour — otherwise
// an answer written while an incident was open keeps asserting it as current
// forever. A flap cycle is minutes, so the hour keeps the flap immunity the
// split exists for; the fresh answer re-stamps the row on overwrite.
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

	// The upsert that re-cached the expired row must also have re-stamped it:
	// with created_at left at the first write, the fresh answer would itself
	// already be expired and never serve.
	if res, err := acct.Explain(ctx, tenantID, ExplainLogs, in, limitPtr(5)); err != nil || !res.Cached {
		t.Fatalf("the re-written row must serve again: cached = %v, err = %v — the upsert must refresh created_at", res.Cached, err)
	}
	if model.calls != 2 {
		t.Fatalf("model calls after the re-cached hit = %d, want 2", model.calls)
	}
}
