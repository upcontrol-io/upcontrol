package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretKeyFromHexRejects(t *testing.T) {
	cases := []string{
		"",                                 // empty
		"nothex",                           // non-hex
		"00",                               // too short
		"00112233445566778899aabbccddeeff", // 16 bytes, wrong size
	}
	for _, in := range cases {
		if _, err := SecretKeyFromHex(in); err == nil {
			t.Errorf("expected error for %q, got nil", in)
		}
	}
}

func TestLoadDevDefaults(t *testing.T) {
	// In dev, required DB values may be absent; Load must succeed with defaults.
	t.Setenv("UC_ENVIRONMENT", "dev")
	t.Setenv("UC_SECRET_KEY_HEX", "")
	// Clear optional prod-only vars so a dirty env can't flip us into prod mode.
	_ = os.Unsetenv("UC_POSTGRES_URL")
	c, err := Load("ucapi")
	if err != nil {
		t.Fatalf("dev Load: %v", err)
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
}

func TestLoadEmailAgentConfig(t *testing.T) {
	t.Setenv("UC_ENVIRONMENT", "dev")
	t.Setenv("UC_SECRET_KEY_HEX", "")
	_ = os.Unsetenv("UC_EMAIL_URL")
	_ = os.Unsetenv("UC_EMAIL_API_KEY")
	_ = os.Unsetenv("UC_EMAIL_API_KEY_FILE")
	c, err := Load("ucapi")
	if err != nil {
		t.Fatalf("dev Load: %v", err)
	}
	if c.EmailURL != "" || c.EmailAPIKey != "" {
		t.Errorf("unset email config should be empty, got URL %q key %q", c.EmailURL, c.EmailAPIKey)
	}

	t.Setenv("UC_EMAIL_URL", "http://mail-agent:8080")
	t.Setenv("UC_EMAIL_API_KEY", "sekret")
	c, err = Load("ucapi")
	if err != nil {
		t.Fatalf("dev Load with email config: %v", err)
	}
	if c.EmailURL != "http://mail-agent:8080" {
		t.Errorf("EmailURL = %q, want http://mail-agent:8080", c.EmailURL)
	}
	if c.EmailAPIKey != "sekret" {
		t.Errorf("EmailAPIKey = %q, want sekret", c.EmailAPIKey)
	}
}

func TestLoadProdRequiresSecrets(t *testing.T) {
	t.Setenv("UC_ENVIRONMENT", "prod")
	_ = os.Unsetenv("UC_POSTGRES_URL")
	_ = os.Unsetenv("UC_NODE_TOKEN")
	_ = os.Unsetenv("UC_SECRET_KEY_HEX")
	_, err := Load("ucapi")
	if err == nil {
		t.Fatal("prod Load with no secrets should fail")
	}
	// Should mention both missing vars, not just the first.
	for _, want := range []string{"UC_POSTGRES_URL", "UC_NODE_TOKEN"} {
		if !contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestLoadProdBadSecretKeyHex(t *testing.T) {
	t.Setenv("UC_ENVIRONMENT", "prod")
	t.Setenv("UC_POSTGRES_URL", "postgres://x")
	t.Setenv("UC_NODE_TOKEN", "tok")
	t.Setenv("UC_SECRET_KEY_HEX", "tooshort")
	_, err := Load("ucapi")
	if err == nil || !contains(err.Error(), "UC_SECRET_KEY_HEX") {
		t.Fatalf("expected secret key hex error, got %v", err)
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}

func TestGetenvOrFile(t *testing.T) {
	var warns []string

	// Direct env var wins over *_FILE.
	t.Setenv("UC_TEST", "direct")
	t.Setenv("UC_TEST_FILE", "/nonexistent")
	if got := getenvOrFile("UC_TEST", &warns); got != "direct" {
		t.Errorf("direct: got %q", got)
	}

	// Fall back to *_FILE contents, trimmed.
	_ = os.Unsetenv("UC_TEST")
	tf, err := os.CreateTemp("", "secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tf.Name()) })
	if _, err := tf.WriteString("  fromfile\n"); err != nil {
		t.Fatal(err)
	}
	if err := tf.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UC_TEST_FILE", tf.Name())
	if got := getenvOrFile("UC_TEST", &warns); got != "fromfile" {
		t.Errorf("file: got %q", got)
	}
	if len(warns) != 0 {
		t.Errorf("readable file should not warn, got %v", warns)
	}

	// Neither set -> empty, still no warning: silence belongs to the var
	// being unset, not to a failed read.
	_ = os.Unsetenv("UC_TEST")
	_ = os.Unsetenv("UC_TEST_FILE")
	if got := getenvOrFile("UC_TEST", &warns); got != "" {
		t.Errorf("empty: got %q", got)
	}
	if len(warns) != 0 {
		t.Errorf("unset vars should not warn, got %v", warns)
	}
}

func TestGetenvOrFileWarnsOnEveryReadFailure(t *testing.T) {
	var warns []string

	// A directory: reading it fails on both Windows (access denied) and
	// Linux (is a directory): the shape compose mounts for a missing secret.
	t.Setenv("UC_TEST_FILE", t.TempDir())
	if got := getenvOrFile("UC_TEST", &warns); got != "" {
		t.Errorf("directory: got %q", got)
	}
	if len(warns) != 1 || !contains(warns[0], "UC_TEST_FILE") || !contains(warns[0], "cannot read") {
		t.Fatalf("want one warning naming UC_TEST_FILE, got %v", warns)
	}

	// A path that does not exist is the same operator error: *_FILE was set,
	// the secret should have been there.
	t.Setenv("UC_TEST_FILE", filepath.Join(t.TempDir(), "absent"))
	if got := getenvOrFile("UC_TEST", &warns); got != "" {
		t.Errorf("missing file: got %q", got)
	}
	if len(warns) != 2 {
		t.Fatalf("missing file should also warn, got %v", warns)
	}
}

func TestLoadProdProbeNeedsNoDatabase(t *testing.T) {
	// Invariant 1: ucprobe holds no database credentials, so prod validation
	// must not demand them from it — only the node token.
	t.Setenv("UC_ENVIRONMENT", "prod")
	_ = os.Unsetenv("UC_POSTGRES_URL")
	_ = os.Unsetenv("UC_SECRET_KEY_HEX")
	t.Setenv("UC_NODE_TOKEN", "tok")
	if _, err := Load("ucprobe"); err != nil {
		t.Fatalf("prod ucprobe Load without DB creds: %v", err)
	}
	_ = os.Unsetenv("UC_NODE_TOKEN")
	if _, err := Load("ucprobe"); err == nil {
		t.Fatal("prod ucprobe Load without node token should fail")
	}
}

// The scrub switch is security-relevant: it is honoured on a self-hosted box
// and MUST be refused on the hosted service, where the tokens being redacted
// belong to other people. If the gate is ever dropped, this fails.
func TestScrubSwitchNeedsSelfHosted(t *testing.T) {
	cases := []struct {
		name       string
		scrub      string
		selfHosted string
		wantOff    bool
		wantWarn   bool
	}{
		{"default scrubs", "", "", false, false},
		{"self-hosted may turn it off", "0", "1", true, false},
		{"hosted refuses and says so", "0", "", false, true},
		{"self-hosted default still scrubs", "", "1", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UC_ENVIRONMENT", "dev")
			t.Setenv("UC_SCRUB", tc.scrub)
			t.Setenv("UC_SELF_HOSTED", tc.selfHosted)
			c, err := Load("ucapi")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.ScrubOff != tc.wantOff {
				t.Errorf("ScrubOff = %v, want %v", c.ScrubOff, tc.wantOff)
			}
			warned := false
			for _, w := range c.Warnings {
				if strings.Contains(w, "UC_SCRUB") {
					warned = true
				}
			}
			if warned != tc.wantWarn {
				t.Errorf("UC_SCRUB warning present = %v, want %v (warnings: %v)", warned, tc.wantWarn, c.Warnings)
			}
		})
	}
}
