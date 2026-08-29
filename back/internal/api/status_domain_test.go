package api

import "testing"

// The credit line is a branding boundary, not a preference: a hosted tenant may
// store or submit showPoweredBy=false through any client and the page still
// publishes it, because a plan buys the page's address and nothing about the
// branding (owner decision, 2026-08-29). A self-hosted operator running the
// AGPL copy decides for themselves.
func TestPoweredByIgnoresTheClientUnlessSelfHosted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selfHosted bool
		stored     bool
		want       bool
	}{
		{"hosted honours nothing the client asks", false, false, true},
		{"hosted with the flag on is still on", false, true, true},
		{"self-hosted may take our name off", true, false, false},
		{"self-hosted may leave it on", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &writeAPI{selfHosted: tc.selfHosted}
			if got := h.poweredBy(statusPageConfig{ShowPoweredBy: tc.stored}); got != tc.want {
				t.Errorf("poweredBy = %v, want %v", got, tc.want)
			}
		})
	}
}

// Only a subdomain is servable: a bare apex cannot carry the CNAME the customer
// is told to create, and our own host would hijack the app itself.
func TestNormalizeStatusDomainAcceptsOnlyAServableSubdomain(t *testing.T) {
	t.Setenv("UC_PUBLIC_ORIGIN", "https://upcontrol.io")

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"status.example.com", "status.example.com"},
		{"  STATUS.Example.COM  ", "status.example.com"},
		{"https://status.example.com/uptime", "status.example.com"},
		{"status.example.com:8443", "status.example.com"},
		{"status.example.com.", "status.example.com"},
		{"", ""},
	} {
		got, err := normalizeStatusDomain(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeStatusDomain(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}

	for _, bad := range []string{
		"example.com",         // apex: nothing to CNAME
		"localhost",           // no dot at all
		"upcontrol.io",        // our own host
		"status.upcontrol.io", // a subdomain of ours
		"status..example.com", // empty label
		"-status.example.com", // label starts with a dash
		"status-.example.com", // label ends with a dash
		"stat us.example.com", // space inside a label
		"status.exam_ple.com", // underscore is not a hostname character
	} {
		if got, err := normalizeStatusDomain(bad); err == nil {
			t.Errorf("normalizeStatusDomain(%q) = %q, nil; want an error", bad, got)
		}
	}
}
