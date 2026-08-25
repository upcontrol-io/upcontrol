// Package guard is the SSRF frontier: every IP is checked against blocked
// ranges after DNS and on every redirect; blocked means refused, not an outage.
package guard

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrBlockedTarget is returned when the resolved IP falls in a blocked range.
var ErrBlockedTarget = errors.New("guard: target is in a blocked range")

// allowedPorts are the only ports a probe may connect to without explicit user
// configuration: 80, 443, and the scheme default.
var allowedPorts = map[string]bool{"": true, "80": true, "443": true}

// blockedNets is the list of IP ranges a probe may never reach. It is computed
// once at package init so CheckIP is a pure iteration with no allocation.
var blockedNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",     // RFC 1918 private
		"172.16.0.0/12",  // RFC 1918 private
		"192.168.0.0/16", // RFC 1918 private
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local (incl. cloud metadata 169.254.169.254)
		"0.0.0.0/8",      // unspecified / "this network"
		"100.64.0.0/10",  // CGNAT
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // Unique Local Addresses
		"fe80::/10",      // IPv6 link-local
		// ::ffff:0.0.0.0/96 is intentionally NOT listed: Go stores IPv4 as
		// IPv4-mapped IPv6, and the stdlib checks handle it via To4().
	} {
		_, n, _ := net.ParseCIDR(cidr)
		blockedNets = append(blockedNets, n)
	}
}

// CheckIP returns ErrBlockedTarget if the IP is in a blocked range. It is the
// hot path: called after every DNS resolve and on every redirect hop.
func CheckIP(ip net.IP) error {
	// Go's stdlib covers private, loopback, link-local, unspecified — but not
	// CGNAT or IPv4-mapped bypass, so we iterate our explicit list too.
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return ErrBlockedTarget
		}
	}
	// Belt-and-suspenders: the stdlib checks catch anything we missed.
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ErrBlockedTarget
	}
	return nil
}

// CheckResolvedIPs checks ALL IPs from a lookup; if ANY is blocked the target
// is rejected (a public+private domain could rotate to the private one).
func CheckResolvedIPs(ips []net.IP) error {
	for _, ip := range ips {
		if err := CheckIP(ip); err != nil {
			return err
		}
	}
	return nil
}

// CheckURL validates scheme (http/https) and port; it does NOT resolve DNS.
// The scheme/port check runs before DNS to reject bad targets early.
func CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("guard: bad URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("guard: scheme %q not allowed", u.Scheme)
	}
	if !allowedPorts[u.Port()] {
		return fmt.Errorf("guard: port %q not allowed", u.Port())
	}
	// Reject hostnames that are raw IPs in blocked ranges (without needing DNS).
	if host := u.Hostname(); host != "" {
		if ip := net.ParseIP(host); ip != nil {
			if err := CheckIP(ip); err != nil {
				return err
			}
		}
	}
	return nil
}

// AllowedRedirectURL checks a redirect target the same way CheckURL does,
// on every hop of a redirect chain.
func AllowedRedirectURL(rawURL string) error {
	return CheckURL(rawURL)
}
