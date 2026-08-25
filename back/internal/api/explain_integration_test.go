//go:build integration

// Handler-level coverage of the explain quota gate: unknown plan falls back to
// Free, a dead database 500s fail-closed, an exhausted quota answers 402.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ai"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// stubExplainLLM stands in for the provider behind the handler.
type stubExplainLLM struct{ calls int }

func (s *stubExplainLLM) ID(context.Context) string { return "stub" }

func (s *stubExplainLLM) Complete(_ context.Context, _ ai.Scenario, _ ai.Input) (ai.Completion, error) {
	s.calls++
	return ai.Completion{
		RawJSON:          []byte(`{"problem":"the dependency refused the connection","cause":"the downstream is down or not listening","confidence":"medium","fix":null,"investigate":[{"step":"Check the dependency is up.","command":null}]}`),
		Model:            "gpt-test",
		PromptTokens:     120,
		CompletionTokens: 45,
	}, nil
}

// openExplainDB applies migrations and returns a pool, a tenant on the given
// plan and the stub model wired into a WriteAPI.
func openExplainDB(t *testing.T, plan string) (*WriteAPI, int64, *stubExplainLLM) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping explain handler integration test")
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
		`INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		fmt.Sprintf("explain-api-%d", time.Now().UnixNano()), plan).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	model := &stubExplainLLM{}
	return &WriteAPI{pool: pool, acct: ai.New(pool, model, ai.Prices{})}, tenantID, model
}

func explainRequest(t *testing.T, line string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/logs/explain",
		strings.NewReader(fmt.Sprintf(`{"lines":[%q]}`, line)))
}

// A plan string outside the seeded plan_entitlement rows falls back to Free,
// the most restrictive seeded tier, instead of 500ing forever.
func TestExplainLogs_UnknownPlanFallsBackToFree(t *testing.T) {
	h, tenantID, model := openExplainDB(t, "Legacy-2019")

	w := httptest.NewRecorder()
	h.explainLogs(w, explainRequest(t, "connect ECONNREFUSED 10.0.0.1:5432"), tenantID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 — an unknown plan is Free's limit, not a permanent 500", w.Code, w.Body.String())
	}
	var body struct {
		Limit  int  `json:"limit"`
		Used   int  `json:"used"`
		Cached bool `json:"cached"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Limit != 5 || body.Used != 1 || body.Cached {
		t.Fatalf("body = limit %d, used %d, cached %v — want 5/1/false (Free's row)", body.Limit, body.Used, body.Cached)
	}
	if model.calls != 1 {
		t.Fatalf("the model was called %d times, want 1", model.calls)
	}
}

// The fail-closed 500: a database that cannot answer the plan read is an
// error, never a silent unlimited — and the provider is never called.
func TestExplainLogs_DeadDatabaseIs500(t *testing.T) {
	h, tenantID, model := openExplainDB(t, "Free")
	h.pool.Close() // idempotent against t.Cleanup

	w := httptest.NewRecorder()
	h.explainLogs(w, explainRequest(t, "401 unauthorized: invalid token"), tenantID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d (%s), want 500 — an unreadable plan read fails closed", w.Code, w.Body.String())
	}
	if model.calls != 0 {
		t.Fatalf("the model was called %d times, want 0 — a dead gate never reaches the provider", model.calls)
	}
}

// The quota wall arrives as the 402 carrying the upgrade reason the client
// shows, after exactly the plan's five Free explains.
func TestExplainLogs_ExhaustedQuotaIs402(t *testing.T) {
	h, tenantID, model := openExplainDB(t, "Free")

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		h.explainLogs(w, explainRequest(t, fmt.Sprintf("call %d: connect ECONNREFUSED 10.0.0.1:5432", i)), tenantID)
		if w.Code != http.StatusOK {
			t.Fatalf("explain %d: status = %d (%s), want 200 — Free allows five", i, w.Code, w.Body.String())
		}
	}
	// Five answers plus the refusal sit exactly on explainBurst (6): empty the
	// window first so the next assertion meets the quota gate, not the 429.
	h.explainSeenAt = nil
	w := httptest.NewRecorder()
	h.explainLogs(w, explainRequest(t, "sixth: connect ECONNREFUSED 10.0.0.1:5432"), tenantID)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("sixth explain: status = %d (%s), want 402", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Upgrade struct {
				Reason string `json:"reason"`
			} `json:"upgrade"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 402 body: %v", err)
	}
	if body.Error.Code != "plan_limit_exceeded" || body.Error.Upgrade.Reason == "" {
		t.Fatalf("402 body = %s, want plan_limit_exceeded with an upgrade reason", w.Body.String())
	}
	if model.calls != 5 {
		t.Fatalf("the model was called %d times, want 5 — the refused explain never reaches the provider", model.calls)
	}
}

// Settings reads the wired brain's identity with an empty `lines`: it must
// reach the handler and answer 200 with the brain's id, never a 400.
func TestPreviewExplain_EmptyLinesIsTheBrainProbe(t *testing.T) {
	h, tenantID, _ := openExplainDB(t, "Free")

	w := httptest.NewRecorder()
	h.previewExplain(w, httptest.NewRequest(http.MethodPost, "/v1/logs/explain/preview",
		strings.NewReader(`{"lines":[]}`)), tenantID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 — empty lines is the Settings brain probe, not a bad request", w.Code, w.Body.String())
	}
	var body struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Model == nil || *body.Model != "stub" {
		t.Fatalf("model = %v, want \"stub\" — the wired brain's id", body.Model)
	}
}
