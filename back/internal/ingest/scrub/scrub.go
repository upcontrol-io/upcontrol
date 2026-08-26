// Package scrub strips secrets from a log line, marking each [redacted:TYPE:LEN].
// Hand-written scanner, not regex; the server re-scrubs what the SDK already scrubbed.
package scrub

import (
	"strconv"
	"strings"
)

// Result is the scrubber's output: the cleaned string and how many of each type
// were removed.
type Result struct {
	Cleaned string
	Counts  map[string]int
}

// Scrub returns the cleaned line and per-type counts. It is safe for concurrent
// use (no shared state).
func Scrub(s string) Result {
	r := Result{Counts: map[string]int{}}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if m, ok := matchAt(s, i); ok {
			b.WriteString("[redacted:")
			b.WriteString(m.kind)
			b.WriteByte(':')
			b.WriteString(strconv.Itoa(m.end - m.start))
			b.WriteByte(']')
			r.Counts[m.kind]++
			i = m.end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	r.Cleaned = b.String()
	return r
}

type match struct {
	start, end int
	kind       string
}

type matcher struct {
	name   string
	fn     func(s string, i int) (end int, ok bool)
	starts func(c byte) bool // true if this matcher could begin on byte c
}

var matchers = []matcher{
	{name: "pem", fn: pemMatcher, starts: func(c byte) bool { return c == '-' }},
	{name: "bearer", fn: bearerMatcher, starts: func(c byte) bool { return c == 'B' }},
	{name: "stripe", fn: prefixTokenFn("sk_live_", 16), starts: func(c byte) bool { return c == 's' }},
	{name: "github", fn: githubAnyMatcher, starts: func(c byte) bool { return c == 'g' }},
	{name: "slack", fn: slackAnyMatcher, starts: func(c byte) bool { return c == 'x' }},
	{name: "aws", fn: prefixTokenFn("AKIA", 16), starts: func(c byte) bool { return c == 'A' }},
	{name: "google", fn: prefixTokenFn("AIza", 32), starts: func(c byte) bool { return c == 'A' }},
	{name: "jwt", fn: jwtMatcher, starts: isB64},
	{name: "conn", fn: connStrMatcher, starts: func(c byte) bool {
		return c == 'p' || c == 'm' || c == 'r'
	}},
	{name: "cookie", fn: cookieSetMatcher, starts: func(c byte) bool { return c == 'S' }},
	{name: "session", fn: sessionEqMatcher, starts: func(c byte) bool { return c == 's' }},
	{name: "card", fn: cardLuhnMatcher, starts: isDigit},
	{name: "email", fn: emailMatcher, starts: isEmailLocal},
}

// dispatch maps a first byte to the matcher indices that could begin on it;
// most bytes map to nothing, so the common position is one lookup, no calls.
var dispatch [256][]int

func init() {
	for idx, m := range matchers {
		for b := 0; b < 256; b++ {
			if m.starts(byte(b)) {
				dispatch[b] = append(dispatch[b], idx)
			}
		}
	}
}

func matchAt(s string, i int) (match, bool) {
	for _, idx := range dispatch[s[i]] {
		m := &matchers[idx]
		if end, ok := m.fn(s, i); ok {
			return match{start: i, end: end, kind: m.name}, true
		}
	}
	return match{}, false
}

func pemMatcher(s string, i int) (int, bool) {
	const begin = "-----BEGIN "
	if !strings.HasPrefix(s[i:], begin) {
		return 0, false
	}
	hdr := s[i:]
	if !strings.Contains(hdr, "PRIVATE KEY") {
		nl := strings.IndexByte(hdr, '\n')
		if nl < 0 || !strings.Contains(hdr[:nl], "PRIVATE KEY") {
			return 0, false
		}
	}
	const endMarker = "-----END "
	end := strings.Index(s[i:], endMarker)
	if end < 0 {
		return 0, false
	}
	tail := i + end
	nl := strings.IndexByte(s[tail:], '\n')
	if nl < 0 {
		return len(s), true
	}
	return tail + nl, true
}

func bearerMatcher(s string, i int) (int, bool) {
	const p = "Bearer "
	if !strings.HasPrefix(s[i:], p) {
		return 0, false
	}
	j := i + len(p)
	for j < len(s) && s[j] != ' ' && s[j] != '\n' && s[j] != '\r' && s[j] != '"' && s[j] != '\'' && s[j] != ',' {
		j++
	}
	if j-i-len(p) < 12 {
		return 0, false
	}
	return j, true
}

func prefixTokenFn(prefix string, minTail int) func(string, int) (int, bool) {
	return func(s string, i int) (int, bool) {
		if !strings.HasPrefix(s[i:], prefix) {
			return 0, false
		}
		j := i + len(prefix)
		for j < len(s) && isTokenByte(s[j]) {
			j++
		}
		if j-i-len(prefix) < minTail {
			return 0, false
		}
		return j, true
	}
}

func isTokenByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-'
}

func githubAnyMatcher(s string, i int) (int, bool) {
	for _, p := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if strings.HasPrefix(s[i:], p) {
			j := i + len(p)
			for j < len(s) && isTokenByte(s[j]) {
				j++
			}
			if j-i >= 20 {
				return j, true
			}
		}
	}
	return 0, false
}

func slackAnyMatcher(s string, i int) (int, bool) {
	for _, p := range []string{"xoxp-", "xoxb-", "xoxa-", "xoxs-"} {
		if strings.HasPrefix(s[i:], p) {
			j := i + len(p)
			for j < len(s) && (isTokenByte(s[j]) || s[j] == '.') {
				j++
			}
			if j-i-len(p) >= 10 {
				return j, true
			}
		}
	}
	return 0, false
}

func jwtMatcher(s string, i int) (int, bool) {
	if !isB64(s[i]) {
		return 0, false
	}
	// Only at a token boundary: a JWT inside a larger b64 run is not standalone.
	if i > 0 && isB64(s[i-1]) {
		return 0, false
	}
	j := i
	dots := 0
	for j < len(s) && (isB64(s[j]) || s[j] == '.') {
		if s[j] == '.' {
			dots++
		}
		j++
	}
	if dots != 2 {
		return 0, false
	}
	d1 := strings.IndexByte(s[i:j], '.')
	if d1 < 0 {
		return 0, false
	}
	d1 += i
	d2 := strings.IndexByte(s[d1+1:j], '.')
	if d2 < 0 {
		return 0, false
	}
	d2 += d1 + 1
	if d1-i < 8 || d2-d1-1 < 8 || j-d2-1 < 8 {
		return 0, false
	}
	return j, true
}

func isB64(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '_' || c == '-'
}

func connStrMatcher(s string, i int) (int, bool) {
	for _, sch := range []string{"postgres://", "postgresql://", "mysql://", "mongodb://", "redis://"} {
		if strings.HasPrefix(s[i:], sch) {
			rest := s[i+len(sch):]
			colon := strings.IndexByte(rest, ':')
			if colon < 0 {
				continue
			}
			at := strings.IndexByte(rest[colon:], '@')
			if at < 0 {
				continue
			}
			return i + len(sch) + colon + at, true // stop at '@' — host is not secret
		}
	}
	return 0, false
}

func sessionEqMatcher(s string, i int) (int, bool) {
	const p = "session="
	if !strings.HasPrefix(s[i:], p) {
		return 0, false
	}
	j := i + len(p)
	for j < len(s) && s[j] != ' ' && s[j] != ';' && s[j] != '&' && s[j] != '\n' && s[j] != '"' && s[j] != '\'' {
		j++
	}
	if j-i-len(p) < 8 {
		return 0, false
	}
	return j, true
}

func cookieSetMatcher(s string, i int) (int, bool) {
	const p = "Set-Cookie: "
	if !strings.HasPrefix(s[i:], p) {
		return 0, false
	}
	j := i + len(p)
	eq := strings.IndexByte(s[j:], '=')
	if eq < 0 {
		return 0, false
	}
	v := j + eq + 1
	for v < len(s) && s[v] != ';' && s[v] != '\n' && s[v] != '\r' && s[v] != '"' {
		v++
	}
	if v-j-eq-1 < 8 {
		return 0, false
	}
	return v, true
}

func cardLuhnMatcher(s string, i int) (int, bool) {
	if !isDigit(s[i]) {
		return 0, false
	}
	if i > 0 && isDigit(s[i-1]) { // only at the start of a digit run
		return 0, false
	}
	var digits [20]byte
	n := 0
	j := i
	for j < len(s) && (isDigit(s[j]) || s[j] == ' ' || s[j] == '-') {
		if isDigit(s[j]) && n < len(digits) {
			digits[n] = s[j]
			n++
		}
		j++
	}
	if n < 13 || n > 19 {
		return 0, false
	}
	if !luhn(digits[:n]) {
		return 0, false
	}
	return j, true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// luhn validates a digit slice with the Luhn checksum.
func luhn(digits []byte) bool {
	sum := 0
	alt := false
	for k := len(digits) - 1; k >= 0; k-- {
		d := int(digits[k] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func emailMatcher(s string, i int) (int, bool) {
	if !isEmailLocal(s[i]) {
		return 0, false
	}
	if i > 0 && isEmailLocal(s[i-1]) { // only at a local-part boundary
		return 0, false
	}
	j := i
	for j < len(s) && isEmailLocal(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '@' {
		return 0, false
	}
	j++
	hostStart := j
	for j < len(s) && (isEmailLocal(s[j]) || s[j] == '.') {
		j++
	}
	if j-hostStart < 5 || !strings.Contains(s[hostStart:j], ".") {
		return 0, false
	}
	return j, true
}

func isEmailLocal(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '%' || c == '+' || c == '-'
}
