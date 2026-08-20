// Package decode turns an arbitrary POST /i body into a stream of unified
// Records. The body's form is decided by a SNIFFER, not a header (plan §4.5):
// a misconfigured Content-Type must not lose the customer's logs. Eight forms
// are recognized — NDJSON, JSON array, JSON object, plain text, syslog RFC 5424,
// OTLP logs JSON, Sentry envelope, Alertmanager, Loki push — and Decode always
// returns 2xx on sane input, carrying structured warnings rather than errors.
//
// Records produced here are tenant- and seq-less: those are added after
// authentication by the ingest layer. decode only normalizes the wire form.
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

// WarningCode is one entry in the receipt's closed warning dictionary (plan §4.5).
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

// Record is the unified shape every format decodes to. Time is zero when the
// source carried no timestamp (the caller stamps now and warns ts_absent).
// Raw is the re-marshalled JSON object when the wire form was one of the JSON
// forms (ndjson/array/object) — the metric sniffer re-reads it so a metric
// line is recognised from its own fields, not from stringified attrs.
type Record struct {
	Time    time.Time
	Level   string
	Service string
	Host    string
	Message string
	Attrs   map[string]string
	Raw     []byte
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

// Result is what Decode returns: the records, the detected format (for the
// receipt), and the warning tally. err is non-nil only on truly unparseable
// input (none of the forms matched); the plan's "always 2xx" rule means the
// caller treats err as a single unparseable_line warning, not a 4xx.
type Result struct {
	Records  []Record
	Format   Format
	Warnings []Warning
}

// Sniff decides the body's form from its bytes, with Content-Type as a hint only.
// It never returns Unknown for non-empty input: the worst case is plain text.
func Sniff(body []byte, contentType string) Format {
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
			// NDJSON: multi-line where the first line is a complete JSON object.
			// We do NOT require every line to parse — a garbled line in a stream
			// is a warning at decode time, not a reason to call the whole body a
			// single (and unparseable) object.
			if hasManyLines(trimmed) {
				return FormatNDJSON
			}
			// Single object on one line.
			return FormatJSONObject
		}
		// First line starts with '{' but does not parse. If the body is
		// multi-line, assume the sender intended JSON-per-line (NDJSON): the
		// malformed line becomes an unparseable_line warning at decode, the good
		// lines are kept. A single malformed line is treated as a bad object.
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
	format := Sniff(body, contentType)
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

// normalizeLevel maps a free-form level to info|warn|error|debug. Unknown levels
// default to info and raise level_unknown (the receipt's job to explain).
func normalizeLevel(r *Record, ws *warningSet) {
	if r.Level == "" {
		r.Level = "info"
		return
	}
	switch strings.ToLower(r.Level) {
	case "info", "warn", "warning", "error", "err", "debug", "trace":
		// keep (collapse warning/warn, err/error)
	default:
		ws.add(WarnLevelUnknown)
		r.Level = "info"
	}
}

// --- shared helpers -----------------------------------------------------

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

// --- per-format decoders -----------------------------------------------

// genericLog is the common JSON log shape: {ts,time,timestamp, level, service,
// svc, msg, message, host, ...rest as attrs}.
func recordsFromObject(obj map[string]any) []Record {
	r := Record{Attrs: map[string]string{}}
	// The re-marshalled object travels on Raw so the ingest layer can recognise
	// a metric line from its own fields (metric+value), not from the stringified
	// attrs — labels would be mangled by toString.
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
		default:
			r.Attrs[k] = toString(v)
		}
	}
	if r.Message == "" {
		// A JSON object with no message is still a record; the raw line is the
		// best text we have.
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
	// A JSON object whose top level is an {"event": "..."} payload is treated as
	// a single event record (the SDK's /v1/event shape) — message is the event.
	if ev, ok := obj["event"]; ok && len(obj) <= 4 {
		r := Record{Attrs: map[string]string{}}
		r.Message = toString(ev)
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

// decodeOTLP: {"resourceLogs":[{"resource":{attributes:[...]}, "scopeLogs":
// [{"logRecords":[{"timeUnixNano":"...","severityText":"...","body":{...},
// "attributes":[...]}]}]}]}. Best-effort; OTLP's nested shape is verbose.
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

// decodeSentry: newline-delimited; first line is a JSON envelope header, the
// rest are items (each a JSON object, possibly with a payload). We extract the
// "message"/"exception" from any item that looks like an event.
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

// --- value coercion ----------------------------------------------------

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return trimFloat(t)
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

func trimFloat(f float64) string {
	s := strings.TrimRight(strings.TrimRight(
		strings.TrimPrefix(
			strings.TrimRight(
				strings.TrimRight(formatFloat(f), "0"), "."), ""), " "), " ")
	if s == "" || s == "-" {
		return "0"
	}
	return s
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
