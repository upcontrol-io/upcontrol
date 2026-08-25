//go:build integration

// Recipient-invite coverage for the address contract (§6.8), the
// invitation mail (Decision 16), the resend (Decision 17: the cooldown
// answers 429, a failed send changes nothing, only pending is a target) and
// Decision 19's server half (a pending address is no e-mail destination).
// The invite normalises before anything is stored, so an invite under one
// spelling and a sign-in under another cannot land in two person rows; a
// second invite of the same address adds no second membership; the mail is
// sent inside the write, so a send failure leaves no member row behind.
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/api/...
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// openRecipientsDB applies migrations and returns a WriteAPI over a fresh
// tenant, ready to receive invites. The inviter is a named, active person on
// the tenant — the way every real invite arrives, since ServeHTTP passes the
// session's person id through to the handler.
func openRecipientsDB(t *testing.T) (*WriteAPI, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping recipients integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "", "", "", "", "../../../db/postgres", ""); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var tenantID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("recipients-api-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	var inviterID int64
	if err := pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		fmt.Sprintf("inviter-%d@example.com", time.Now().UnixNano()), "Ada Inviter").Scan(&inviterID); err != nil {
		t.Fatalf("seed inviter: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		tenantID, inviterID); err != nil {
		t.Fatalf("seed inviter membership: %v", err)
	}
	return &WriteAPI{pool: pool}, tenantID, inviterID
}

// inviteMailer records the invitation, and can fail it the way an email agent
// that is down does. SendCode is here only because WriteAPI.mailer is the
// auth.Mailer the whole API shares; this path never calls it.
type inviteMailer struct {
	calls     int
	to        string
	code      string
	project   string
	invitedBy string
	err       error
}

func (m *inviteMailer) SendCode(context.Context, string, string) error { return nil }

func (m *inviteMailer) SendInvite(_ context.Context, to, code, project, invitedBy string) error {
	m.calls++
	m.to, m.code, m.project, m.invitedBy = to, code, project, invitedBy
	return m.err
}

// withFreshIP stamps a unique X-Forwarded-For on a synthesized request.
// IssueLoginCode meters a per-IP window shared with the sign-in door, and
// every httptest request would otherwise arrive from the same 192.0.2.1 —
// not only would two runs of this suite inside five minutes start failing
// as rate_limited, so would two resends in one test, for a reason no test
// here means to touch.
var inviteSeq int64

func withFreshIP(t *testing.T, r *http.Request) *http.Request {
	t.Helper()
	now := time.Now().UnixNano()
	inviteSeq++
	r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", (now>>24)&0xff, (now>>16)&0xff, inviteSeq&0xff))
	return r
}

func inviteRequest(t *testing.T, email string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/recipients",
		strings.NewReader(fmt.Sprintf(`{"email":%q,"role":"notify"}`, email)))
	return withFreshIP(t, r)
}

// An invite under mixed case must land as the one lower-case spelling: the
// person is found by the normalised address (the same lookup the sign-in doors
// use), exactly one row exists for it, and the account is named from the local
// part instead of the empty string the INSERT used to bake in.
func TestCreateRecipient_NormalisedAddressIsOnePerson(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, "Bob@Example.com"), tenantID, inviterID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body.String())
	}
	ctx := context.Background()
	email := "bob@example.com"
	person, err := h.pool.Queries().GetPersonByEmail(ctx, &email)
	if err != nil {
		t.Fatalf("GetPersonByEmail(bob@example.com): %v — the invite was stored under another spelling", err)
	}
	if person.Name != "bob" {
		t.Fatalf("person name = %q, want %q — NameFromEmail of the normalised address", person.Name, "bob")
	}
	var n int
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM person WHERE lower(email) = $1`, email).Scan(&n); err != nil || n != 1 {
		t.Fatalf("persons for that address = %d (err %v), want exactly 1", n, err)
	}
}

// The same address invited twice — the second invite under a different
// spelling, which is the case normalisation exists for — finds the person and
// adds no second membership row.
func TestCreateRecipient_SecondInviteAddsNoMembership(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)

	for _, addr := range []string{"Bob@Example.com", "bob@example.com"} {
		w := httptest.NewRecorder()
		h.createRecipient(w, inviteRequest(t, addr), tenantID, inviterID)
		if w.Code != http.StatusCreated {
			t.Fatalf("invite %q: status = %d (%s), want 201", addr, w.Code, w.Body.String())
		}
	}
	var n int
	if err := h.pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_member tm JOIN person p ON p.id = tm.person_id
		  WHERE tm.tenant_id = $1 AND p.email = 'bob@example.com'`, tenantID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("membership rows for that address = %d (err %v), want exactly 1", n, err)
	}
}

// An address with no @ in it is not an address: 400 bad_email, and nothing
// stored. The empty body takes the same refusal.
func TestCreateRecipient_BadEmailIsRefused(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)

	for _, body := range []string{`{"email":"not-an-address"}`, `{}`} {
		w := httptest.NewRecorder()
		h.createRecipient(w, httptest.NewRequest(http.MethodPost, "/v1/recipients", strings.NewReader(body)), tenantID, inviterID)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d (%s), want 400", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "bad_email") {
			t.Fatalf("body %s: response = %s, want error code bad_email", body, w.Body.String())
		}
	}
	var n int
	if err := h.pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FROM person WHERE email LIKE '%not-an-address%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("persons stored = %d (err %v), want 0 — a refused invite writes nothing", n, err)
	}
}

// §4: the e-mail channel leaves with the person. A removed member's address
// stops being a destination, not just a login — the delete would otherwise
// leave a channel that keeps mailing somebody the project just evicted. The
// bystander row (another address, same tenant) proves the match is by the
// person's own e-mail, not a blanket sweep of the tenant's e-mail channels.
func TestDeleteRecipient_EmailChannelLeavesWithThePerson(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, "gone@example.com"), tenantID, inviterID)
	if w.Code != http.StatusCreated {
		t.Fatalf("invite: status = %d (%s), want 201", w.Code, w.Body.String())
	}
	var personID int64
	ctx := context.Background()
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT id FROM person WHERE email = 'gone@example.com'`).Scan(&personID); err != nil {
		t.Fatalf("find the invited person: %v", err)
	}
	// The channel rows go in by SQL: the API's create path would only repeat
	// what the invite already proved, and the row under test is the delete's
	// input, not its output.
	for _, addr := range []string{"gone@example.com", "stays@example.com"} {
		if _, err := h.pool.Raw().Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, kind, target) VALUES (gen_random_uuid(), $1, 'email', $2)`,
			tenantID, addr); err != nil {
			t.Fatalf("seed email channel %s: %v", addr, err)
		}
	}

	w = httptest.NewRecorder()
	h.deleteRecipient(w, httptest.NewRequest(http.MethodDelete,
		"/v1/recipients/"+strconv.FormatInt(personID, 10), nil), tenantID)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d (%s), want 204", w.Code, w.Body.String())
	}
	var gone, stays int
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE target = 'gone@example.com'),
		        count(*) FILTER (WHERE target = 'stays@example.com')
		   FROM alert_channel WHERE tenant_id = $1 AND kind = 'email'`, tenantID).Scan(&gone, &stays); err != nil {
		t.Fatalf("count email channels: %v", err)
	}
	if gone != 0 {
		t.Fatalf("removed person's email channel rows = %d, want 0 — the address must stop being a destination", gone)
	}
	if stays != 1 {
		t.Fatalf("bystander email channel rows = %d, want 1 — only the removed person's channel goes", stays)
	}
}

// Decision 16: the invite sends the mail inside the write. A fresh invite
// calls SendInvite exactly once, with the normalised address, the inviter's
// person.name, and the tenant's own name as the project (this tenant has no
// project, so Decision 15's fallback must name it). Dev mode echoes the very
// code the mailer was handed.
func TestCreateRecipient_SendsOneInvitation(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{}
	h.devMode = true
	// A unique address per run: the database holds earlier runs' codes, and
	// the 60-second cooldown would otherwise turn the second run's "fresh"
	// invite into the no-mail branch this test must not take.
	local := fmt.Sprintf("kira%d", time.Now().UnixNano())

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, local+"@Example.com"), tenantID, inviterID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body.String())
	}
	m := h.mailer.(*inviteMailer)
	if m.calls != 1 {
		t.Fatalf("SendInvite calls = %d, want exactly 1", m.calls)
	}
	want := local + "@example.com"
	if m.to != want {
		t.Fatalf("invite sent to %q, want the normalised %q", m.to, want)
	}
	if m.invitedBy != "Ada Inviter" {
		t.Fatalf("invited_by = %q, want the inviter's person.name \"Ada Inviter\"", m.invitedBy)
	}
	var tenantName string
	ctx := context.Background()
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT name FROM tenant WHERE id = $1`, tenantID).Scan(&tenantName); err != nil {
		t.Fatalf("read tenant name: %v", err)
	}
	if m.project != tenantName {
		t.Fatalf("project = %q, want the tenant name %q (no project domain exists)", m.project, tenantName)
	}
	var resp struct {
		Status   string `json:"status"`
		DevToken string `json:"dev_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "pending" {
		t.Fatalf("status = %q, want pending", resp.Status)
	}
	if resp.DevToken == "" || resp.DevToken != m.code {
		t.Fatalf("dev_token %q, want the code the mailer was handed (%q)", resp.DevToken, m.code)
	}
	var n int
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM tenant_member tm JOIN person p ON p.id = tm.person_id
		  WHERE tm.tenant_id = $1 AND p.email = $2 AND tm.status = 'pending'`,
		tenantID, want).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pending membership rows = %d (err %v), want 1 — the send succeeded, so the invite landed", n, err)
	}
}

// Decision 16's error table: a send failure rolls the whole write back and
// answers 503, so nobody is added. The person row goes with the membership —
// the rollback undoes the transaction, not just its last statement.
func TestCreateRecipient_FailingMailerRollsBack(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{err: errors.New("agent down")}
	// Unique per run, same reason as the fresh-invite test: a stale code from
	// an earlier run would take the cooldown branch and never reach the mailer.
	addr := fmt.Sprintf("lost%d@example.com", time.Now().UnixNano())

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, addr), tenantID, inviterID)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "email_unavailable") {
		t.Fatalf("response = %s, want error code email_unavailable", w.Body.String())
	}
	ctx := context.Background()
	var members, persons int
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM tenant_member tm JOIN person p ON p.id = tm.person_id
		  WHERE tm.tenant_id = $1 AND p.email = $2`,
		tenantID, addr).Scan(&members); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if members != 0 {
		t.Fatalf("membership rows = %d, want 0 — the invite was not sent, so nobody was added", members)
	}
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM person WHERE email = $1`, addr).Scan(&persons); err != nil || persons != 0 {
		t.Fatalf("person rows = %d (err %v), want 0 — the rollback undoes the whole transaction", persons, err)
	}
}

// An already-active member is in: the write short-circuits before any code is
// minted or mail sent, and answers the status the row already has.
func TestCreateRecipient_ActiveMemberShortCircuits(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{}
	ctx := context.Background()
	// Unique per run: person.email is UNIQUE, and this run's member must not
	// collide with an earlier run's.
	addr := fmt.Sprintf("in%d@example.com", time.Now().UnixNano())
	var memberID int64
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'in') RETURNING id`, addr).Scan(&memberID); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'login', 'active')`,
		tenantID, memberID); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, addr), tenantID, inviterID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body.String())
	}
	var resp struct {
		Status   string `json:"status"`
		DevToken string `json:"dev_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "active" {
		t.Fatalf("status = %q, want active", resp.Status)
	}
	if resp.DevToken != "" {
		t.Fatalf("dev_token = %q, want none — no code is minted for an active member", resp.DevToken)
	}
	if m := h.mailer.(*inviteMailer); m.calls != 0 {
		t.Fatalf("SendInvite calls = %d, want 0 — an active member is never mailed", m.calls)
	}
	var codes int
	if err := h.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM magic_link_code WHERE email = $1`, addr).Scan(&codes); err != nil || codes != 0 {
		t.Fatalf("magic-link rows = %d (err %v), want 0 — the short-circuit mints nothing", codes, err)
	}
}

// seedMember plants a person + membership directly, the way the invite would
// have: role 'notify' is what its INSERT writes, status is the caller's. The
// resend tests need a row with no live code, which the invite path cannot
// give — it mints one on the way in, and the 60-second cooldown would then
// swallow the resend under test.
func seedMember(t *testing.T, h *WriteAPI, tenantID int64, status string) (int64, string) {
	t.Helper()
	addr := fmt.Sprintf("%s%d@example.com", status, time.Now().UnixNano())
	var personID int64
	if err := h.pool.Raw().QueryRow(context.Background(),
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'seeded') RETURNING id`,
		addr).Scan(&personID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if _, err := h.pool.Raw().Exec(context.Background(),
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, 'notify', $3)`,
		tenantID, personID, status); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	return personID, addr
}

// resendRequest builds the Decision-17 request for personID. The fresh IP is
// withFreshIP's own reason: the shared per-IP window must stay out of the
// way so the address cooldown is the only limiter these tests exercise.
func resendRequest(t *testing.T, personID int64) *http.Request {
	t.Helper()
	return withFreshIP(t, httptest.NewRequest(http.MethodPost,
		"/v1/recipients/"+strconv.FormatInt(personID, 10)+"/resend", nil))
}

// Decision 17's error table on the resend: a failed send answers 503 and the
// membership is still pending — a resend inserts nothing, so there is also
// nothing to roll back; the failed mail is the only thing that changed.
func TestResendInvite_FailingMailerKeepsPending(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{err: errors.New("agent down")}
	personID, addr := seedMember(t, h, tenantID, "pending")

	w := httptest.NewRecorder()
	h.resendInvite(w, resendRequest(t, personID), tenantID, inviterID)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d (%s), want 503", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "email_unavailable") {
		t.Fatalf("response = %s, want error code email_unavailable", w.Body.String())
	}
	var n int
	if err := h.pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_member tm JOIN person p ON p.id = tm.person_id
		  WHERE tm.tenant_id = $1 AND p.email = $2 AND tm.status = 'pending'`,
		tenantID, addr).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pending membership rows = %d (err %v), want 1 — the failed resend changed nothing", n, err)
	}
}

// Only a pending membership is a resend target: an active member accepted
// their invitation, and a second one is noise. The 404 is the same code the
// patch and delete arms use for a stranger's id.
func TestResendInvite_ActiveMemberIsNotATarget(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{}
	personID, _ := seedMember(t, h, tenantID, "active")

	w := httptest.NewRecorder()
	h.resendInvite(w, resendRequest(t, personID), tenantID, inviterID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want 404", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_such_person") {
		t.Fatalf("response = %s, want error code no_such_person", w.Body.String())
	}
	if m := h.mailer.(*inviteMailer); m.calls != 0 {
		t.Fatalf("SendInvite calls = %d, want 0 — an active member is never mailed", m.calls)
	}
}

// Decision 17: a resend inside the 60-second address cooldown answers 429
// rate_limited, never a 202 — a 202 would show "Sent!" while no second mail
// can go out. Each request carries its own IP so only the cooldown fires,
// and exactly one mail left.
func TestResendInvite_CooldownAnswers429(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	h.mailer = &inviteMailer{}
	personID, _ := seedMember(t, h, tenantID, "pending")

	w := httptest.NewRecorder()
	h.resendInvite(w, resendRequest(t, personID), tenantID, inviterID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("first resend: status = %d (%s), want 202", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.resendInvite(w, resendRequest(t, personID), tenantID, inviterID)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second resend: status = %d (%s), want 429", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("response = %s, want error code rate_limited", w.Body.String())
	}
	if m := h.mailer.(*inviteMailer); m.calls != 1 {
		t.Fatalf("SendInvite calls = %d, want exactly 1 — the cooldown refuses before the mailer", m.calls)
	}
}

// Decision 19's server half: an e-mail channel may only address an ACTIVE
// member — a pending person has not signed in, so their address is unproven
// and answers the same 400 unknown_recipient a stranger's does. The active
// member beside them proves the refusal is the status, not the query.
func TestCreateChannel_PendingAddressIsUnknownRecipient(t *testing.T) {
	h, tenantID, _ := openRecipientsDB(t)
	_, pendingAddr := seedMember(t, h, tenantID, "pending")
	_, activeAddr := seedMember(t, h, tenantID, "active")
	channelReq := func(addr string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/v1/channels",
			strings.NewReader(fmt.Sprintf(`{"kind":"email","target":%q}`, addr)))
	}

	w := httptest.NewRecorder()
	h.createChannel(w, channelReq(pendingAddr), tenantID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("pending address: status = %d (%s), want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown_recipient") {
		t.Fatalf("response = %s, want error code unknown_recipient", w.Body.String())
	}
	w = httptest.NewRecorder()
	h.createChannel(w, channelReq(activeAddr), tenantID)
	if w.Code != http.StatusCreated {
		t.Fatalf("active address: status = %d (%s), want 201", w.Code, w.Body.String())
	}
	var pending, active int
	if err := h.pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE target = $2), count(*) FILTER (WHERE target = $3)
		   FROM alert_channel WHERE tenant_id = $1 AND kind = 'email'`,
		tenantID, pendingAddr, activeAddr).Scan(&pending, &active); err != nil {
		t.Fatalf("count email channels: %v", err)
	}
	if pending != 0 || active != 1 {
		t.Fatalf("email channels: pending = %d, active = %d — want 0 and 1: the status decides, not the address", pending, active)
	}
}
