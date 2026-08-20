package guard

import (
	"net"
	"testing"
)

func TestCheckIPBlockedRanges(t *testing.T) {
	blocked := []string{
		"10.0.0.1",               // RFC 1918
		"10.255.255.255",         // RFC 1918 edge
		"172.16.0.1",             // RFC 1918
		"172.31.255.255",         // RFC 1918 edge
		"192.168.1.1",            // RFC 1918
		"127.0.0.1",              // loopback
		"127.255.255.255",        // loopback edge
		"169.254.169.254",        // cloud metadata — the one that matters most
		"0.0.0.0",                // unspecified
		"100.64.0.1",             // CGNAT
		"::1",                    // IPv6 loopback
		"fc00::1",                // ULA
		"fd00::1",                // ULA
		"fe80::1",                // IPv6 link-local
		"::ffff:127.0.0.1",       // IPv4-mapped bypass attempt
		"::ffff:169.254.169.254", // IPv4-mapped metadata bypass
	}
	for _, ip := range blocked {
		if err := CheckIP(net.ParseIP(ip)); err == nil {
			t.Errorf("CheckIP(%q) should be blocked", ip)
		}
	}
}

func TestCheckIPAllowed(t *testing.T) {
	allowed := []string{
		"1.1.1.1",                  // Cloudflare DNS
		"8.8.8.8",                  // Google DNS
		"140.82.121.4",             // github.com
		"2606:4700:4700::1111",     // Cloudflare IPv6
		"2a00:1450:4001:824::200e", // google IPv6
	}
	for _, ip := range allowed {
		if err := CheckIP(net.ParseIP(ip)); err != nil {
			t.Errorf("CheckIP(%q) should be allowed, got %v", ip, err)
		}
	}
}

func TestCheckIPNotPrivateButAdjacentToPrivate(t *testing.T) {
	// 172.15.x.x is NOT private (private starts at 172.16) — must pass.
	if err := CheckIP(net.ParseIP("172.15.0.1")); err != nil {
		t.Errorf("172.15.0.1 should be allowed (not in 172.16/12), got %v", err)
	}
	// 172.32.x.x is NOT private (private ends at 172.31) — must pass.
	if err := CheckIP(net.ParseIP("172.32.0.1")); err != nil {
		t.Errorf("172.32.0.1 should be allowed, got %v", err)
	}
	// 11.0.0.1 is NOT in 10/8 — must pass.
	if err := CheckIP(net.ParseIP("11.0.0.1")); err != nil {
		t.Errorf("11.0.0.1 should be allowed, got %v", err)
	}
	// 100.63.x and 100.128.x are outside CGNAT (100.64/10).
	if err := CheckIP(net.ParseIP("100.63.0.1")); err != nil {
		t.Errorf("100.63.0.1 should be allowed (before CGNAT), got %v", err)
	}
	if err := CheckIP(net.ParseIP("100.128.0.1")); err != nil {
		t.Errorf("100.128.0.1 should be allowed (after CGNAT), got %v", err)
	}
}

func TestCheckResolvedIPsMixedBlocksAll(t *testing.T) {
	// A domain resolving to BOTH a public and a private IP must be rejected.
	ips := []net.IP{
		net.ParseIP("1.2.3.4"),         // public
		net.ParseIP("169.254.169.254"), // metadata — blocked
	}
	if err := CheckResolvedIPs(ips); err == nil {
		t.Error("mixed public+private should be blocked")
	}
}

func TestCheckResolvedIPsAllPublic(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("1.2.3.4"),
		net.ParseIP("5.6.7.8"),
	}
	if err := CheckResolvedIPs(ips); err != nil {
		t.Errorf("all-public should be allowed, got %v", err)
	}
}

func TestCheckURLScheme(t *testing.T) {
	bad := []string{"ftp://example.com", "file:///etc/passwd", "gopher://evil.com"}
	for _, u := range bad {
		if err := CheckURL(u); err == nil {
			t.Errorf("CheckURL(%q) should reject scheme", u)
		}
	}
}

func TestCheckURLBlockedIP(t *testing.T) {
	if err := CheckURL("http://169.254.169.254/"); err == nil {
		t.Error("metadata IP in URL should be blocked")
	}
	if err := CheckURL("http://10.0.0.1/"); err == nil {
		t.Error("private IP in URL should be blocked")
	}
}

func TestCheckURLAllowedPort(t *testing.T) {
	if err := CheckURL("https://example.com:443/"); err != nil {
		t.Errorf("port 443 should be allowed: %v", err)
	}
	if err := CheckURL("http://example.com:80/"); err != nil {
		t.Errorf("port 80 should be allowed: %v", err)
	}
}

func TestCheckURLBlockedPort(t *testing.T) {
	if err := CheckURL("https://example.com:8080/"); err == nil {
		t.Error("port 8080 should be blocked")
	}
	if err := CheckURL("http://example.com:22/"); err == nil {
		t.Error("port 22 should be blocked")
	}
}

func TestCheckURLValidPublic(t *testing.T) {
	for _, u := range []string{"https://example.com/", "http://example.com/path?q=1"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("CheckURL(%q) should pass: %v", u, err)
		}
	}
}
