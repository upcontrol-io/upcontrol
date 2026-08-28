package decode

import (
	"testing"
	"time"
)

func TestSniff(t *testing.T) {
	cases := []struct {
		name string
		body string
		ct   string
		want Format
	}{
		{"empty", "", "", FormatUnknown},
		{"ndjson", "{\"msg\":\"a\"}\n{\"msg\":\"b\"}\n", "", FormatNDJSON},
		{"json-array", "[{\"msg\":\"a\"},{\"msg\":\"b\"}]", "", FormatJSONArray},
		{"json-object", "{\"msg\":\"a\",\"level\":\"error\"}", "", FormatJSONObject},
		{"plain", "line one\nline two\n", "", FormatPlain},
		{"plain-no-json", "not json at all", "", FormatPlain},
		{"syslog", "<134>1 2026-08-12T14:32:04Z app cmd - - hello", "", FormatSyslog},
		{"otlp", `{"resourceLogs":[{"scopeLogs":[{"logRecords":[]}]}]}`, "", FormatOTLP},
		{"loki", `{"streams":[{"stream":{"level":"info"},"values":[["1","x"]]}]}`, "", FormatLoki},
		{"sentry-ct", `{"event_id":"x","sent_at":"t"}` + "\n" + `{"message":"boom"}`, "application/octet-stream", FormatSentry},
		{"loki-ct", `{"x":1}`, "application/json; loki", FormatLoki},
		{"alertmanager", `[{"status":"firing","labels":{"alertname":"X"},"startsAt":"...","annotations":{}}]`, "", FormatAlertmanager},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sniff([]byte(c.body), c.ct)
			if got != c.want {
				t.Errorf("sniff(%q) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

func TestDecodeNDJSON(t *testing.T) {
	body := []byte(`{"msg":"a","level":"error","ts":"2026-08-12T14:32:04Z"}` + "\n" +
		`{"msg":"b"}` + "\n")
	r := Decode(body, "")
	if r.Format != FormatNDJSON {
		t.Fatalf("format = %q, want ndjson", r.Format)
	}
	if len(r.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(r.Records))
	}
	if r.Records[0].Message != "a" || r.Records[0].Level != "error" {
		t.Errorf("rec0 = %+v", r.Records[0])
	}
	if !r.Records[0].Time.Equal(time.Date(2026, 8, 12, 14, 32, 4, 0, time.UTC)) {
		t.Errorf("rec0 ts = %v", r.Records[0].Time)
	}
	// rec1 has no ts → ts_absent warning, level defaulted to info.
	if !r.Records[1].Time.IsZero() {
		t.Errorf("rec1 ts should be zero")
	}
	if !hasWarning(r.Warnings, WarnTimestampAbsent) {
		t.Errorf("expected ts_absent warning, got %v", r.Warnings)
	}
	if r.Records[1].Level != "info" {
		t.Errorf("rec1 level = %q, want info", r.Records[1].Level)
	}
}

func TestDecodeJSONObject(t *testing.T) {
	r := Decode([]byte(`{"service":"app","host":"h1","level":"warn","message":"hi"}`), "")
	if r.Format != FormatJSONObject || len(r.Records) != 1 {
		t.Fatalf("format/recs: %q %d", r.Format, len(r.Records))
	}
	rec := r.Records[0]
	if rec.Service != "app" || rec.Host != "h1" || rec.Level != "warn" || rec.Message != "hi" {
		t.Errorf("rec = %+v", rec)
	}
}

func TestDecodeEventObject(t *testing.T) {
	// The SDK's {"event":"payment_failed"} shape: message is the event name.
	r := Decode([]byte(`{"event":"payment_failed"}`), "")
	if len(r.Records) != 1 || r.Records[0].Message != "payment_failed" {
		t.Fatalf("event decode: %+v", r.Records)
	}
}

func TestDecodePlain(t *testing.T) {
	r := Decode([]byte("GET / 200 12ms\nPOST /checkout 502\n\n"), "")
	if r.Format != FormatPlain {
		t.Fatalf("format = %q", r.Format)
	}
	if len(r.Records) != 2 {
		t.Fatalf("records = %d, want 2 (blank line skipped)", len(r.Records))
	}
}

func TestDecodeSyslog(t *testing.T) {
	body := []byte("<134>1 2026-08-12T14:32:04Z app cmd 123 - hello\n<38>1 ...")
	r := Decode(body, "")
	if r.Format != FormatSyslog {
		t.Fatalf("format = %q", r.Format)
	}
	// pri 134 → severity 6 (info); pri 38 → severity 6 (info).
	if len(r.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(r.Records))
	}
	if r.Records[0].Level != "info" {
		t.Errorf("rec0 level = %q, want info", r.Records[0].Level)
	}
}

func TestDecodeLoki(t *testing.T) {
	body := []byte(`{"streams":[{"stream":{"level":"error","job":"app"},"values":[["1660000000000000000","boom"]]}]}`)
	r := Decode(body, "")
	if r.Format != FormatLoki {
		t.Fatalf("format = %q", r.Format)
	}
	if len(r.Records) != 1 {
		t.Fatalf("records = %d", len(r.Records))
	}
	if r.Records[0].Message != "boom" || r.Records[0].Level != "error" || r.Records[0].Service != "app" {
		t.Errorf("rec = %+v", r.Records[0])
	}
}

func TestDecodeAlertmanager(t *testing.T) {
	body := []byte(`[{"status":"firing","labels":{"alertname":"HighCPU","severity":"critical"},"startsAt":"2026-08-12T14:32:04Z","annotations":{}}]`)
	r := Decode(body, "")
	if r.Format != FormatAlertmanager {
		t.Fatalf("format = %q", r.Format)
	}
	if len(r.Records) != 1 {
		t.Fatalf("records = %d", len(r.Records))
	}
	if r.Records[0].Message != "HighCPU" {
		t.Errorf("message = %q, want HighCPU", r.Records[0].Message)
	}
	if r.Records[0].Attrs["severity"] != "critical" {
		t.Errorf("severity attr missing: %+v", r.Records[0].Attrs)
	}
}

func TestDecodeLevelUnknownWarning(t *testing.T) {
	// A non-standard level raises level_unknown and defaults to info.
	r := Decode([]byte(`{"msg":"x","level":"FATALISH"}`), "")
	if !hasWarning(r.Warnings, WarnLevelUnknown) {
		t.Errorf("expected level_unknown warning, got %v", r.Warnings)
	}
	if r.Records[0].Level != "info" {
		t.Errorf("level = %q, want info (defaulted)", r.Records[0].Level)
	}
}

func TestNormalizeLevelKeepsRawAndMapsCanonical(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantRaw string
		unknown bool
	}{
		{"fatal maps to error", "fatal", "error", "fatal", false},
		{"casing collapsed", "ERROR", "error", "ERROR", false},
		{"warning collapses to warn", "Warning", "warn", "Warning", false},
		{"unknown defaults to info", "sev2", "info", "sev2", true},
		{"empty is info, no warning", "", "info", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Record{Level: c.in}
			ws := &warningSet{}
			normalizeLevel(&r, ws)
			if r.Level != c.want {
				t.Errorf("Level = %q, want %q", r.Level, c.want)
			}
			if r.LevelRaw != c.wantRaw {
				t.Errorf("LevelRaw = %q, want %q", r.LevelRaw, c.wantRaw)
			}
			if got := hasWarning(ws.slice(), WarnLevelUnknown); got != c.unknown {
				t.Errorf("level_unknown warning = %v, want %v", got, c.unknown)
			}
		})
	}
}

func TestDecodeUnparseableLineIsWarning(t *testing.T) {
	// A garbled line in an NDJSON stream is a warning, never a hard error.
	body := []byte("{not json}\n{\"msg\":\"ok\"}\n")
	r := Decode(body, "")
	if !hasWarning(r.Warnings, WarnUnparseable) {
		t.Errorf("expected unparseable warning, got %v", r.Warnings)
	}
	if len(r.Records) != 1 {
		t.Errorf("records = %d, want 1 (the good line)", len(r.Records))
	}
}

func TestToStringFloats(t *testing.T) {
	// Regression: json.Marshal already emits the shortest form, so no
	// trailing-zero trimming (the old trimFloat turned 100 into "1").
	cases := []struct {
		in   float64
		want string
	}{
		{100, "100"},
		{2.5, "2.5"},
		{1724668800, "1724668800"},
	}
	for _, c := range cases {
		if got := toString(c.in); got != c.want {
			t.Errorf("toString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func hasWarning(ws []Warning, code WarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}
