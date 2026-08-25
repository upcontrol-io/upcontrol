//go:build integration

// Recipient-invite coverage for the address contract (§6.8): the invite
// normalises before anything is stored, so an invite under one spelling and a
// sign-in under another cannot land in two person rows, and a second invite of
// the same address adds no second membership.
//
// UC_TEST_POSTGRES=postgres://... go test -tags=integration ./internal/api/...
package api

import (
	"context"
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
// tenant, ready to receive invites.
func openRecipientsDB(t *testing.T) (*WriteAPI, int64) {
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
	return &WriteAPI{pool: pool}, tenantID
}

func inviteRequest(t *testing.T, email string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodPost, "/v1/recipients",
		strings.NewReader(fmt.Sprintf(`{"email":%q,"role":"notify"}`, email)))
}

// An invite under mixed case must land as the one lower-case spelling: the
// person is found by the normalised address (the same lookup the sign-in doors
// use), exactly one row exists for it, and the account is named from the local
// part instead of the empty string the INSERT used to bake in.
func TestCreateRecipient_NormalisedAddressIsOnePerson(t *testing.T) {
	h, tenantID := openRecipientsDB(t)

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, "Bob@Example.com"), tenantID)
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
	h, tenantID := openRecipientsDB(t)

	for _, addr := range []string{"Bob@Example.com", "bob@example.com"} {
		w := httptest.NewRecorder()
		h.createRecipient(w, inviteRequest(t, addr), tenantID)
		if w.Code != http.StatusCreated {
			t.Fatalf("invite %q: status = %d (%s), want 201", addr, w.Code, w.Body.String())
		}
	}
	var n int
	if err := h.pool.Raw().QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_member WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("membership rows = %d (err %v), want exactly 1", n, err)
	}
}

// An address with no @ in it is not an address: 400 bad_email, and nothing
// stored. The empty body takes the same refusal.
func TestCreateRecipient_BadEmailIsRefused(t *testing.T) {
	h, tenantID := openRecipientsDB(t)

	for _, body := range []string{`{"email":"not-an-address"}`, `{}`} {
		w := httptest.NewRecorder()
		h.createRecipient(w, httptest.NewRequest(http.MethodPost, "/v1/recipients", strings.NewReader(body)), tenantID)
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
	h, tenantID := openRecipientsDB(t)

	w := httptest.NewRecorder()
	h.createRecipient(w, inviteRequest(t, "gone@example.com"), tenantID)
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
