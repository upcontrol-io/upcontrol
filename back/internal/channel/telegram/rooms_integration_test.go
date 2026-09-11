//go:build integration

// The broadcast paid wall (plan_entitlement.telegram_rooms): a group redeem on
// Free is refused with the transaction rolled back — the invite survives for a
// private chat and no channel row exists — while the same link on a paid plan
// connects the group as a broadcast destination. A frozen project refuses
// every redeem the same way.
// Run: UC_TEST_POSTGRES=... go test -tags=integration ./internal/channel/telegram/...
package telegram

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedUnboundInvite mints the teammate/group link (person_id NULL), the only
// kind a group redeem accepts.
func seedUnboundInvite(t *testing.T, b *bot, tenantID, projectID, inviterID int64) string {
	t.Helper()
	payload := fmt.Sprintf("inv_room%d", time.Now().UnixNano())
	if _, err := b.pool.Raw().Exec(context.Background(),
		`INSERT INTO telegram_invite (tenant_id, project_id, role, invited_by, token_hash, expires_at)
		 VALUES ($1, $2, 'notify', $3, $4, now() + interval '1 hour')`,
		tenantID, projectID, inviterID, InviteTokenHash(payload)); err != nil {
		t.Fatalf("seed unbound invite: %v", err)
	}
	return payload
}

// startFromGroup drives the /start a group chat produces: a negative chat id
// and the group's own title.
func startFromGroup(ctx context.Context, b *bot, payload string) {
	b.handleStart(ctx, &tgMessage{
		Text: "/start " + payload,
		From: tgUser{ID: 700100, FirstName: "Ada", Username: "ada"},
		Chat: tgChat{ID: -4200100, Type: "supergroup", Title: "Ops room"},
	}, payload)
}

func TestGroupRedeemPaidWall(t *testing.T) {
	b, tenantID, projectID := openTelegramDB(t)
	ctx := context.Background()
	inviterID := seedOwner(t, b, tenantID, projectID, "active")
	payload := seedUnboundInvite(t, b, tenantID, projectID, inviterID)

	broadcasts := func() int {
		var n int
		if err := b.pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM alert_channel
			  WHERE project_id = $1 AND kind = 'telegram' AND recipient_person_id IS NULL`,
			projectID).Scan(&n); err != nil {
			t.Fatalf("count broadcasts: %v", err)
		}
		return n
	}
	redeemed := func() bool {
		var done bool
		if err := b.pool.Raw().QueryRow(ctx,
			`SELECT redeemed_at IS NOT NULL FROM telegram_invite WHERE token_hash = $1`,
			InviteTokenHash(payload)).Scan(&done); err != nil {
			t.Fatalf("read invite: %v", err)
		}
		return done
	}

	// Free (the seeded tenant's default): refused, and refused WHOLE — the
	// rollback keeps the link valid, and no half-connected group remains.
	startFromGroup(ctx, b, payload)
	if broadcasts() != 0 {
		t.Fatal("a Free group redeem created a broadcast channel — telegram_rooms did not gate")
	}
	if redeemed() {
		t.Fatal("the refused invite was burned — the rollback must keep it valid for a private chat")
	}

	// The same link on a paid plan connects the group.
	if _, err := b.pool.Raw().Exec(ctx,
		`UPDATE tenant SET plan = 'Indie' WHERE id = $1`, tenantID); err != nil {
		t.Fatalf("set plan: %v", err)
	}
	startFromGroup(ctx, b, payload)
	if broadcasts() != 1 {
		t.Fatal("an Indie group redeem did not connect the group")
	}
	if !redeemed() {
		t.Fatal("the successful redeem left the invite unredeemed")
	}
}

// A frozen project's team does not change: a private redeem is refused whole,
// nothing linked and the link left unredeemed, and the same link connects
// once the project is live again.
func TestFrozenProjectRedeemRefused(t *testing.T) {
	b, tenantID, projectID := openTelegramDB(t)
	ctx := context.Background()
	inviterID := seedOwner(t, b, tenantID, projectID, "active")
	payload := seedUnboundInvite(t, b, tenantID, projectID, inviterID)
	tgID := time.Now().UnixNano() % 1_000_000_000
	destinations := func() int {
		var n int
		if err := b.pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM alert_channel WHERE project_id = $1 AND kind = 'telegram'`,
			projectID).Scan(&n); err != nil {
			t.Fatalf("count destinations: %v", err)
		}
		return n
	}
	redeemed := func() bool {
		var done bool
		if err := b.pool.Raw().QueryRow(ctx,
			`SELECT redeemed_at IS NOT NULL FROM telegram_invite WHERE token_hash = $1`,
			InviteTokenHash(payload)).Scan(&done); err != nil {
			t.Fatalf("read invite: %v", err)
		}
		return done
	}
	setFrozen := func(frozen bool) {
		t.Helper()
		if _, err := b.pool.Raw().Exec(ctx,
			`UPDATE project SET frozen_at = CASE WHEN $2 THEN now() END WHERE id = $1`, projectID, frozen); err != nil {
			t.Fatalf("set frozen=%v: %v", frozen, err)
		}
	}

	setFrozen(true)
	startFrom(ctx, b, tgID, payload)
	if destinations() != 0 || redeemed() {
		t.Fatalf("a redeem into a frozen project connected %d destinations (redeemed=%v)", destinations(), redeemed())
	}
	setFrozen(false)
	startFrom(ctx, b, tgID, payload)
	if destinations() != 1 || !redeemed() {
		t.Fatalf("the same link on the live project connected %d destinations (redeemed=%v), want 1", destinations(), redeemed())
	}
}
