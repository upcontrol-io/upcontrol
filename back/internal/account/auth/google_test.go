package auth

// Tests for the Google door. Everything runs without a database: the checks
// under test happen before a person is ever looked up.

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A logger that keeps the suite output clean; the handler logs every refusal
// on purpose, and those lines are not what these tests are reading.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

const testClientID = "233728563388-test.apps.googleusercontent.com"

// idToken builds an unsigned JWT: the handler checks claims, not signatures,
// so signing here would test a step the code does not take.
func idToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

func liveClaims() map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            testClientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"email":          "ada@example.com",
		"email_verified": true,
		"name":           "Ada Lovelace",
	}
}

// googleWith builds a handler pointed at a fake token endpoint. The endpoint
// records the form it was posted so the exchange itself can be asserted.
func googleWith(t *testing.T, handler http.HandlerFunc) *Google {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	g := NewGoogle(nil, nil, testClientID, "secret", []string{"http://localhost/sign-in"}, true, nil, discardLogger())
	g.tokenURL = ts.URL
	return g
}

// jsonPost sets the header that goes with the body shape below; without it
// the cross-site guard refuses, which is its job.
func jsonPost(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/google", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func postGoogle(h *Google, body string) *httptest.ResponseRecorder {
	req := jsonPost(body)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func errCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %s", rr.Body.String())
	}
	return body.Error.Code
}

func TestGoogleUnconfiguredAnswers503AndNamesItself(t *testing.T) {
	g := NewGoogle(nil, nil, "", "", nil, true, nil, discardLogger())
	if g.Configured() {
		t.Fatal("Configured() with no client id must be false")
	}
	rr := postGoogle(g, `{"code":"x","redirect_uri":"http://localhost/sign-in"}`)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
	if got := errCode(t, rr); got != "google_not_configured" {
		t.Errorf("error code = %q, want google_not_configured", got)
	}
}

func TestGoogleRefusesAnUnpublishedRedirectURI(t *testing.T) {
	// The parameter is attacker-controlled; the allowlist stops a code being
	// exchanged against a redirect this deployment never published.
	var called bool
	g := googleWith(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	rr := postGoogle(g, `{"code":"x","redirect_uri":"https://evil.example/sign-in"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rr.Code)
	}
	if got := errCode(t, rr); got != "bad_redirect_uri" {
		t.Errorf("error code = %q, want bad_redirect_uri", got)
	}
	if called {
		t.Error("the token endpoint was called for a redirect_uri we never published")
	}
}

func TestGoogleSendsTheCodeAndTheVerifierToTheTokenEndpoint(t *testing.T) {
	var form url.Values
	g := googleWith(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"` + idToken(t, liveClaims()) + `"}`))
	})
	// exchange() directly: what follows a successful exchange needs a database,
	// and the form this test is about is built before any of that.
	tok, err := g.exchange(t.Context(), googleReq{
		Code:         "the-code",
		RedirectURI:  "http://localhost/sign-in",
		CodeVerifier: "v3rif13r",
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok == "" {
		t.Fatal("exchange returned no id_token")
	}
	if form == nil {
		t.Fatal("the token endpoint was never called")
	}
	if got := form.Get("grant_type"); got != "authorization_code" {
		t.Errorf("grant_type = %q", got)
	}
	if got := form.Get("code"); got != "the-code" {
		t.Errorf("code = %q", got)
	}
	if got := form.Get("client_secret"); got != "secret" {
		t.Errorf("client_secret = %q", got)
	}
	if got := form.Get("code_verifier"); got != "v3rif13r" {
		t.Errorf("code_verifier = %q, want it forwarded for PKCE", got)
	}
	if got := form.Get("redirect_uri"); got != "http://localhost/sign-in" {
		t.Errorf("redirect_uri = %q", got)
	}
}

func TestGoogleOmitsTheVerifierWhenThePageSentNone(t *testing.T) {
	// A caller that sent no verifier must not have an empty one forwarded,
	// which Google rejects outright.
	var form url.Values
	g := googleWith(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"id_token":"` + idToken(t, liveClaims()) + `"}`))
	})
	if _, err := g.exchange(t.Context(), googleReq{Code: "c", RedirectURI: "http://localhost/sign-in"}); err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, present := form["code_verifier"]; present {
		t.Error("code_verifier was sent as an empty value")
	}
}

func TestGoogleTurnsEveryExchangeFailureIntoOneAnswer(t *testing.T) {
	// A refused code, a spent code and a Google outage must be indistinguishable
	// from outside: anything finer is an oracle.
	g := googleWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
	})
	rr := postGoogle(g, `{"code":"spent","redirect_uri":"http://localhost/sign-in"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rr.Code)
	}
	if got := errCode(t, rr); got != "invalid_token" {
		t.Errorf("error code = %q, want invalid_token", got)
	}
}

func TestGoogleVerifyRejectsEveryBadClaim(t *testing.T) {
	g := NewGoogle(nil, nil, testClientID, "secret", []string{"http://localhost/sign-in"}, true, nil, discardLogger())
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"another application's audience", func(c map[string]any) { c["aud"] = "someone-else.apps.googleusercontent.com" }},
		{"an issuer that is not Google", func(c map[string]any) { c["iss"] = "https://accounts.evil.example" }},
		{"an expired token", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"an unverified email", func(c map[string]any) { c["email_verified"] = false }},
		{"no email at all", func(c map[string]any) { delete(c, "email") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := liveClaims()
			tc.mutate(claims)
			if _, err := g.verify(idToken(t, claims)); err == nil {
				t.Fatalf("verify accepted a token with %s", tc.name)
			}
		})
	}
	// The control: the same builder with nothing mutated must pass, or every
	// case above would "pass" for the wrong reason.
	claims, err := g.verify(idToken(t, liveClaims()))
	if err != nil {
		t.Fatalf("verify rejected a good token: %v", err)
	}
	if claims.Email != "ada@example.com" {
		t.Errorf("email = %q", claims.Email)
	}
}

func TestGoogleVerifyRejectsMalformedTokens(t *testing.T) {
	g := NewGoogle(nil, nil, testClientID, "secret", []string{"http://localhost/sign-in"}, true, nil, discardLogger())
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.b.c.d", "x." + "!!!not-base64!!!" + ".z"} {
		if _, err := g.verify(bad); err == nil {
			t.Errorf("verify accepted %q", bad)
		}
	}
}

func TestGoogleConfiguredNeedsAllThreeParts(t *testing.T) {
	// A client id with no secret cannot complete an exchange; both must read as
	// unconfigured rather than fail later with an unexplained 401.
	uris := []string{"http://localhost/sign-in"}
	if NewGoogle(nil, nil, "id", "", uris, true, nil, discardLogger()).Configured() {
		t.Error("no secret must not read as configured")
	}
	if NewGoogle(nil, nil, "", "secret", uris, true, nil, discardLogger()).Configured() {
		t.Error("no client id must not read as configured")
	}
	if NewGoogle(nil, nil, "id", "secret", nil, true, nil, discardLogger()).Configured() {
		t.Error("no redirect uri must not read as configured")
	}
	if !NewGoogle(nil, nil, "id", "secret", uris, true, nil, discardLogger()).Configured() {
		t.Error("all three present must read as configured")
	}
}

func TestGoogleDropsBlankRedirectEntries(t *testing.T) {
	// A trailing comma in the env var would otherwise leave "" in the list,
	// and "" matches a caller that sent no redirect_uri at all.
	g := NewGoogle(nil, nil, "id", "secret", []string{"http://localhost/sign-in", "", "  "}, true, nil, discardLogger())
	if len(g.redirects) != 1 {
		t.Fatalf("redirects = %q, want the blanks dropped", g.redirects)
	}
	if g.allowedRedirect("") {
		t.Error("an empty redirect_uri must never be allowed")
	}
}

// The door installs a session, so a code cross-site POSTed here would sign the
// reader into the attacker's tenant; the guard cannot be skipped by the page.

const goodBody = `{"code":"c","redirect_uri":"http://localhost/sign-in"}`

func TestGoogleRefusesACrossSitePost(t *testing.T) {
	g := googleWith(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the token endpoint was reached by a cross-site request")
	})
	for _, site := range []string{"cross-site", "same-site", "none"} {
		t.Run(site, func(t *testing.T) {
			req := jsonPost(goodBody)
			req.Header.Set("Sec-Fetch-Site", site)
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 for Sec-Fetch-Site: %s", rr.Code, site)
			}
		})
	}
}

func TestGoogleRefusesAFormEncodedPost(t *testing.T) {
	// A cross-site HTML form can send only three encodings; text/plain is the
	// one whose body parses as JSON. application/json forces a CORS preflight.
	g := googleWith(t, func(http.ResponseWriter, *http.Request) {
		t.Error("the token endpoint was reached by a form post")
	})
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/google", strings.NewReader(goodBody))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("Content-Type %q: code = %d, want 403", ct, rr.Code)
		}
	}
}

func TestGoogleAllowsThePageAndTheProgram(t *testing.T) {
	// same-origin is the page; absent Sec-Fetch-Site is a non-browser caller
	// with no ambient session. The charset parameter must survive too.
	for _, site := range []string{"same-origin", ""} {
		var reached bool
		g := googleWith(t, func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusBadRequest)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/google", strings.NewReader(goodBody))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, req)
		if !reached {
			t.Errorf("Sec-Fetch-Site %q was refused before the exchange", site)
		}
	}
}
