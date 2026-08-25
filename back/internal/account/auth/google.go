package auth

// Google sign-in, the authorization-code half: the code is exchanged
// server-side; redirect_uri, audience and email_verified are all checked.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/analytics"
	"go.upcontrol.io/back/internal/storage/pg"
)

// googleTokenURL is Google's token endpoint; NewGoogle copies it into the
// handler's tokenURL field, which tests may repoint.
const googleTokenURL = "https://oauth2.googleapis.com/token"

// The issuer Google stamps into an id_token. Both spellings are current and
// documented, so both are accepted and nothing else is.
var googleIssuers = map[string]bool{
	"accounts.google.com":         true,
	"https://accounts.google.com": true,
}

// Google handles POST /v1/auth/google.
type Google struct {
	pool       *pg.Pool
	sess       *session.Manager
	rec        *analytics.Recorder
	log        *slog.Logger
	devMode    bool
	selfHosted bool

	clientID     string
	clientSecret string
	// The exact redirect_uri values this deployment exchanges a code for; an
	// allowlist because the app is served from more than one origin.
	redirects []string

	tokenURL string
	http     *http.Client
}

// NewGoogle builds the handler. An empty clientID or clientSecret leaves it
// unconfigured, which answers 503, never a 200 that reads as a login.
func NewGoogle(p *pg.Pool, sm *session.Manager, clientID, clientSecret string, redirects []string, devMode bool, rec *analytics.Recorder, log *slog.Logger) *Google {
	if log == nil {
		log = slog.Default()
	}
	clean := make([]string, 0, len(redirects))
	for _, r := range redirects {
		if r = strings.TrimSpace(r); r != "" {
			clean = append(clean, r)
		}
	}
	return &Google{
		pool: p, sess: sm, rec: rec, log: log, devMode: devMode,
		clientID: clientID, clientSecret: clientSecret, redirects: clean,
		tokenURL: googleTokenURL,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// WithSelfHosted mirrors MagicLink's: a tenant created through this door lands
// on the same plan a tenant created through that one does.
func (h *Google) WithSelfHosted(v bool) *Google { h.selfHosted = v; return h }

// Configured reports whether this deployment can complete a Google sign-in.
func (h *Google) Configured() bool {
	return h != nil && h.clientID != "" && h.clientSecret != "" && len(h.redirects) > 0
}

type googleReq struct {
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier,omitempty"`
}

func (h *Google) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := analytics.WithScope(r.Context(), analytics.ScopeFromRequest(r))

	// This door installs a session, so it needs no victim cookie: a code
	// cross-site POSTed here would sign the reader into the attacker's tenant.
	if crossSitePost(r) {
		writeErr(w, http.StatusForbidden, "cross_site")
		return
	}

	if !h.Configured() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"code":    "google_not_configured",
				"message": "Google sign-in is not configured on this instance — use the magic link.",
			},
		})
		return
	}

	var req googleReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_body")
		return
	}
	if req.Code == "" {
		writeErr(w, http.StatusBadRequest, "missing_code")
		return
	}
	if !h.allowedRedirect(req.RedirectURI) {
		// The caller supplied this value, so refusing loudly is no oracle leak;
		// never hand Google a redirect_uri we never published.
		h.log.Warn("google: redirect_uri not allowed", "redirect_uri", req.RedirectURI)
		writeErr(w, http.StatusBadRequest, "bad_redirect_uri")
		return
	}

	idToken, err := h.exchange(ctx, req)
	if err != nil {
		// One error for every failure: the caller learns it did not work, and
		// nothing else.
		h.log.Warn("google: code exchange failed", "err", err)
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}
	claims, err := h.verify(idToken)
	if err != nil {
		h.log.Warn("google: id_token rejected", "err", err)
		writeErr(w, http.StatusUnauthorized, "invalid_token")
		return
	}

	// Normalised, or the same person through the two doors lands in two
	// accounts: the email column is UNIQUE and byte-exact.
	email := NormalizeEmail(claims.Email)
	door := &MagicLink{pool: h.pool, rec: h.rec, selfHosted: h.selfHosted}
	person, err := door.ensurePerson(ctx, email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Google verified the address, the same proof a magic-link redeem carries:
	// invites activate here, never on the request path.
	door.activateInvites(ctx, person.ID, email)
	sessToken, err := h.sess.Create(ctx, person.ID, person.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	session.SetCookie(w, sessToken, session.DefaultTTL, !h.devMode)
	h.rec.ServerEvent(ctx, "signed_in", person.ID, person.TenantID, nil)
	h.rec.LinkPerson(ctx, person.ID, person.TenantID)
	writeAccount(w, person)
}

func (h *Google) allowedRedirect(uri string) bool {
	for _, allowed := range h.redirects {
		if uri == allowed {
			return true
		}
	}
	return false
}

// exchange trades the one-time code for Google's token response and returns the
// raw id_token. The client secret leaves the process here and nowhere else.
func (h *Google) exchange(ctx context.Context, req googleReq) (string, error) {
	form := url.Values{
		"code":          {req.Code},
		"client_id":     {h.clientID},
		"client_secret": {h.clientSecret},
		"redirect_uri":  {req.RedirectURI},
		"grant_type":    {"authorization_code"},
	}
	// PKCE when the page sent a verifier. The code sits in a browser URL on the
	// way here, and the verifier is what makes a stolen one useless.
	if req.CodeVerifier != "" {
		form.Set("code_verifier", req.CodeVerifier)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("token response unreadable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Google's error body names the reason and carries no secret of ours;
		// it is the caller's input echoed back, never a token.
		var g struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &g)
		return "", fmt.Errorf("token endpoint %s: %s %s", resp.Status, g.Error, g.Desc)
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("token response is not JSON: %w", err)
	}
	if out.IDToken == "" {
		return "", fmt.Errorf("token response carried no id_token")
	}
	return out.IDToken, nil
}

type googleClaims struct {
	Iss           string `json:"iss"`
	Aud           string `json:"aud"`
	Exp           int64  `json:"exp"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

// verify checks an id_token received directly from Google's token endpoint over
// TLS. The signature is not checked (the channel authenticates it); claims are.
func (h *Google) verify(idToken string) (googleClaims, error) {
	var c googleClaims
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return c, fmt.Errorf("id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, fmt.Errorf("id_token payload is not base64url: %w", err)
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, fmt.Errorf("id_token payload is not JSON: %w", err)
	}
	if !googleIssuers[c.Iss] {
		return c, fmt.Errorf("issuer %q is not Google", c.Iss)
	}
	if c.Aud != h.clientID {
		// The check that matters most: without it, a token minted for any other
		// Google application would sign its bearer in here.
		return c, fmt.Errorf("audience is not this client")
	}
	if c.Exp > 0 && time.Now().After(time.Unix(c.Exp, 0)) {
		return c, fmt.Errorf("id_token has expired")
	}
	if c.Email == "" {
		return c, fmt.Errorf("id_token carries no email")
	}
	if !c.EmailVerified {
		// An unverified address is a claim about somebody else's mailbox.
		return c, fmt.Errorf("email is not verified")
	}
	return c, nil
}
