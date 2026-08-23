package mailer

import (
	"strings"
	"testing"
)

func TestRenderCodeCarriesAWorkingLink(t *testing.T) {
	subject, body := RenderCode("ada@example.com", "1ece76e3", "https://upcontrol.io")
	if subject == "" {
		t.Fatal("subject is empty")
	}
	// The link format SignIn already redeems on mount, which is what makes this
	// testable before any mail is sent.
	want := "https://upcontrol.io/sign-in?email=ada%40example.com&token=1ece76e3"
	if !strings.Contains(body, want) {
		t.Fatalf("body does not carry the sign-in link\nwant substring: %s\ngot:\n%s", want, body)
	}
	// The bare code too: a link that a mail client mangles still leaves a way in.
	if !strings.Contains(body, "1ece76e3") {
		t.Fatal("body does not carry the bare code")
	}
}

func TestBuildMessageCarriesTheHeaders(t *testing.T) {
	msg := string(buildMessage("no-reply@upcontrol.io", "UpControl", "ada@example.com", "[down] api", "body line"))
	for _, want := range []string{
		"From: UpControl <no-reply@upcontrol.io>\r\n",
		"To: ada@example.com\r\n",
		"Subject: [down] api\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n\r\nbody line",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message misses %q\ngot:\n%s", want, msg)
		}
	}
	// No display name: the From header is the bare address, not "<addr>".
	if msg := string(buildMessage("no-reply@upcontrol.io", "", "a@b.c", "s", "t")); !strings.Contains(msg, "From: no-reply@upcontrol.io\r\n") {
		t.Fatalf("bare-address From header wrong:\n%s", msg)
	}
}

func TestNewSMTPRefusesAnIncompleteConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Host: "", From: "hi@upcontrol.io"},
		{Host: "smtp.example.com", From: ""},
	} {
		if _, err := NewSMTP(cfg, nil); err == nil {
			t.Fatalf("NewSMTP(%+v) returned no error; a half-configured mailer that "+
				"silently drops mail is worse than none", cfg)
		}
	}
}
