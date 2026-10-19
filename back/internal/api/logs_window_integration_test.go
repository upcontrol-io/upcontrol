//go:build integration

// GET /v1/logs against a real database: every part of the answer stops at the
// plan's log window, the map says its own bucket width, and detail comes only
// when it is finer than that map. Run with -tags=integration and
// UC_TEST_POSTGRES set.

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/pg"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

type logsAnswer struct {
	Lines []struct {
		Service string `json:"service"`
	} `json:"lines"`
	Volume []struct {
		Lines int `json:"lines"`
	} `json:"volume"`
	VolumeBucketSeconds *int `json:"volumeBucketSeconds"`
	Total               int  `json:"total"`
	Services            []struct {
		Name string `json:"name"`
	} `json:"services"`
	Detail *struct {
		BucketSeconds int `json:"bucketSeconds"`
	} `json:"detail"`
}

// logsWorld is a tenant on plan whose owner reads the logs, one line from a
// minute ago (service "fresh") and one from three days ago ("stale").
func logsWorld(t *testing.T, plan string) *writeAPI {
	t.Helper()
	pool := openProjectsGateDB(t)
	tenantID := seedPlanTenant(t, pool, plan, 1)
	h := planTenantAPI(t, pool, tenantID)
	h.pgs = pgstore.New(pool.Raw())
	project := boardOf(t, pool, tenantID)
	now := time.Now().UTC()
	seedLogLine(t, pool, tenantID, project, 1, now.Add(-72*time.Hour), "stale")
	seedLogLine(t, pool, tenantID, project, 2, now.Add(-time.Minute), "fresh")
	return h
}

func seedLogLine(t *testing.T, pool *pg.Pool, tenantID, projectID, seq int64, ts time.Time, service string) {
	t.Helper()
	// The day's partition may predate this database or lie behind its roll.
	day := ts.UTC().Truncate(24 * time.Hour)
	if _, err := pool.Raw().Exec(t.Context(), fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS logs_%s PARTITION OF logs FOR VALUES FROM ('%s') TO ('%s')",
		day.Format("20060102"), day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))); err != nil {
		t.Fatalf("partition for %s: %v", day, err)
	}
	if _, err := pool.Raw().Exec(t.Context(),
		`INSERT INTO logs (tenant_id, project_id, ts, seq, source, service, host, level, message, fingerprint)
		 VALUES ($1, $2, $3, $4, 'test', $5, 'h', 'info', 'line', 0)`,
		tenantID, projectID, ts, seq, service); err != nil {
		t.Fatalf("seed %s line: %v", service, err)
	}
}

func readLogs(t *testing.T, h *writeAPI, query string) logsAnswer {
	t.Helper()
	code, body := sessionCall(t, h, http.MethodGet, "/v1/logs"+query, "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/logs%s = %d %s", query, code, body)
	}
	var out logsAnswer
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode the logs: %v (%s)", err, body)
	}
	return out
}

func TestLogsReadStopsAtThePlanWindow(t *testing.T) {
	h := logsWorld(t, "Free") // a 24h window
	now := time.Now().UTC()
	for name, query := range map[string]string{
		"the whole window": "",
		// A from below the floor is raised to it, never honoured.
		"a range reaching past it": "?from=" + now.Add(-96*time.Hour).Format(time.RFC3339) +
			"&to=" + now.Add(time.Minute).Format(time.RFC3339),
	} {
		t.Run(name, func(t *testing.T) {
			got := readLogs(t, h, query)
			if len(got.Lines) != 1 || got.Lines[0].Service != "fresh" {
				t.Errorf("lines = %+v, want the fresh line alone", got.Lines)
			}
			if got.Total != 1 {
				t.Errorf("total = %d, want 1", got.Total)
			}
			sum := 0
			for _, b := range got.Volume {
				sum += b.Lines
			}
			if sum != 1 {
				t.Errorf("volume holds %d lines, want 1", sum)
			}
			if len(got.Services) != 1 || got.Services[0].Name != "fresh" {
				t.Errorf("services = %+v, want fresh alone", got.Services)
			}
			if got.VolumeBucketSeconds == nil || *got.VolumeBucketSeconds != 60 {
				t.Errorf("volumeBucketSeconds = %v, want 60 on a day's window", got.VolumeBucketSeconds)
			}
		})
	}
}

func TestLogsDetailOnlyWhenFinerThanTheMap(t *testing.T) {
	now := time.Now().UTC()
	hour := "&from=" + now.Add(-time.Hour).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)

	agency := logsWorld(t, "Agency") // 720h: the map is 30 minutes wide
	got := readLogs(t, agency, "?bucketSeconds=30"+hour)
	if got.VolumeBucketSeconds == nil || *got.VolumeBucketSeconds != 1800 {
		t.Fatalf("volumeBucketSeconds = %v, want 1800 on a month's window", got.VolumeBucketSeconds)
	}
	if got.Detail == nil || got.Detail.BucketSeconds != 30 {
		t.Fatalf("detail = %+v, want 30s under a 30-minute map", got.Detail)
	}

	free := logsWorld(t, "Free")
	if got := readLogs(t, free, "?bucketSeconds=60"+hour); got.Detail != nil {
		t.Fatalf("a minute of detail under a minute map adds nothing; got %+v", got.Detail)
	}
	if got := readLogs(t, free, "?bucketSeconds=5"+hour); got.Detail == nil || got.Detail.BucketSeconds != 10 {
		t.Fatalf("detail = %+v, want an hour snapped to 10s", got.Detail)
	}
}
