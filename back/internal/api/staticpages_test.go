package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.upcontrol.io/back/internal/probe/executor"
)

// The static pages carry no data: they render from constants, print the
// executor's User-Agent string verbatim, and carry no em-dash.

func staticGet(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStaticPagesRender(t *testing.T) {
	static := NewStaticPages()
	mux := http.NewServeMux()
	mux.Handle("GET /bot", static)
	mux.Handle("GET /status/policy", static)

	bot := staticGet(mux, "/bot")
	if bot.Code != http.StatusOK {
		t.Fatalf("/bot = %d", bot.Code)
	}
	if !strings.Contains(bot.Body.String(), "<code>"+executor.UserAgent+"</code>") {
		t.Fatal("the bot page does not print the executor's User-Agent verbatim")
	}
	for k, want := range map[string]string{
		"Content-Type":  "text/html; charset=utf-8",
		"Vary":          "User-Agent",
		"Cache-Control": "public, max-age=60",
	} {
		if got := bot.Header().Get(k); got != want {
			t.Fatalf("bot %s = %q, want %q", k, got, want)
		}
	}

	pol := staticGet(mux, "/status/policy")
	if pol.Code != http.StatusOK {
		t.Fatalf("/status/policy = %d", pol.Code)
	}
	for _, want := range []string{"_upcontrol-remove.", "removal@upcontrol.io", "one business day", "not affiliated"} {
		if !strings.Contains(pol.Body.String(), want) {
			t.Fatalf("the policy page is missing %q", want)
		}
	}
	if strings.Contains(bot.Body.String(), "—") || strings.Contains(pol.Body.String(), "—") {
		t.Fatal("a static page carries an em-dash")
	}
}
