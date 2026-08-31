//go:build integration

// The telegram_recipients axis counts DESTINATIONS: linked people and
// connected broadcast rows (groups/channels) each hold a seat; a person's own
// channel row is not a second seat for the same human.
// Run: UC_TEST_POSTGRES=... go test -tags=integration ./internal/api/...
package api

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestTelegramSeatsCountDestinations(t *testing.T) {
	h, tenantID, inviterID := openRecipientsDB(t)
	ctx := context.Background()
	// person.telegram_id is UNIQUE across the whole database, which survives
	// between runs — a constant here collides with its own previous run.
	tgID := time.Now().UnixNano()

	seats := func() (used, max int) {
		used, max, err := countTelegramRecipients(ctx, h.pool, tenantID)
		if err != nil {
			t.Fatalf("countTelegramRecipients: %v", err)
		}
		return used, max
	}

	if used, max := seats(); used != 0 || max != 3 {
		t.Fatalf("fresh Free tenant: used %d max %d, want 0 and 3", used, max)
	}

	// A connected group holds a seat — before broadcasts counted, a redeemed
	// group FREED the seat its pending invite had held.
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, label)
		 VALUES (gen_random_uuid(), $1, 'telegram', '-4200200', 'Ops room')`, tenantID); err != nil {
		t.Fatalf("seed broadcast row: %v", err)
	}
	if used, _ := seats(); used != 1 {
		t.Fatalf("after a group connect: used %d, want 1", used)
	}

	// A person's own channel row is the delivery leg of a seat already counted
	// through person.telegram_id — never a second seat.
	if _, err := h.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, recipient_person_id)
		 VALUES (gen_random_uuid(), $1, 'telegram', $2, $3)`,
		tenantID, strconv.FormatInt(tgID, 10), inviterID); err != nil {
		t.Fatalf("seed personal row: %v", err)
	}
	if used, _ := seats(); used != 1 {
		t.Fatalf("a personal channel row changed the count: used %d, want 1", used)
	}
	if _, err := h.pool.Raw().Exec(ctx,
		`UPDATE person SET telegram_id = $2 WHERE id = $1`, inviterID, tgID); err != nil {
		t.Fatalf("link inviter: %v", err)
	}
	if used, _ := seats(); used != 2 {
		t.Fatalf("a linked member must hold a seat: used %d, want 2", used)
	}
}
