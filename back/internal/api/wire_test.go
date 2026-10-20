package api

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/ingest"
)

// The flush seam promotes classified rows to events: the Event name survives,
// sha is aliased from commit_sha, plain lines stay log-only, corrupt JSON drops.
func TestDecodeRows(t *testing.T) {
	sent := time.Now().UTC().Add(-time.Hour) // inside the log partitions: both rows are written
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
				TS: sent.Format(time.RFC3339Nano), Level: "info", Message: "deploy",
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
			if !ev.TS.Equal(sent) {
				t.Errorf("event TS = %v, want %v (same as the LogRow)", ev.TS, sent)
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

// A ts outside the days logs has partitions for must not fail the COPY it rides
// in: a plain line's log row is stamped with the receive time, while an event
// keeps its own time in its event row and has no log row, so a backfill does not
// show up as lines sent now.
func TestDecodeRowsClampsLogTimestamp(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name     string
		offset   time.Duration // the sent ts, relative to now
		event    string
		wantLogs int  // log rows written
		clamped  bool // the log row is stamped with the receive time
	}{
		{"event 400 days old", -400 * day, "deploy", 0, false},
		{"event 10 days ahead", 10 * day, "deploy", 0, false},
		{"plain log line, 400 days old", -400 * day, "", 1, true},
		{"plain log line, 10 days ahead", 10 * day, "", 1, true},
		{"event an hour ago", -time.Hour, "deploy", 1, false},
		{"event 29 days old is still inside", -29 * day, "deploy", 1, false},
		{"event 1 day ahead is still inside", day, "deploy", 1, false},
		{"plain log line, an hour ago", -time.Hour, "", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent := time.Now().UTC().Add(tc.offset)
			raw := mustMarshalEnvelope(t, &ingest.RowEnvelope{
				TenantID: 7, ProjectID: 9, Level: "info", Message: "m",
				TS: sent.Format(time.RFC3339Nano), Event: tc.event,
			})
			logRows, eventRows := decodeRows([][]byte{raw})
			if len(logRows) != tc.wantLogs {
				t.Fatalf("log rows = %d, want %d", len(logRows), tc.wantLogs)
			}
			if tc.wantLogs == 1 {
				if tc.clamped {
					assertNow(t, "log row TS", logRows[0].TS)
				} else if !logRows[0].TS.Equal(sent) {
					t.Errorf("log row TS = %v, want the sent %v", logRows[0].TS, sent)
				}
			}
			if tc.event == "" {
				if len(eventRows) != 0 {
					t.Fatalf("event rows = %d, want 0", len(eventRows))
				}
				return
			}
			if len(eventRows) != 1 {
				t.Fatalf("event rows = %d, want 1", len(eventRows))
			}
			if !eventRows[0].TS.Equal(sent) {
				t.Errorf("event TS = %v, want the sent %v (an event keeps its own time)", eventRows[0].TS, sent)
			}
		})
	}
}

// A missing or unreadable ts still means the receive time, for both rows.
func TestDecodeRowsMissingTimestampIsReceiveTime(t *testing.T) {
	cases := map[string]string{"missing": "", "unreadable": "last tuesday"}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			raw := mustMarshalEnvelope(t, &ingest.RowEnvelope{
				TenantID: 7, ProjectID: 9, Level: "info", Message: "m", TS: ts, Event: "deploy",
			})
			logRows, eventRows := decodeRows([][]byte{raw})
			if len(logRows) != 1 || len(eventRows) != 1 {
				t.Fatalf("rows = (%d, %d), want (1, 1)", len(logRows), len(eventRows))
			}
			assertNow(t, "log row TS", logRows[0].TS)
			assertNow(t, "event TS", eventRows[0].TS)
		})
	}
}

// An event filters by service like its log twin: the envelope's service is
// copied into the labels unless the sender set one, and the log row's attrs
// stay as sent.
func TestDecodeRowsEventServiceLabel(t *testing.T) {
	cases := []struct {
		name    string
		service string
		attrs   map[string]string
		want    map[string]string
	}{
		{"copied when absent", "api",
			map[string]string{"region": "eu"},
			map[string]string{"region": "eu", "service": "api"}},
		{"the sender's label is not overwritten", "api",
			map[string]string{"service": "billing"},
			map[string]string{"service": "billing"}},
		{"copied into a fresh map when attrs are nil", "api",
			nil,
			map[string]string{"service": "api"}},
		{"beside the sha alias", "api",
			map[string]string{"commit_sha": "abc1234"},
			map[string]string{"commit_sha": "abc1234", "sha": "abc1234", "service": "api"}},
		{"no service, nothing to copy", "",
			map[string]string{"region": "eu"},
			map[string]string{"region": "eu"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := mustMarshalEnvelope(t, &ingest.RowEnvelope{
				TenantID: 7, ProjectID: 9, Level: "info", Message: "m",
				Service: tc.service, Attrs: tc.attrs, Event: "deploy",
			})
			logRows, eventRows := decodeRows([][]byte{raw})
			if len(logRows) != 1 || len(eventRows) != 1 {
				t.Fatalf("rows = (%d, %d), want (1, 1)", len(logRows), len(eventRows))
			}
			if !maps.Equal(eventRows[0].Labels, tc.want) {
				t.Errorf("event labels = %v, want %v", eventRows[0].Labels, tc.want)
			}
			if !maps.Equal(logRows[0].Attrs, tc.attrs) {
				t.Errorf("log row attrs = %v, want %v as sent (event labels must be a copy)", logRows[0].Attrs, tc.attrs)
			}
		})
	}
}

// The amount ingest lifted off uc.amount / uc.currency reaches the events row.
func TestDecodeRowsEventCarriesAmount(t *testing.T) {
	raw := mustMarshalEnvelope(t, &ingest.RowEnvelope{
		TenantID: 7, ProjectID: 9, Level: "info", Message: "m", Event: "payment",
		AmountMinor: 1999, Currency: "USD",
	})
	_, eventRows := decodeRows([][]byte{raw})
	if len(eventRows) != 1 || eventRows[0].AmountMinor != 1999 || eventRows[0].Currency != "USD" {
		t.Fatalf("event rows = %+v, want one carrying 1999 USD", eventRows)
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

// assertNow checks got is the moment the test runs, to a minute: a stamp that
// was not clamped, or not defaulted, is off by days.
func assertNow(t *testing.T, what string, got time.Time) {
	t.Helper()
	if d := time.Since(got); d < -time.Minute || d > time.Minute {
		t.Errorf("%s = %v, want about now", what, got)
	}
}
