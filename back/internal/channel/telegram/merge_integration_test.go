//go:build integration

// The bound-invite redeem: the ORPHAN fork an unbound redeem left behind is
// absorbed, a real second account and a live teammate are refused, the merge
// reaches no further than the tenant the invite named, and a linked member
// holds a Telegram seat at whatever status they carry.
// Run: UC_TEST_POSTGRES=... go test -tags=integration ./internal/channel/telegram/...
package telegram

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/migrate"
	"go.upcontrol.io/back/internal/storage/pg"
)

// okTransport answers every Bot API call 200 {"ok":true}: a test must never
// reach api.telegram.org.
type okTransport struct{}

func (okTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

// openTelegramDB applies migrations and returns a bot with a stubbed Bot API
// plus a fresh tenant and its project (an incident needs one).
func openTelegramDB(t *testing.T) (*bot, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("UC_TEST_POSTGRES not set; skipping telegram merge integration test")
	}
	ctx := context.Background()
	if err := migrate.Run(ctx, dsn, "../../../../db/postgres"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	b := &bot{
		pool:   pool,
		log:    slog.New(slog.DiscardHandler),
		client: &http.Client{Transport: okTransport{}},
	}
	tenantID, projectID := seedTenant(t, b)
	return b, tenantID, projectID
}

// seedTenant plants a tenant and its project. A second one is what makes the
// merge's tenant scoping testable: the invite names one workspace only.
func seedTenant(t *testing.T, b *bot) (tenantID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO tenant (public_id, name) VALUES (gen_random_uuid(), $1) RETURNING id`,
		fmt.Sprintf("tg-merge-%d", time.Now().UnixNano())).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES (gen_random_uuid(), $1, 'merge.example')
		 RETURNING id`, tenantID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return tenantID, projectID
}

// seedMember plants one membership row.
func seedMember(t *testing.T, b *bot, tenantID, personID int64, role, status string) {
	t.Helper()
	if _, err := b.pool.Raw().Exec(context.Background(),
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, $3, $4)`,
		tenantID, personID, role, status); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// seedOwner plants the invited person: an e-mail account on the tenant with
// no Telegram of their own yet, at the membership status asked for.
func seedOwner(t *testing.T, b *bot, tenantID int64, status string) int64 {
	t.Helper()
	var id int64
	if err := b.pool.Raw().QueryRow(context.Background(),
		`INSERT INTO person (public_id, email, name) VALUES (gen_random_uuid(), $1, 'Ada Owner') RETURNING id`,
		fmt.Sprintf("owner-%d@example.com", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	seedMember(t, b, tenantID, id, "login", status)
	return id
}

// seedTelegramOnly plants the row that holds the telegram_id: the same human's
// fork until some other identity says otherwise.
func seedTelegramOnly(t *testing.T, b *bot, tgID int64) int64 {
	t.Helper()
	var id int64
	if err := b.pool.Raw().QueryRow(context.Background(),
		`INSERT INTO person (public_id, telegram_id, telegram_username, name)
		 VALUES (gen_random_uuid(), $1, 'ada', 'Ada') RETURNING id`, tgID).Scan(&id); err != nil {
		t.Fatalf("seed telegram-only person: %v", err)
	}
	return id
}

// seedInvite mints a BOUND invite for personID and returns the deep-link
// payload the /start carries. 'notify' is the literal the mint hardcodes — an
// Admin must have an e-mail and a Telegram invitee has none.
func seedInvite(t *testing.T, b *bot, tenantID, personID int64) string {
	t.Helper()
	payload := fmt.Sprintf("inv_tok%d", time.Now().UnixNano())
	if _, err := b.pool.Raw().Exec(context.Background(),
		`INSERT INTO telegram_invite (tenant_id, role, invited_by, person_id, token_hash, expires_at)
		 VALUES ($1, 'notify', $2, $2, $3, now() + interval '1 hour')`,
		tenantID, personID, InviteTokenHash(payload)); err != nil {
		t.Fatalf("seed invite: %v", err)
	}
	return payload
}

// startFrom drives the private-chat /start the way handleUpdate would; in a
// private chat the chat id IS the user id.
func startFrom(ctx context.Context, b *bot, tgID int64, payload string) {
	b.handleStart(ctx, &tgMessage{
		Text: "/start " + payload,
		From: tgUser{ID: tgID, FirstName: "Ada", Username: "ada"},
		Chat: tgChat{ID: tgID, Type: "private"},
	}, payload)
}

// personRef reads one nullable reference to person — telegram_id, acked_by,
// recipient_person_id — the shape every assertion here checks.
func personRef(t *testing.T, b *bot, query string, arg int64) *int64 {
	t.Helper()
	var id *int64
	if err := b.pool.Raw().QueryRow(context.Background(), query, arg).Scan(&id); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return id
}

// assertRefused pins a refusal whole: nothing linked, the holder untouched,
// and the rolled-back redeem leaving the link valid for its person.
func assertRefused(t *testing.T, b *bot, ownerID, holderID, tgID int64, payload string) {
	t.Helper()
	ctx := context.Background()
	if tg := personRef(t, b, `SELECT telegram_id FROM person WHERE id = $1`, ownerID); tg != nil {
		t.Fatalf("invited person's telegram_id = %d, want NULL — somebody else's Telegram is never linked", *tg)
	}
	var stillThere int
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM person WHERE id = $1 AND telegram_id = $2`, holderID, tgID).Scan(&stillThere); err != nil || stillThere != 1 {
		t.Fatalf("the other account's rows = %d (err %v), want 1 — a real account is never absorbed", stillThere, err)
	}
	var redeemed *time.Time
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT redeemed_at FROM telegram_invite WHERE token_hash = $1`, InviteTokenHash(payload)).Scan(&redeemed); err != nil {
		t.Fatalf("read the invite: %v", err)
	}
	if redeemed != nil {
		t.Fatalf("invite redeemed_at = %v, want NULL — a refusal rolls the redeem back and the link stays valid", *redeemed)
	}
}

// The fork an unbound redeem left behind is absorbed: its channel and its
// acknowledgement follow it into the invited person, its visitor row goes
// anonymous, and the row goes.
func TestBoundRedeem_AbsorbsTelegramOnlyFork(t *testing.T) {
	b, tenantID, projectID := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")

	// The fork: same human, telegram-only, and — the shape deleteRecipient
	// leaves — member of NO tenant at all. A membership anywhere, this tenant
	// included, would make it a live teammate and refuse the merge.
	tgID := time.Now().UnixNano()
	forkID := seedTelegramOnly(t, b, tgID)
	// The three FKs to person without a cascade. The first two block the
	// DELETE outright; web_visitor.person_id nulls itself instead, and the
	// merge lets it — web_events keeps naming the dead row whatever we do.
	if _, err := b.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, recipient_person_id, label)
		 VALUES (gen_random_uuid(), $1, 'telegram', $2, $3, 'Ada @ada')`,
		tenantID, strconv.FormatInt(tgID, 10), forkID); err != nil {
		t.Fatalf("seed fork channel: %v", err)
	}
	var incidentID int64
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO incident (public_id, tenant_id, project_id, detector, fingerprint, title,
		                       status, detected_at, acked_at, acked_by)
		 VALUES (gen_random_uuid(), $1, $2, 'http', 1, 'merge.example is down', 'down', now(), now(), $3)
		 RETURNING id`, tenantID, projectID, forkID).Scan(&incidentID); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	var visitorID int64
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO web_visitor (token_hash, person_id, tenant_id)
		 VALUES (decode(md5(random()::text), 'hex'), $1, $2) RETURNING id`,
		forkID, tenantID).Scan(&visitorID); err != nil {
		t.Fatalf("seed visitor: %v", err)
	}

	startFrom(ctx, b, tgID, seedInvite(t, b, tenantID, ownerID))

	ownerTG := personRef(t, b, `SELECT telegram_id FROM person WHERE id = $1`, ownerID)
	if ownerTG == nil || *ownerTG != tgID {
		t.Fatalf("invited person's telegram_id = %v, want %d — the link must bind after the merge", ownerTG, tgID)
	}
	var forks int
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM person WHERE id = $1`, forkID).Scan(&forks); err != nil || forks != 0 {
		t.Fatalf("fork person rows = %d (err %v), want 0 — the fork is absorbed, not left holding the telegram_id", forks, err)
	}
	channelOwner := personRef(t, b,
		`SELECT recipient_person_id FROM alert_channel WHERE tenant_id = $1 AND kind = 'telegram'`, tenantID)
	if channelOwner == nil || *channelOwner != ownerID {
		t.Fatalf("channel recipient = %v, want the invited person %d", channelOwner, ownerID)
	}
	ackedBy := personRef(t, b, `SELECT acked_by FROM incident WHERE id = $1`, incidentID)
	if ackedBy == nil || *ackedBy != ownerID {
		t.Fatalf("incident acked_by = %v, want the invited person %d — the ack is the same human's", ackedBy, ownerID)
	}
	if visitor := personRef(t, b,
		`SELECT person_id FROM web_visitor WHERE id = $1`, visitorID); visitor != nil {
		t.Fatalf("visitor person_id = %d, want NULL — the merge does not claim to keep the analytics identity: web_events would still name the deleted row", *visitor)
	}
	var members int
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM tenant_member WHERE tenant_id = $1`, tenantID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("tenant members = %d (err %v), want 1 — one human, one membership", members, err)
	}
}

// The invite names one tenant, so the merge repoints rows inside it and takes
// no identity out of the tenants it never named: the foreign ack loses its
// name, the foreign private destination is removed rather than re-typed into a
// broadcast group. deleteRecipient is the path that leaves such rows behind:
// it drops a member's tenant_member row and telegram channel and leaves their
// acks pointing at a person the other tenant no longer knows.
func TestBoundRedeem_MergeStopsAtTheInvitesTenant(t *testing.T) {
	b, tenantID, projectID := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")
	otherTenantID, otherProjectID := seedTenant(t, b)

	tgID := time.Now().UnixNano()
	forkID := seedTelegramOnly(t, b, tgID)
	// No membership in either tenant — that is what leaves this a fork rather
	// than somebody's account — but the other tenant's rows are still there:
	// deleteRecipient drops the membership and leaves the acks behind.
	var mineIncidentID, theirsIncidentID int64
	for _, seed := range []struct {
		tenant, project int64
		into            *int64
	}{{tenantID, projectID, &mineIncidentID}, {otherTenantID, otherProjectID, &theirsIncidentID}} {
		if err := b.pool.Raw().QueryRow(ctx,
			`INSERT INTO incident (public_id, tenant_id, project_id, detector, fingerprint, title,
			                       status, detected_at, acked_at, acked_by)
			 VALUES (gen_random_uuid(), $1, $2, 'http', 1, 'merge.example is down', 'down', now(), now(), $3)
			 RETURNING id`, seed.tenant, seed.project, forkID).Scan(seed.into); err != nil {
			t.Fatalf("seed incident: %v", err)
		}
	}
	var theirsChannelID int64
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, recipient_person_id)
		 VALUES (gen_random_uuid(), $1, 'telegram', $2, $3) RETURNING id`,
		otherTenantID, "other-"+strconv.FormatInt(tgID, 10), forkID).Scan(&theirsChannelID); err != nil {
		t.Fatalf("seed the other tenant's channel: %v", err)
	}

	startFrom(ctx, b, tgID, seedInvite(t, b, tenantID, ownerID))

	var forks int
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM person WHERE id = $1`, forkID).Scan(&forks); err != nil || forks != 0 {
		t.Fatalf("fork person rows = %d (err %v), want 0 — the non-cascading FKs must not block the DELETE", forks, err)
	}
	if mine := personRef(t, b, `SELECT acked_by FROM incident WHERE id = $1`, mineIncidentID); mine == nil || *mine != ownerID {
		t.Fatalf("this tenant's incident acked_by = %v, want the invited person %d", mine, ownerID)
	}
	if theirs := personRef(t, b, `SELECT acked_by FROM incident WHERE id = $1`, theirsIncidentID); theirs != nil {
		t.Fatalf("the other tenant's incident acked_by = %d, want NULL — an invite grants authority over one tenant", *theirs)
	}
	var theirsChannels int
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT count(*) FROM alert_channel WHERE id = $1`, theirsChannelID).Scan(&theirsChannels); err != nil || theirsChannels != 0 {
		t.Fatalf("the other tenant's channel rows = %d (err %v), want 0 — a NULL recipient reads as a broadcast group, so a private destination whose person is deleted is removed, not re-typed", theirsChannels, err)
	}
}

// The seat, with no fork anywhere near it: createRecipient plants the
// membership 'pending', the mint binds a link to that pending row, and the
// redeem writes NO membership status — a Telegram redeem proves control of a
// Telegram account, not of an address, and status is what gates e-mail
// channels. Activating on this evidence would let a manager invite a victim
// by e-mail, redeem the bound link from a throwaway Telegram, and unlock an
// e-mail channel to an address nobody proved they hold.
func TestBoundRedeem_DoesNotActivateTheMembership(t *testing.T) {
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "pending")

	// Nobody else holds this telegram_id: no fork, no merge, nothing but the
	// redeem itself.
	tgID := time.Now().UnixNano()

	startFrom(ctx, b, tgID, seedInvite(t, b, tenantID, ownerID))

	var status string
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT status FROM tenant_member WHERE tenant_id = $1 AND person_id = $2`,
		tenantID, ownerID).Scan(&status); err != nil {
		t.Fatalf("read the membership: %v", err)
	}
	if status != "pending" {
		t.Fatalf("membership status = %q, want pending — a Telegram redeem is no proof of the address, and 'active' is what unlocks an e-mail channel to it", status)
	}
}

// A telegram-only person who is a LIVE member of the invite's own tenant is a
// teammate, not a fork: they joined by an unbound link, and absorbing them
// would delete their person row, cascade their membership and session and let
// the invitee sign in as them through /v1/auth/telegram.
func TestBoundRedeem_HolderInThisTenantIsRefused(t *testing.T) {
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")

	tgID := time.Now().UnixNano()
	// Telegram-only and e-mail-less: the holder passes every other test for a
	// fork, and only their own membership here refuses it.
	otherID := seedTelegramOnly(t, b, tgID)
	seedMember(t, b, tenantID, otherID, "notify", "active")
	payload := seedInvite(t, b, tenantID, ownerID)

	startFrom(ctx, b, tgID, payload)

	assertRefused(t, b, ownerID, otherID, tgID, payload)
}

// A holder with an e-mail is a real second account: refused, nothing linked,
// and the rollback keeps the invite usable.
func TestBoundRedeem_HolderWithEmailIsRefused(t *testing.T) {
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")

	tgID := time.Now().UnixNano()
	var otherID int64
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, email, telegram_id, telegram_username, name)
		 VALUES (gen_random_uuid(), $1, $2, 'bob', 'Bob Other') RETURNING id`,
		fmt.Sprintf("bob-%d@example.com", time.Now().UnixNano()), tgID).Scan(&otherID); err != nil {
		t.Fatalf("seed the other account: %v", err)
	}
	// Deliberately NO membership: an orphan is exactly the shape the merge absorbs, so the
	// refusal here can only come from the identity column, and the test sees it go.
	payload := seedInvite(t, b, tenantID, ownerID)

	startFrom(ctx, b, tgID, payload)

	assertRefused(t, b, ownerID, otherID, tgID, payload)
}

// google_sub is the third identity: the person CHECK passes on google_sub +
// telegram_id with no e-mail at all, and that row is somebody's Google login,
// not a fork.
func TestBoundRedeem_HolderWithGoogleSubIsRefused(t *testing.T) {
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")

	tgID := time.Now().UnixNano()
	var otherID int64
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, google_sub, telegram_id, telegram_username, name)
		 VALUES (gen_random_uuid(), $1, $2, 'bob', 'Bob Google') RETURNING id`,
		fmt.Sprintf("google-sub-%d", time.Now().UnixNano()), tgID).Scan(&otherID); err != nil {
		t.Fatalf("seed the Google account: %v", err)
	}
	// Deliberately NO membership: an orphan is exactly the shape the merge absorbs, so the
	// refusal here can only come from the identity column, and the test sees it go.
	payload := seedInvite(t, b, tenantID, ownerID)

	startFrom(ctx, b, tgID, payload)

	assertRefused(t, b, ownerID, otherID, tgID, payload)
}

// A membership in ANOTHER tenant is an identity too: this Telegram is how a
// second workspace reaches that person, and absorbing the row here would take
// it away from them there.
func TestBoundRedeem_HolderInAnotherTenantIsRefused(t *testing.T) {
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	ownerID := seedOwner(t, b, tenantID, "active")
	otherTenantID, _ := seedTenant(t, b)

	tgID := time.Now().UnixNano()
	// Telegram-only and e-mail-less: the holder passes every other test for a
	// fork, and only the other tenant's membership refuses it.
	otherID := seedTelegramOnly(t, b, tgID)
	seedMember(t, b, otherTenantID, otherID, "notify", "active")
	payload := seedInvite(t, b, tenantID, ownerID)

	startFrom(ctx, b, tgID, payload)

	assertRefused(t, b, ownerID, otherID, tgID, payload)
}
