//go:build integration

// The broadcast paid wall (plan_entitlement.telegram_rooms): a group redeem on
// Free is refused with the transaction rolled back — the invite survives for a
// private chat and no channel row exists — while the same link on a paid plan
// connects the group as a broadcast destination.
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
func seedUnboundInvite(t *testing.T, b *bot, tenantID, inviterID int64) string {
	t.Helper()
	payload := fmt.Sprintf("inv_room%d", time.Now().UnixNano())
	if _, err := b.pool.Raw().Exec(context.Background(),
		`INSERT INTO telegram_invite (tenant_id, role, invited_by, token_hash, expires_at)
		 VALUES ($1, 'notify', $2, $3, now() + interval '1 hour')`,
		tenantID, inviterID, InviteTokenHash(payload)); err != nil {
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
	b, tenantID, _ := openTelegramDB(t)
	ctx := context.Background()
	inviterID := seedOwner(t, b, tenantID, "active")
	payload := seedUnboundInvite(t, b, tenantID, inviterID)

	broadcasts := func() int {
		var n int
		if err := b.pool.Raw().QueryRow(ctx,
			`SELECT count(*) FROM alert_channel
			  WHERE tenant_id = $1 AND kind = 'telegram' AND recipient_person_id IS NULL`,
			tenantID).Scan(&n); err != nil {
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
