package analytics

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := Event{
		Name: "page_view", Path: "/pricing", Title: "Pricing",
		Referrer: "https://news.ycombinator.com/", UTMSource: "hn", UTMMedium: "referral",
		Props: map[string]string{"which": "header"},
	}
	if e := valid; !validate(e) {
		t.Fatal("a plain page_view must validate")
	}

	cases := []struct {
		name   string
		mutate func(*Event)
		why    string
	}{
		{"name_uppercase", func(e *Event) { e.Name = "Page_View" }, "name grammar is lowercase only"},
		{"name_space", func(e *Event) { e.Name = "page view" }, "names cannot contain spaces"},
		{"name_dash", func(e *Event) { e.Name = "page-view" }, "dash is not in the grammar"},
		{"name_empty", func(e *Event) { e.Name = "" }, "empty name"},
		{"name_too_long", func(e *Event) { e.Name = strings.Repeat("a", 65) }, "name cap is 64"},
		{"path_too_long", func(e *Event) { e.Path = "/" + strings.Repeat("p", 256) }, "path cap is 256"},
		{"title_too_long", func(e *Event) { e.Title = strings.Repeat("t", 129) }, "title cap is 128"},
		{"referrer_too_long", func(e *Event) { e.Referrer = "https://" + strings.Repeat("r", 505) }, "referrer cap is 512"},
		{"utm_source_too_long", func(e *Event) { e.UTMSource = strings.Repeat("s", 129) }, "utm cap is 128"},
		{"utm_medium_too_long", func(e *Event) { e.UTMMedium = strings.Repeat("m", 129) }, "utm cap is 128"},
		{"utm_campaign_too_long", func(e *Event) { e.UTMCampaign = strings.Repeat("c", 129) }, "utm cap is 128"},
		{"props_too_many", func(e *Event) {
			e.Props = map[string]string{}
			for i := 0; i < 17; i++ {
				e.Props["k"+strings.Repeat("x", 2)+string(rune('a'+i%26))] = "v"
			}
		}, "props cap is 16 keys"},
		{"prop_key_too_long", func(e *Event) { e.Props = map[string]string{strings.Repeat("k", 65): "v"} }, "prop key cap is 64"},
		{"prop_value_too_long", func(e *Event) { e.Props = map[string]string{"k": strings.Repeat("v", 201)} }, "prop value cap is 200"},
	}
	for _, c := range cases {
		e := valid // copy
		c.mutate(&e)
		e.sanitize()
		if validate(e) {
			t.Errorf("%s: event must be dropped (%s)", c.name, c.why)
		}
	}

	// 64-char name and 16 props are exactly at the cap: still valid.
	atCap := valid
	atCap.Name = strings.Repeat("n", 64)
	atCap.Props = map[string]string{}
	for i := 0; i < 16; i++ {
		atCap.Props["k"+string(rune('a'+i))] = "v"
	}
	if !validate(atCap) {
		t.Error("caps are inclusive: 64-char name and 16 props must validate")
	}
}

func TestSanitizeStripsControlChars(t *testing.T) {
	e := Event{Name: "page\x00_view", Title: "Pri\x1bcing", Props: map[string]string{"a\x02b": "v\x7f"}}
	e.sanitize()
	if e.Name != "page_view" {
		t.Errorf("control char not stripped from name: %q", e.Name)
	}
	if e.Title != "Pricing" {
		t.Errorf("control char not stripped from title: %q", e.Title)
	}
	if v, ok := e.Props["ab"]; !ok || v != "v" {
		t.Errorf("control chars not stripped from props: %#v", e.Props)
	}
	if !validate(e) {
		t.Error("a sanitized event must validate")
	}
}

func TestParseBody(t *testing.T) {
	// Valid + invalid mixed: the invalid ones drop individually, the request
	// keeps the valid remainder.
	body := `{"events":[
		{"name":"page_view","path":"/"},
		{"name":"BAD NAME"},
		{"name":"nav_click","props":{"to":"/pricing"}}
	]}`
	kept, dropped := ParseBody(bytes.NewReader([]byte(body)))
	if len(kept) != 2 || dropped != 1 {
		t.Fatalf("kept=%d dropped=%d, want 2/1", len(kept), dropped)
	}
	if kept[0].Name != "page_view" || kept[1].Name != "nav_click" {
		t.Errorf("kept wrong events: %+v", kept)
	}

	// Over the per-request cap: the first 20 survive, the rest count as dropped.
	var b bytes.Buffer
	b.WriteString(`{"events":[`)
	for i := 0; i < 25; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"evt"}`)
	}
	b.WriteString(`]}`)
	kept, dropped = ParseBody(&b)
	if len(kept) != 20 || dropped != 5 {
		t.Fatalf("kept=%d dropped=%d, want 20/5", len(kept), dropped)
	}

	// Malformed and oversized bodies yield zero events, never an error.
	if kept, dropped = ParseBody(strings.NewReader("{not json")); len(kept) != 0 || dropped != 0 {
		t.Errorf("malformed body: kept=%d dropped=%d, want 0/0", len(kept), dropped)
	}
	big := `{"events":[{"name":"page_view","title":"` + strings.Repeat("x", MaxBodyBytes) + `"}]}`
	if kept, dropped = ParseBody(strings.NewReader(big)); len(kept) != 0 || dropped != 0 {
		t.Errorf("oversized body: kept=%d dropped=%d, want 0/0", len(kept), dropped)
	}

	// Empty body: nothing to do.
	if kept, dropped = ParseBody(strings.NewReader("")); len(kept) != 0 || dropped != 0 {
		t.Errorf("empty body: kept=%d dropped=%d, want 0/0", len(kept), dropped)
	}
}

func TestVisitorCookieRoundTrip(t *testing.T) {
	tok := MintVisitorToken()
	if len(tok) != 32 {
		t.Fatalf("token length = %d, want 32 hex chars", len(tok))
	}
	for _, ch := range tok {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Fatalf("token %q is not lowercase hex", tok)
		}
	}
	if tok == MintVisitorToken() {
		t.Fatal("two minted tokens must differ")
	}

	r := httptest.NewRequest("POST", "/public/track", nil)
	if _, ok := VisitorToken(r); ok {
		t.Error("no cookie: VisitorToken must report absent")
	}
	r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	got, ok := VisitorToken(r)
	if !ok || got != tok {
		t.Fatalf("VisitorToken = %q %v, want %q true", got, ok, tok)
	}

	// A corrupt cookie is treated as no cookie, not as a lookup on garbage.
	for _, bad := range []string{"short", strings.Repeat("g", 32), "", strings.Repeat("a", 33)} {
		r := httptest.NewRequest("POST", "/public/track", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: bad})
		if _, ok := VisitorToken(r); ok {
			t.Errorf("cookie %q must not validate", bad)
		}
	}

	w := httptest.NewRecorder()
	SetVisitorCookie(w, tok, false)
	c := w.Result().Cookies()[0]
	if c.Name != CookieName || c.Value != tok || c.Path != "/" || !c.HttpOnly ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge != int(VisitorCookieTTL.Seconds()) {
		t.Errorf("cookie not per spec: %+v", c)
	}
	if c.Secure {
		t.Error("plain-HTTP request (secure=false) must not set the Secure flag")
	}
	w = httptest.NewRecorder()
	SetVisitorCookie(w, tok, true)
	if !w.Result().Cookies()[0].Secure {
		t.Error("TLS request (secure=true) must set the Secure flag")
	}
}

func TestScopeRoundTrip(t *testing.T) {
	r := httptest.NewRequest("POST", "/public/track", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) Chrome/126.0")
	r.AddCookie(&http.Cookie{Name: CookieName, Value: MintVisitorToken()})

	s := ScopeFromRequest(r)
	if len(s.Token) != 32 {
		t.Errorf("scope token = %q, want 32 hex chars", s.Token)
	}
	if s.IP != "203.0.113.9" {
		t.Errorf("scope IP = %q, want the first X-Forwarded-For entry", s.IP)
	}
	if s.UA == "" {
		t.Error("scope UA must carry the User-Agent")
	}

	ctx := WithScope(r.Context(), s)
	if ScopeFrom(ctx) != s {
		t.Error("ScopeFrom must return the attached scope")
	}
	if ScopeFrom(r.Context()) != nil {
		t.Error("a context without a scope must yield nil")
	}
}

func TestGeoResolvesCountry(t *testing.T) {
	geo, err := OpenGeo()
	if err != nil {
		t.Fatalf("embedded mmdb failed to open: %v", err)
	}

	// A well-known anycast address: resolvable in every country database.
	if c := geo.Country("8.8.8.8"); c == "" {
		t.Error("8.8.8.8 must resolve to a country code")
	}
	// Private ranges are not in the database: unknown, not an error.
	for _, ip := range []string{"192.168.1.1", "127.0.0.1", "10.0.0.1", "", "not-an-ip"} {
		if c := geo.Country(ip); c != "" {
			t.Errorf("Country(%q) = %q, want \"\"", ip, c)
		}
	}
	// A nil Geo degrades to unknown instead of panicking.
	var nilGeo *Geo
	if c := nilGeo.Country("8.8.8.8"); c != "" {
		t.Errorf("nil Geo must answer \"\", got %q", c)
	}
}

func TestIPHashTruncatesToEightBytes(t *testing.T) {
	h := IPHash("203.0.113.9")
	if len(h) != 8 {
		t.Fatalf("hash length = %d, want 8", len(h))
	}
	full := sha256.Sum256([]byte("203.0.113.9"))
	if string(h[:]) != string(full[:8]) {
		t.Error("hash must be the first 8 bytes of sha256")
	}
	if IPHash("203.0.113.9") != h {
		t.Error("hash must be deterministic")
	}
	if IPHash("203.0.113.10") == h {
		t.Error("different IPs must hash differently")
	}
}
