package api

import (
	"encoding/json"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ingest"
)

// The flush seam promotes classified rows to events: the Event name survives,
// sha is aliased from commit_sha, plain lines stay log-only, corrupt JSON drops.
func TestDecodeRows(t *testing.T) {
	cases := []struct {
		name          string
		env           *ingest.RowEnvelope // nil → feed raw corrupt JSON
		wantLogRows   int
		wantEventRows int
	}{
		{
			name: "classified deploy row promotes one event",
			env: &ingest.RowEnvelope{
				TenantID: 7, ProjectID: 9, Seq: 42,
				TS: "2026-08-18T12:00:00.000Z", Level: "info", Message: "deploy",
				Attrs: map[string]string{"commit_sha": "abc1234"},
				Event: "deploy",
			},
			wantLogRows:   1,
			wantEventRows: 1,
		},
		{
			name: "plain log line stays log-only",
			env: &ingest.RowEnvelope{
				TenantID: 7, ProjectID: 9,
				Level: "error", Message: "boom",
			},
			wantLogRows:   1,
			wantEventRows: 0,
		},
		{
			name:          "corrupt JSON is skipped, not fatal",
			env:           nil,
			wantLogRows:   0,
			wantEventRows: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("not json at all")
			if tc.env != nil {
				raw = mustMarshalEnvelope(t, tc.env)
			}
			logRows, eventRows := decodeRows([][]byte{raw})
			if len(logRows) != tc.wantLogRows {
				t.Fatalf("log rows = %d, want %d", len(logRows), tc.wantLogRows)
			}
			if len(eventRows) != tc.wantEventRows {
				t.Fatalf("event rows = %d, want %d", len(eventRows), tc.wantEventRows)
			}
			if tc.wantEventRows == 0 {
				return
			}

			ev := eventRows[0]
			if ev.Name != "deploy" {
				t.Errorf("event name = %q, want %q", ev.Name, "deploy")
			}
			if ev.TenantID != 7 || ev.ProjectID != 9 {
				t.Errorf("event ids = (%d, %d), want (7, 9)", ev.TenantID, ev.ProjectID)
			}
			if want := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC); !ev.TS.Equal(want) {
				t.Errorf("event TS = %v, want %v (same as the LogRow)", ev.TS, want)
			}
			if ev.Labels["sha"] != "abc1234" {
				t.Errorf(`event Labels["sha"] = %q, want %q (aliased from commit_sha)`, ev.Labels["sha"], "abc1234")
			}
			if ev.Labels["commit_sha"] != "abc1234" {
				t.Errorf(`event Labels["commit_sha"] = %q, want %q (original attr kept)`, ev.Labels["commit_sha"], "abc1234")
			}
			// The LogRow shares env.Attrs, so a mutated (non-copied) label set
			// would leak the alias back into the log row.
			if logRows[0].Attrs["sha"] != "" {
				t.Errorf(`log row Attrs["sha"] = %q — event labels must be a copy, not a mutation of env.Attrs`, logRows[0].Attrs["sha"])
			}
		})
	}
}

func mustMarshalEnvelope(t *testing.T, env *ingest.RowEnvelope) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}
