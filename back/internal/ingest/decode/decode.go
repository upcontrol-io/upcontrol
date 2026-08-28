// Package decode turns an arbitrary POST /i body into unified Records. The
// form is sniffed from the bytes, not Content-Type; warnings ride the result.
package decode

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// Format is the detected wire form.
type Format string

const (
	FormatUnknown      Format = ""
	FormatNDJSON       Format = "ndjson"
	FormatJSONArray    Format = "json-array"
	FormatJSONObject   Format = "json-object"
	FormatPlain        Format = "plain"
	FormatSyslog       Format = "syslog"
	FormatOTLP         Format = "otlp"
	FormatSentry       Format = "sentry"
	FormatAlertmanager Format = "alertmanager"
	FormatLoki         Format = "loki"
)

// WarningCode is one entry in the receipt's closed warning dictionary.
type WarningCode string

const (
	WarnTimestampAbsent WarningCode = "ts_absent"
	WarnLevelUnknown    WarningCode = "level_unknown"
	WarnUnparseable     WarningCode = "unparseable_line"
)

// Warning is a per-code tally for the receipt.
type Warning struct {
	Code  WarningCode
	Count int
}

// Record is the unified shape every format decodes to. Time zero = the source
// carried none (caller stamps now); Raw re-marshals JSON for the metric sniffer.
type Record struct {
	Time  time.Time
	Level string
	// LevelRaw is the level exactly as sent (capped at 32 bytes); Level is the mapped one.
	LevelRaw string
	Service  string
	Host     string
	Message  string
	Attrs    map[string]string
	Raw      []byte
	// Named: the sender marked the line with "uc.event": true, declaring an
	// event name rather than sending a log message.
	Named bool
}

// warningSet accumulates per-code counts without allocations for the common path.
type warningSet struct {
	counts map[WarningCode]int
}

func (w *warningSet) add(c WarningCode) {
	if w.counts == nil {
		w.counts = map[WarningCode]int{}
	}
	w.counts[c]++
}

func (w *warningSet) slice() []Warning {
	out := make([]Warning, 0, len(w.counts))
	for c, n := range w.counts {
		out = append(out, Warning{Code: c, Count: n})
	}
	return out
}

// Result is what Decode returns: records, the detected format, and the
// warning tally; a parse failure on one line is a warning, never a hard error.
type Result struct {
	Records  []Record
	Format   Format
	Warnings []Warning
}

// sniff decides the body's form from its bytes, with Content-Type as a hint only.
// It never returns Unknown for non-empty input: the worst case is plain text.
func sniff(body []byte, contentType string) Format {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return FormatUnknown
	}

	// Content-Type hints that win outright (these formats are unambiguous).
	switch {
	case strings.Contains(ct, "sentry"):
		return FormatSentry
	case strings.Contains(ct, "loki"):
		return FormatLoki
	}

	// Sentry envelope: newline-delimited, first line a JSON object with event_id.
	if fl := firstLine(trimmed); len(fl) > 0 && fl[0] == '{' {
		var head map[string]any
		if json.Unmarshal(fl, &head) == nil {
			if _, ok := head["event_id"]; ok {
				if hasManyLines(trimmed) {
					return FormatSentry
				}
			}
			// OTLP logs JSON: top-level resourceLogs.
			if _, ok := head["resourceLogs"]; ok {
				return FormatOTLP
			}
			// Loki push: {"streams":[...]}.
			if _, ok := head["streams"]; ok {
				return FormatLoki
			}
			// NDJSON: multi-line, first line a complete object. A garbled line is
			// a warning at decode time, not a reason to reject the whole body.
			if hasManyLines(trimmed) {
				return FormatNDJSON
			}
			// Single object on one line.
			return FormatJSONObject
		}
		// Unparseable first line on a multi-line body: assume NDJSON; the bad
		// line becomes a warning and the good lines are kept.
		if hasManyLines(trimmed) {
			return FormatNDJSON
		}
		return FormatJSONObject
	}

	// Array form: JSON array of records, OR an Alertmanager POST.
	if trimmed[0] == '[' {
		if looksLikeAlertmanager(trimmed) {
			return FormatAlertmanager
		}
		return FormatJSONArray
	}

	// Syslog RFC 5424: "<PRI>VERSION ...".
	if trimmed[0] == '<' {
		if _, ok := parseSyslogPri(trimmed); ok {
			return FormatSyslog
		}
	}

	return FormatPlain
}

// Decode sniffs then parses. It is total on the recognized formats: a parse
// failure on one line becomes an unparseable_line warning, not a hard error.
func Decode(body []byte, contentType string) Result {
	format := sniff(body, contentType)
	ws := &warningSet{}
	var recs []Record
	switch format {
	case FormatNDJSON:
		recs = decodeNDJSON(body, ws)
	case FormatJSONArray:
		recs = decodeJSONArray(body, ws)
	case FormatJSONObject:
		recs = decodeJSONObject(body, ws)
	case FormatPlain:
		recs = decodePlain(body)
	case FormatSyslog:
		recs = decodeSyslog(body, ws)
	case FormatLoki:
		recs = decodeLoki(body, ws)
	case FormatAlertmanager:
		recs = decodeAlertmanager(body)
	case FormatOTLP:
		recs = decodeOTLP(body, ws)
	case FormatSentry:
		recs = decodeSentry(body)
	default:
		// Empty body: no records, no warnings.
	}
	for i := range recs {
		normalizeLevel(&recs[i], ws)
		if recs[i].Time.IsZero() {
			ws.add(WarnTimestampAbsent)
		}
	}
	return Result{Records: recs, Format: format, Warnings: ws.slice()}
}

// normalizeLevel keeps the client's spelling on LevelRaw (capped at 32 bytes)
// and maps Level to the canonical info|warn|error|debug|trace. Unknown levels
// default to info and raise level_unknown (the receipt's job to explain).
func normalizeLevel(r *Record, ws *warningSet) {
	r.LevelRaw = r.Level
	if len(r.LevelRaw) > 32 {
		r.LevelRaw = r.LevelRaw[:32]
	}
	switch strings.ToLower(strings.TrimSpace(r.Level)) {
	case "", "info", "notice":
		r.Level = "info"
	case "warn", "warning":
		r.Level = "warn"
	case "error", "err", "fatal", "critical", "crit", "panic", "emerg", "emergency", "alert":
		r.Level = "error"
	case "debug":
		r.Level = "debug"
	case "trace":
		r.Level = "trace"
	default:
		ws.add(WarnLevelUnknown)
		r.Level = "info"
	}
}

func firstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return bytes.TrimRight(b[:i], "\r")
	}
	return b
}

// hasManyLines reports whether body has >1 non-blank line.
func hasManyLines(b []byte) bool {
	count := 0
	for _, ln := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(ln)) > 0 {
			count++
		}
	}
	return count > 1
}

func looksLikeAlertmanager(b []byte) bool {
	var arr []map[string]any
	if json.Unmarshal(b, &arr) != nil || len(arr) == 0 {
		return false
	}
	_, hasStatus := arr[0]["status"]
	_, hasLabels := arr[0]["labels"]
	_, hasStarts := arr[0]["startsAt"]
	return hasStatus && hasLabels && hasStarts
}

func parseSyslogPri(b []byte) (pri int, ok bool) {
	// b[0] == '<'. Read digits until '>'.
	if len(b) < 3 || b[0] != '<' {
		return 0, false
	}
	i := 1
	n := 0
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		n = n*10 + int(b[i]-'0')
		i++
		if i > 4 {
			return 0, false
		}
	}
	if i >= len(b) || b[i] != '>' {
		return 0, false
	}
	return n, true
}

// recordsFromObject maps the common JSON log shape (ts/level/service/msg/host)
// to a Record; the rest of the keys land in Attrs.
func recordsFromObject(obj map[string]any) []Record {
	r := Record{Attrs: map[string]string{}}
	// Raw carries the re-marshalled object so a metric line is recognised from
	// its own fields; stringified attrs would mangle the labels.
	r.Raw, _ = json.Marshal(obj)
	for k, v := range obj {
		switch k {
		case "ts", "time", "timestamp", "@timestamp":
			r.Time = parseTime(toString(v))
		case "level", "severity":
			r.Level = toString(v)
		case "service", "svc":
			r.Service = toString(v)
		case "host", "hostname":
			r.Host = toString(v)
		case "msg", "message":
			r.Message = toString(v)
		case "uc.event":
			// The event marker is consumed here, never stored as an attr.
			r.Named = v == true
		default:
			r.Attrs[k] = toString(v)
		}
	}
	if r.Message == "" {
		// No message: the raw line is the best text we have.
		r.Message = string(r.Raw)
	}
	return []Record{r}
}

func decodeNDJSON(body []byte, ws *warningSet) []Record {
	var out []Record
	for _, ln := range bytes.Split(body, []byte{'\n'}) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(ln, &obj) != nil {
			ws.add(WarnUnparseable)
			continue
		}
		out = append(out, recordsFromObject(obj)...)
	}
	return out
}

func decodeJSONArray(body []byte, ws *warningSet) []Record {
	var arr []map[string]any
	if json.Unmarshal(body, &arr) != nil {
		ws.add(WarnUnparseable)
		return nil
	}
	var out []Record
	for _, obj := range arr {
		out = append(out, recordsFromObject(obj)...)
	}
	return out
}

func decodeJSONObject(body []byte, ws *warningSet) []Record {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		ws.add(WarnUnparseable)
		return nil
	}
	// A top-level {"event": "..."} payload (the SDK's /v1/event shape) is one
	// event record; message is the event.
	if ev, ok := obj["event"]; ok && len(obj) <= 4 {
		r := Record{Attrs: map[string]string{}}
		r.Message = toString(ev)
		r.Named = true
		if name, ok := obj["name"]; ok {
			r.Attrs["name"] = toString(name)
		}
		return []Record{r}
	}
	return recordsFromObject(obj)
}

func decodePlain(body []byte) []Record {
	var out []Record
	for _, ln := range bytes.Split(body, []byte{'\n'}) {
		ln = bytes.TrimRight(ln, "\r")
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		out = append(out, Record{Message: string(ln)})
	}
	return out
}

func decodeSyslog(body []byte, ws *warningSet) []Record {
	var out []Record
	for _, ln := range bytes.Split(body, []byte{'\n'}) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		pri, ok := parseSyslogPri(ln)
		if !ok {
			ws.add(WarnUnparseable)
			continue
		}
		// severity = pri % 8: 0 emerg .. 7 debug. Map to our levels.
		sev := pri % 8
		r := Record{Message: string(ln), Level: syslogSeverity(sev)}
		out = append(out, r)
	}
	return out
}

func syslogSeverity(sev int) string {
	switch sev {
	case 0, 1, 2:
		return "error"
	case 3:
		return "error"
	case 4:
		return "warn"
	case 5:
		return "info"
	case 6:
		return "info"
	case 7:
		return "debug"
	default:
		return "info"
	}
}

// decodeLoki: {"streams":[{"stream":{labels}, "values":[[ts,line], ...]}]}.
func decodeLoki(body []byte, ws *warningSet) []Record {
	var push struct {
		Streams []struct {
			Stream map[string]string
			Values [][]string // Loki sends [ts, line, ?metadata] arrays.
		}
	}
	if json.Unmarshal(body, &push) != nil {
		ws.add(WarnUnparseable)
		return nil
	}
	var out []Record
	for _, s := range push.Streams {
		for _, pair := range s.Values {
			r := Record{Attrs: map[string]string{}}
			switch {
			case len(pair) >= 2:
				r.Message = pair[1]
				r.Time = parseLokiTS(pair[0])
			case len(pair) == 1:
				r.Message = pair[0]
			default:
				continue
			}
			for k, val := range s.Stream {
				switch k {
				case "level":
					r.Level = val
				case "service", "job":
					r.Service = val
				case "host", "instance":
					r.Host = val
				default:
					r.Attrs[k] = val
				}
			}
			out = append(out, r)
		}
	}
	return out
}

// decodeAlertmanager: array of alerts; each becomes a record whose message is
// the alertname and whose attrs carry the labels. Severity label → level.
func decodeAlertmanager(body []byte) []Record {
	var alerts []map[string]any
	if json.Unmarshal(body, &alerts) != nil {
		return nil
	}
	var out []Record
	for _, a := range alerts {
		r := Record{Attrs: map[string]string{}}
		if labels, ok := a["labels"].(map[string]any); ok {
			for k, v := range labels {
				val := toString(v)
				r.Attrs[k] = val
				if k == "alertname" {
					r.Message = val
				}
				if k == "severity" {
					r.Level = val
				}
			}
		}
		if r.Message == "" {
			r.Message = toString(a["status"])
		}
		if s, ok := a["startsAt"].(string); ok {
			r.Time = parseTime(s)
		}
		out = append(out, r)
	}
	return out
}

// decodeOTLP parses OTLP logs JSON (resourceLogs/scopeLogs/logRecords),
// best-effort.
func decodeOTLP(body []byte, ws *warningSet) []Record {
	var doc struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					TimeUnixNano string
					SeverityText string
					Body         map[string]any
					Attributes   []struct {
						Key   string
						Value map[string]any
					}
				}
			} `json:"logRecords"`
		} `json:"resourceLogs"`
	}
	if json.Unmarshal(body, &doc) != nil {
		ws.add(WarnUnparseable)
		return nil
	}
	var out []Record
	for _, rl := range doc.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				r := Record{Attrs: map[string]string{}}
				r.Level = lr.SeverityText
				if body := lr.Body; body != nil {
					r.Message = toString(body["stringValue"])
				}
				r.Time = parseUnixNano(lr.TimeUnixNano)
				for _, a := range lr.Attributes {
					if v, ok := a.Value["stringValue"]; ok {
						r.Attrs[a.Key] = toString(v)
					}
				}
				out = append(out, r)
			}
		}
	}
	return out
}

// decodeSentry reads the newline-delimited envelope: a JSON header line, then
// items; any item that looks like an event yields its message/exception.
func decodeSentry(body []byte) []Record {
	var out []Record
	for _, ln := range bytes.Split(body, []byte{'\n'}) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 || ln[0] != '{' {
			continue
		}
		var item map[string]any
		if json.Unmarshal(ln, &item) != nil {
			continue
		}
		r := Record{Attrs: map[string]string{}}
		if msg, ok := item["message"]; ok {
			r.Message = toString(msg)
		}
		if _, ok := item["exception"]; ok {
			r.Level = "error"
		}
		if lvl, ok := item["level"]; ok {
			r.Level = toString(lvl)
		}
		if r.Message != "" {
			out = append(out, r)
		}
	}
	return out
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatFloat(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// formatFloat avoids pulling strconv into the hot path; json.Marshal of a
// float64 already gives a canonical form, so reuse it.
func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// parseTime accepts RFC3339, RFC3339Nano and a few common variants.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseLokiTs parses Loki's nanosecond unix timestamp (string) or ms epoch.
func parseLokiTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Loki sends ns as a string of digits.
	var ns int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return parseTime(s) // fall back to a date layout
		}
		ns = ns*10 + int64(s[i]-'0')
	}
	// Distinguish ns (19 digits) from ms (13) from s (10) by magnitude.
	switch {
	case ns > 1e18:
		return time.Unix(0, ns).UTC()
	case ns > 1e15:
		return time.Unix(0, ns*1e6).UTC()
	case ns > 1e12:
		return time.Unix(0, ns*1e9).UTC()
	default:
		return time.Unix(ns, 0).UTC()
	}
}

func parseUnixNano(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	var ns int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return time.Time{}
		}
		ns = ns*10 + int64(s[i]-'0')
	}
	return time.Unix(0, ns).UTC()
}
