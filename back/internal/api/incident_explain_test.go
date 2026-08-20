//go:build integration

// Handler-level coverage for the incident explain (incident-triage-honesty
// T5): the id gate (an incident this tenant does not own is a 404 before
// anything is counted), the happy path through the stub brain (an incident
// with a frozen slice answers 200 with a cause), and the quota wall (a plan
// whose entitlement allows zero explains answers the 402 upgrade shape the
// client reads).
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/api/...
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

	"github.com/jackc/pgx/v5/pgtype"
	"go.upcontrol.io/back/internal/ai"
	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// openIncidentExplainDB applies migrations and returns a WriteAPI wired to
// the stub brain, plus a tenant on the given plan, mirroring openExplainDB
// above.
func openIncidentExplainDB(t *testing.T, plan string, zeroQuota bool) (*WriteAPI, int64) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping incident explain handler integration test")
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
	// The quota-0 case needs an entitlement row that allows nothing; every
	// seeded plan allows some. Same column shape as the seeded rows.
	if zeroQuota {
		if _, err := pool.Raw().Exec(ctx,
			`INSERT INTO plan_entitlement (plan, http_checks, regions, window_lines, window_hours, retain_mult, ai_explains, incident_days)
			 VALUES ('ZeroAI', 3, 1, 25000, 24, 2.0, 0, 30)
			 ON CONFLICT (plan) DO NOTHING`); err != nil {
			t.Fatalf("seed zero-quota plan: %v", err)
		}
	}
	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name, plan) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		fmt.Sprintf("incident-explain-%d", time.Now().UnixNano()), plan).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return &WriteAPI{pool: pool, acct: ai.New(pool, &stubExplainLLM{}, ai.Prices{})}, tenantID
}

// seedIncidentWithSlice plants one ongoing incident with a lifecycle mark
// and — unless withSlice is false — a frozen log slice: the rows both
// incidentWithEvidence and the explain handler read. withSlice false seeds
// the incident and its lifecycle mark only, for the empty-slice promise (an
// incident that fired with nothing frozen still explains, off its timeline).
// Returns the PUBLIC id, as `uuidStr` renders it — the only id a caller ever
// holds, because incidentToAPI sends nothing else. Returning the serial id
// here is what let the handler 404 every real click while this test passed:
// the test addressed the row by a number no response had ever carried.
func seedIncidentWithSlice(t *testing.T, h *WriteAPI, tenantID int64, withSlice bool) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var projectID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'example.com') RETURNING id`,
		tenantID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	opened := time.Now().UTC().Add(-12 * time.Minute)
	var id int64
	var pubID pgtype.UUID
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO incident (public_id, tenant_id, project_id, detector, fingerprint, title, status, detected_at)
		 VALUES (gen_random_uuid(), $1, $2, 'monitor', 1, 'Checkout is down', 'down', $3) RETURNING id, public_id`,
		tenantID, projectID, opened).Scan(&id, &pubID); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO incident_update (incident_id, at, kind, text) VALUES ($1, $2, 'opened', 'Checkout started failing')`,
		id, opened); err != nil {
		t.Fatalf("seed incident update: %v", err)
	}
	if withSlice {
		if _, err := h.pool.Raw().Exec(ctx,
			`INSERT INTO incident_slice (incident_id, seq, ts, level, service, message) VALUES
			 ($1, 1, $2, 'error', 'api', 'checkout: connect ECONNREFUSED 10.0.0.1:5432'),
			 ($1, 2, $3, 'error', 'api', 'checkout: payment query gave up after 3 retries')`,
			id, opened.Add(time.Minute), opened.Add(2*time.Minute)); err != nil {
			t.Fatalf("seed incident slice: %v", err)
		}
	}
	return id, uuidStr(pubID)
}

func TestExplainIncident(t *testing.T) {
	cases := []struct {
		name      string
		plan      string
		zeroQuota bool
		unknownID bool
		noSlice   bool
		want      int
	}{
		{"unknown incident id is a 404", "Free", false, true, false, http.StatusNotFound},
		{"incident with a log slice answers 200", "Free", false, false, false, http.StatusOK},
		{"incident with no log slice still answers 200", "Free", false, false, true, http.StatusOK},
		{"quota-0 plan is a 402", "ZeroAI", true, false, false, http.StatusPaymentRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, tenantID := openIncidentExplainDB(t, tc.plan, tc.zeroQuota)
			_, pathID := seedIncidentWithSlice(t, h, tenantID, !tc.noSlice)
			if tc.unknownID {
				// A well-formed public id that belongs to nobody.
				pathID = "ffffffffffffffffffffffffffffffff"
			}
			// The handler reads the {id} segment the mux binds, so the test
			// binds it the same way rather than parsing the URL itself.
			r := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+pathID+"/explain", nil)
			r.SetPathValue("id", pathID)
			w := httptest.NewRecorder()
			h.explainIncident(w, r, tenantID)
			if w.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
			switch tc.want {
			case http.StatusOK:
				var body struct {
					Cause  string `json:"cause"`
					Used   int    `json:"used"`
					Limit  int    `json:"limit"`
					Prompt string `json:"prompt"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if body.Cause == "" {
					t.Fatalf("body = %s, want a non-empty cause — the brain answers even with no lines", w.Body.String())
				}
				if body.Used != 1 || body.Limit != 5 {
					t.Fatalf("body = used %d, limit %d — want 1/5 (Free's row, first explain)", body.Used, body.Limit)
				}
				// The fact lines must reach the model input: the seeded title rides
				// as the first context line of the prompt.
				if !strings.Contains(body.Prompt, "Incident: Checkout is down") {
					t.Fatalf("prompt = %q, want it to contain the incident fact line", body.Prompt)
				}
				// The stub answer carries no severity: both keys must be
				// PRESENT and null — honestly absent, not missing.
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
					t.Fatalf("decode raw body: %v", err)
				}
				for _, key := range []string{"severity", "area"} {
					v, ok := raw[key]
					if !ok {
						t.Fatalf("body = %s, want key %q present", w.Body.String(), key)
					}
					if string(v) != "null" {
						t.Fatalf("%s = %s, want null (an answer without a grade shows no badge)", key, v)
					}
				}
			case http.StatusPaymentRequired:
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
			}
		})
	}
}
