package mailer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAgentRefusesAnEmptyURL(t *testing.T) {
	if _, err := NewAgent("", "key", nil); err == nil {
		t.Fatal("NewAgent with an empty URL returned no error; a mailer pointed " +
			"at nothing is worse than none")
	}
}

func TestAgentSendCodePostsTheAgentContract(t *testing.T) {
	var (
		method string
		path   string
		ctype  string
		body   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body is not a JSON object: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	// The trailing slash must be normalized away, not doubled into the path.
	a, err := NewAgent(srv.URL+"/", "sekret", nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.WithSignInBase("https://upcontrol.io").
		SendCode(context.Background(), "ada@example.com", "1ece76e3"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/send" {
		t.Errorf("path = %q, want /send", path)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}
	// The exact body the agent's /send validates and renders magic-link from.
	if body["kind"] != "transactional" {
		t.Errorf("kind = %v, want transactional", body["kind"])
	}
	if body["template"] != "magic-link" {
		t.Errorf("template = %v, want magic-link", body["template"])
	}
	if body["to"] != "ada@example.com" {
		t.Errorf("to = %v, want ada@example.com", body["to"])
	}
	vars, ok := body["vars"].(map[string]any)
	if !ok {
		t.Fatalf("vars = %v, want an object", body["vars"])
	}
	if vars["code"] != "1ece76e3" {
		t.Errorf("vars.code = %v, want 1ece76e3", vars["code"])
	}
	if vars["sign_in_base"] != "https://upcontrol.io" {
		t.Errorf("vars.sign_in_base = %v, want https://upcontrol.io", vars["sign_in_base"])
	}
}

func TestAgentSendInvitePostsTheAgentContract(t *testing.T) {
	var (
		method string
		path   string
		ctype  string
		body   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body is not a JSON object: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	a, err := NewAgent(srv.URL, "sekret", nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.WithSignInBase("https://upcontrol.io").
		SendInvite(context.Background(), "kira@example.com", "1ece76e3", "acme.io", "Ada"); err != nil {
		t.Fatalf("SendInvite: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/send" {
		t.Errorf("path = %q, want /send", path)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}
	// The exact body the agent's /send validates and renders invite from:
	// `to` rides the envelope, the template owns the link, so no address is
	// rendered into the vars twice.
	if body["kind"] != "transactional" {
		t.Errorf("kind = %v, want transactional", body["kind"])
	}
	if body["template"] != "invite" {
		t.Errorf("template = %v, want invite", body["template"])
	}
	if body["to"] != "kira@example.com" {
		t.Errorf("to = %v, want kira@example.com", body["to"])
	}
	vars, ok := body["vars"].(map[string]any)
	if !ok {
		t.Fatalf("vars = %v, want an object", body["vars"])
	}
	if vars["code"] != "1ece76e3" {
		t.Errorf("vars.code = %v, want 1ece76e3", vars["code"])
	}
	if vars["sign_in_base"] != "https://upcontrol.io" {
		t.Errorf("vars.sign_in_base = %v, want https://upcontrol.io", vars["sign_in_base"])
	}
	if vars["project"] != "acme.io" {
		t.Errorf("vars.project = %v, want acme.io", vars["project"])
	}
	if vars["invited_by"] != "Ada" {
		t.Errorf("vars.invited_by = %v, want Ada", vars["invited_by"])
	}
}

func TestAgentBearerHeaderFollowsTheKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{name: "key set", key: "sekret", want: "Bearer sekret"},
		{name: "key empty sends no header", key: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			a, err := NewAgent(srv.URL, tc.key, nil)
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			if err := a.SendCode(context.Background(), "ada@example.com", "1ece76e3"); err != nil {
				t.Fatalf("SendCode: %v", err)
			}
			if auth != tc.want {
				t.Errorf("Authorization = %q, want %q", auth, tc.want)
			}
		})
	}
}

func TestAgentSendCodeFailsOnANon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "queue insert failed", http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := NewAgent(srv.URL, "", nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	err = a.SendCode(context.Background(), "ada@example.com", "1ece76e3")
	if err == nil {
		t.Fatal("SendCode on a 500 returned no error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should carry the status: %v", err)
	}
}

func TestAgentSendCodeFailsWhenTheServiceIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // every connection is refused from here on
	a, err := NewAgent(srv.URL, "", nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.SendCode(context.Background(), "ada@example.com", "1ece76e3"); err == nil {
		t.Fatal("SendCode against a stopped service returned no error")
	}
}
