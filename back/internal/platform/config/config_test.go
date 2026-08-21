package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	// Linux (is a directory). This is what compose mounts when the backing
	// secret file is missing, so it is the common production shape.
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

func TestLoadAIKeyFromFile(t *testing.T) {
	t.Setenv("UC_ENVIRONMENT", "dev")
	_ = os.Unsetenv("UC_AI_API_KEY")
	tf, err := os.CreateTemp("", "aikey")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tf.Name()) })
	if _, err := tf.WriteString("  sk-fromfile\n"); err != nil {
		t.Fatal(err)
	}
	if err := tf.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("UC_AI_API_KEY_FILE", tf.Name())
	c, err := Load("ucapi")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AIAPIKey != "sk-fromfile" {
		t.Errorf("AIAPIKey = %q, want sk-fromfile", c.AIAPIKey)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("readable key file should not warn, got %v", c.Warnings)
	}

	// The direct var beats the file.
	t.Setenv("UC_AI_API_KEY", "sk-direct")
	c, err = Load("ucapi")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AIAPIKey != "sk-direct" {
		t.Errorf("AIAPIKey = %q, want sk-direct", c.AIAPIKey)
	}
}

func TestLoadAIDefaults(t *testing.T) {
	// No AI vars set: every field falls back, and an absent key is not a
	// boot error — Explain simply stays off until a key arrives.
	t.Setenv("UC_ENVIRONMENT", "dev")
	for _, k := range []string{
		"UC_AI_BASE_URL", "UC_AI_API_KEY", "UC_AI_API_KEY_FILE",
		"UC_AI_MODEL", "UC_AI_TIMEOUT",
		"UC_AI_INPUT_PRICE_PER_1M", "UC_AI_OUTPUT_PRICE_PER_1M",
	} {
		_ = os.Unsetenv(k)
	}
	c, err := Load("ucapi")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AIBaseURL != "https://api.openai.com/v1" {
		t.Errorf("AIBaseURL = %q, want default", c.AIBaseURL)
	}
	if c.AIAPIKey != "" {
		t.Errorf("AIAPIKey = %q, want empty", c.AIAPIKey)
	}
	if c.AIModel != "gpt-5-nano-2025-08-07" {
		t.Errorf("AIModel = %q, want default", c.AIModel)
	}
	if c.AITimeout != 60*time.Second {
		t.Errorf("AITimeout = %v, want 60s", c.AITimeout)
	}
	if c.AIInputPricePer1M != 0 || c.AIOutputPricePer1M != 0 {
		t.Errorf("prices = %v/%v, want 0/0", c.AIInputPricePer1M, c.AIOutputPricePer1M)
	}
}

func TestLoadAIValues(t *testing.T) {
	t.Setenv("UC_ENVIRONMENT", "dev")
	t.Setenv("UC_AI_BASE_URL", "https://gw.internal/v1")
	t.Setenv("UC_AI_API_KEY", "sk-test")
	t.Setenv("UC_AI_MODEL", "gpt-4o")
	t.Setenv("UC_AI_TIMEOUT", "90s")
	t.Setenv("UC_AI_INPUT_PRICE_PER_1M", "0.15")
	t.Setenv("UC_AI_OUTPUT_PRICE_PER_1M", "0.6")
	c, err := Load("ucapi")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AIBaseURL != "https://gw.internal/v1" {
		t.Errorf("AIBaseURL = %q", c.AIBaseURL)
	}
	if c.AIAPIKey != "sk-test" {
		t.Errorf("AIAPIKey = %q", c.AIAPIKey)
	}
	if c.AIModel != "gpt-4o" {
		t.Errorf("AIModel = %q", c.AIModel)
	}
	if c.AITimeout != 90*time.Second {
		t.Errorf("AITimeout = %v, want 90s", c.AITimeout)
	}
	if c.AIInputPricePer1M != 0.15 || c.AIOutputPricePer1M != 0.6 {
		t.Errorf("prices = %v/%v, want 0.15/0.6", c.AIInputPricePer1M, c.AIOutputPricePer1M)
	}
}

func TestLoadAIBadValuesFailLoud(t *testing.T) {
	// A malformed price or duration must surface in Load's aggregated error,
	// not silently price every call at $0.
	t.Setenv("UC_ENVIRONMENT", "dev")
	t.Setenv("UC_AI_INPUT_PRICE_PER_1M", "0,60") // comma decimal, a realistic paste
	_, err := Load("ucapi")
	if err == nil || !contains(err.Error(), "UC_AI_INPUT_PRICE_PER_1M") {
		t.Fatalf("expected UC_AI_INPUT_PRICE_PER_1M error, got %v", err)
	}

	t.Setenv("UC_AI_INPUT_PRICE_PER_1M", "0.15")
	t.Setenv("UC_AI_TIMEOUT", "soon")
	_, err = Load("ucapi")
	if err == nil || !contains(err.Error(), "UC_AI_TIMEOUT") {
		t.Fatalf("expected UC_AI_TIMEOUT error, got %v", err)
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
