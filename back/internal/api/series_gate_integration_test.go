//go:build integration

// The history plan-axis gate: which range each plan may ask POST /v1/series
// for, the copy the 402 carries and the plan that lifts it. Run with
// -tags=integration and UC_TEST_POSTGRES set; the migrations seed the ladder
// the assertions read back, which is the point: the table decides, not this
// file.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHistoryRefusal(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	ctx := context.Background()
	cases := []struct {
		name        string
		plan        string
		ranges      []string
		wantMsg     string
		wantUpgrade string
	}{
		{"a day is on every plan", "Free", []string{"1h", "24h"}, "", ""},
		{"Free at a week points at Indie", "Free", []string{"7d"},
			"7 days of history is on Indie and up. It counts from the day you switch.", "indie"},
		{"Free at a month points at Growth", "Free", []string{"31d"},
			"31 days of history is on Growth and up. It counts from the day you switch.", "growth"},
		// Agency is the top rung, so the copy has nothing above it to offer.
		{"Free at a year points at Agency", "Free", []string{"365d"},
			"12 months of history is on Agency. It counts from the day you switch.", "agency"},
		{"Indie keeps its week", "Indie", []string{"7d"}, "", ""},
		{"Indie at a month points at Growth", "Indie", []string{"31d"},
			"31 days of history is on Growth and up. It counts from the day you switch.", "growth"},
		{"Growth keeps its month", "Growth", []string{"31d"}, "", ""},
		{"Agency reaches the year", "Agency", []string{"365d"}, "", ""},
		{"Self-hosted is unlimited", "Self-hosted", []string{"365d"}, "", ""},
		// One query past the depth refuses the whole batch: a board that drew
		// the shallow half and dropped the rest would read as measured.
		{"the batch is refused for its deepest query", "Free", []string{"24h", "31d", "1h"},
			"31 days of history is on Growth and up. It counts from the day you switch.", "growth"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tenantID := seedPlanTenant(t, pool, c.plan, 0)
			qs := make([]seriesQuery, 0, len(c.ranges))
			for i, rng := range c.ranges {
				qs = append(qs, seriesQuery{ID: string(rune('a' + i)), Source: "logs", Range: rng})
			}
			msg, plan, err := h.historyRefusal(ctx, tenantID, qs)
			if err != nil {
				t.Fatalf("historyRefusal(%s, %v): %v", c.plan, c.ranges, err)
			}
			if msg != c.wantMsg || plan != c.wantUpgrade {
				t.Fatalf("historyRefusal(%s, %v) = (%q, %q), want (%q, %q)",
					c.plan, c.ranges, msg, plan, c.wantMsg, c.wantUpgrade)
			}
		})
	}
}

// The wall reaches the client as the same 402 every other paid axis uses: the
// front opens UpgradeModal off error.upgrade, and a different shape here would
// surface as "Backend not reachable".
func TestHistoryRefusalTravelsAsTheUpgrade402(t *testing.T) {
	pool := openProjectsGateDB(t)
	h := &writeAPI{pool: pool}
	tenantID := seedPlanTenant(t, pool, "Free", 0)
	msg, plan, err := h.historyRefusal(context.Background(), tenantID,
		[]seriesQuery{{ID: "a", Source: "logs", Range: "31d"}})
	if err != nil || msg == "" {
		t.Fatalf("a Free tenant may not read a month; got (%q, %q, %v)", msg, plan, err)
	}
	w := httptest.NewRecorder()
	writeUpgradeRequired(w, msg, plan)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("the wall is a 402; got %d", w.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Upgrade struct {
				Plan   string `json:"plan"`
				Reason string `json:"reason"`
			} `json:"upgrade"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 402: %v", err)
	}
	if body.Error.Code != "plan_limit_exceeded" || body.Error.Upgrade.Plan != "growth" || body.Error.Upgrade.Reason != msg {
		t.Fatalf("the 402 must carry the plan and the reason; got %+v", body.Error)
	}
}
