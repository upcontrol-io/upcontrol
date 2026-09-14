package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The crawler door's fixed answers carry the right status, the shared
// headers, and no em-dash.
func TestStaticPagesRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter)
		code int
		want string
	}{
		{"removed", removedPage, http.StatusGone, "removed at the site owner's request"},
		{"notFound", notFoundPage, http.StatusNotFound, "no status page at this address"},
	} {
		w := httptest.NewRecorder()
		tc.call(w)
		if w.Code != tc.code {
			t.Fatalf("%s = %d, want %d", tc.name, w.Code, tc.code)
		}
		if got := w.Header().Get("Vary"); got != "User-Agent" {
			t.Fatalf("%s Vary = %q", tc.name, got)
		}
		body := w.Body.String()
		if !strings.Contains(body, tc.want) {
			t.Fatalf("%s is missing %q", tc.name, tc.want)
		}
		if strings.Contains(body, "—") {
			t.Fatalf("%s carries an em-dash", tc.name)
		}
	}
}
