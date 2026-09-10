// Package targetkey computes the identity of a probe target: one URL is
// checked once for everybody, and the key is what makes two spellings of the
// same fetch land on the same row. Used by every door that creates or looks
// up a target; the SQL backfill in migration 009 mirrors it byte for byte.
package targetkey

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// sep joins the key's parts. It must be a byte Postgres text can store:
// chr(0) is rejected with SQLSTATE 54000, which is why this is \x1f (ASCII
// unit separator), never \x00. Changing it means changing migration 009's
// chr(31) in the same breath.
const sep = "\x1f"

// Website returns the key for a website target: the normalized URL plus the
// keyword. A URL that cannot be normalized falls back to the raw spelling so
// the door never fails on weird input; convergence is safe (a fresh target is
// minted on the next clean subscribe).
func Website(rawURL, keyword string) string {
	u := rawURL
	if n, err := NormalizeURL(rawURL); err == nil {
		u = n
	}
	return join("website", u, keyword)
}

// Heartbeat returns the private key of a heartbeat monitor: never shared,
// derived from the monitor's public id (canonical lowercase dashed uuid, the
// same text migration 009 builds with public_id::text).
func Heartbeat(monitorPublicID string) string {
	return join("heartbeat", strings.ToLower(monitorPublicID))
}

// NormalizeURL canonicalizes a check target: https when no scheme was typed,
// host lowercased and punycoded (idna; a weird host falls back to lowercase,
// the door never fails on it), leading www. stripped, the default port
// stripped, the fragment dropped, a bare trailing slash dropped. An empty
// path stays empty (https://datrade.io, not https://datrade.io/).
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("targetkey: empty url")
	}
	if !hasScheme(s) {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("targetkey: parse %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("targetkey: no host in %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("targetkey: no host in %q", raw)
	}
	if ascii, aerr := idna.Lookup.ToASCII(host); aerr == nil {
		host = strings.ToLower(ascii)
	}
	host = strings.TrimPrefix(host, "www.")

	hostPart := host
	if port := u.Port(); port != "" {
		switch {
		case scheme == "https" && port == "443", scheme == "http" && port == "80":
		default:
			hostPart = net.JoinHostPort(host, port)
		}
	}

	path := u.Path
	if path == "/" && u.RawQuery == "" {
		path = ""
	}
	out := scheme + "://" + hostPart + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// hasScheme mirrors the SQL backfill's regex: a scheme is letters/digits/+
// -/. followed by ://. Anything else gets https prepended, so example.com
// and example.com:8080 both parse as hosts, not as scheme + opaque.
func hasScheme(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	sch := strings.ToLower(s[:i])
	for j, c := range sch {
		first := j == 0
		switch {
		case c >= 'a' && c <= 'z':
		case !first && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// join builds kind<sep>part<sep>part with the separator stripped from every
// part first, so a \x1f smuggled into a URL or keyword can never make two
// different (url, keyword) pairs collide on one key.
func join(parts ...string) string {
	clean := make([]string, len(parts))
	for i, p := range parts {
		p = strings.ReplaceAll(p, sep, "")
		clean[i] = strings.ReplaceAll(p, "\x00", "")
	}
	return strings.Join(clean, sep)
}
