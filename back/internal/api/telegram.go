// Telegram invite endpoints (openspec telegram-bot-auth-and-recipients, D2/D9).
//
//   POST   /v1/telegram/invites     — mint a one-time inv_<token> link (login+)
//   PATCH  /v1/telegram/invites/{id} — change the role an invite will grant
//   DELETE /v1/telegram/invites/{id} — burn an unredeemed invite
//
// The token is the credential: stored as sha256, shown in the POST response
// exactly once (the key rule — never in a log, never retrievable). The plan
// axis telegram_recipients gates minting; the wall is the same 402
// error.upgrade every other paid feature answers with.

package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/channel/telegram"
	"go.upcontrol.io/back/internal/storage/pg"
)

// inviteTTL: the link has to survive being forwarded to a colleague, so it is
// days, not the magic link's minutes. One-time still bounds the damage of a
// leaked link far more than any short TTL would.
const inviteTTL = 7 * 24 * time.Hour

// Telegram serves the invite endpoints.
type Telegram struct {
	pool *pg.Pool
	sess *session.Manager
	// A func, not a string: the bot username can arrive at runtime from the
	// Settings screen, and invites have to mint without a restart.
	botUsername func(context.Context) string
}

func NewTelegram(p *pg.Pool, sm *session.Manager, botUsername func(context.Context) string) *Telegram {
	return &Telegram{pool: p, sess: sm, botUsername: botUsername}
}

func (h *Telegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	if !roleAtLeastLogin(r.Context(), h.pool, s.PersonID, s.TenantID) {
		writeAPIErr(w, http.StatusForbidden, "notify_role")
		return
	}
	switch {
	case r.URL.Path == "/v1/telegram/invites" && r.Method == http.MethodPost:
		h.createInvite(w, r, s.TenantID, s.PersonID)
	case strings.HasPrefix(r.URL.Path, "/v1/telegram/invites/") && r.Method == http.MethodPatch:
		h.patchInvite(w, r, s.TenantID)
	case strings.HasPrefix(r.URL.Path, "/v1/telegram/invites/") && r.Method == http.MethodDelete:
		h.deleteInvite(w, r, s.TenantID)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

func (h *Telegram) createInvite(w http.ResponseWriter, r *http.Request, tenantID, personID int64) {
	ctx := r.Context()
	var req struct {
		Role string `json:"role"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Role == "" {
		req.Role = "notify" // least privilege, chosen by default at invite
	}
	if req.Role != "notify" && req.Role != "login" {
		writeAPIErr(w, http.StatusBadRequest, "bad_role")
		return
	}
	botUsername := h.botUsername(ctx)
	if botUsername == "" {
		writeAPIErr(w, http.StatusConflict, "no_bot")
		return
	}

	used, max, err := countTelegramRecipients(ctx, h.pool, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if used >= max {
		writeUpgrade(w, planRecipientWall(max))
		return
	}

	token := "inv_" + randomHex()
	// The bot's own hasher, not a local sha256: mint and redeem hashing the
	// token independently is how every invite ever minted was unredeemable —
	// this side hashed the full "inv_…" string, the bot hashed the tail.
	hash := telegram.InviteTokenHash(token)
	expires := time.Now().UTC().Add(inviteTTL)
	var id int64
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO telegram_invite (tenant_id, role, invited_by, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		tenantID, req.Role, personID, hash, expires).Scan(&id); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id":        intToStr(id),
		"role":      req.Role,
		"link":      "https://t.me/" + botUsername + "?start=" + token,
		"expiresAt": expires.Format(time.RFC3339),
	})
}

func (h *Telegram) patchInvite(w http.ResponseWriter, r *http.Request, tenantID int64) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeStrict(w, r, &req) || (req.Role != "notify" && req.Role != "login") {
		writeAPIErr(w, http.StatusBadRequest, "bad_role")
		return
	}
	id := parseID(pathLast(r.URL.Path))
	var role string
	if err := h.pool.Raw().QueryRow(r.Context(),
		`UPDATE telegram_invite SET role = $1
		  WHERE id = $2 AND tenant_id = $3 AND redeemed_at IS NULL AND expires_at > now()
		 RETURNING role`, req.Role, id, tenantID).Scan(&role); err != nil {
		writeAPIErr(w, http.StatusNotFound, "no_such_invite")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"id": pathLast(r.URL.Path), "role": role, "status": "pending"})
}

func (h *Telegram) deleteInvite(w http.ResponseWriter, r *http.Request, tenantID int64) {
	// "Burning" is expiry-by-hand: the row stays auditable, the token dies.
	tag, err := h.pool.Raw().Exec(r.Context(),
		`UPDATE telegram_invite SET expires_at = now()
		  WHERE id = $1 AND tenant_id = $2 AND redeemed_at IS NULL`, parseID(pathLast(r.URL.Path)), tenantID)
	if err != nil || tag.RowsAffected() == 0 {
		writeAPIErr(w, http.StatusNotFound, "no_such_invite")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared helpers (read_api and the gate both count the same thing) ---

// countTelegramRecipients is THE definition of the telegram_recipients axis:
// active members with a linked telegram_id (the owner included — decision 0.2,
// Free is the owner's chat + 2 teammates) plus pending unredeemed invites.
func countTelegramRecipients(ctx context.Context, pool *pg.Pool, tenantID int64) (used, max int, err error) {
	var usedI, maxI int
	err = pool.Raw().QueryRow(ctx,
		`SELECT (SELECT count(*)
		          FROM tenant_member tm JOIN person p ON p.id = tm.person_id
		         WHERE tm.tenant_id = $1 AND tm.status = 'active' AND p.telegram_id IS NOT NULL)
		     + (SELECT count(*) FROM telegram_invite
		         WHERE tenant_id = $1 AND redeemed_at IS NULL AND expires_at > now()),
		       (SELECT telegram_recipients FROM plan_entitlement WHERE plan = (SELECT plan FROM tenant WHERE id = $1))`,
		tenantID).Scan(&usedI, &maxI)
	return usedI, maxI, err
}

// planRecipientWall names the wall the plan owns; the client never hardcodes it.
func planRecipientWall(max int) string {
	return "Free allows 3 Telegram recipients; paid plans carry 10 and up. Your plan allows " + intToStr(int64(max)) + "."
}

// roleAtLeastLogin answers whether this person may manage this tenant (§7.4:
// notify gets alerts and acknowledges; login changes things). The owner's own
// membership row carries login (EnsureTenantMember's first member), so this one
// check covers owner and login teammates; notify members are read-only here.
func roleAtLeastLogin(ctx context.Context, pool *pg.Pool, personID, tenantID int64) bool {
	var role string
	return pool.Raw().QueryRow(ctx,
		`SELECT role FROM tenant_member WHERE person_id = $1 AND tenant_id = $2`,
		personID, tenantID).Scan(&role) == nil && role != "notify"
}

// writeUpgrade is the 402 paid wall — the one shape every gate answers with.
func writeUpgrade(w http.ResponseWriter, reason string) {
	writeAPIJSON(w, http.StatusPaymentRequired, map[string]any{
		"error": map[string]any{
			"code":    "plan_limit_exceeded",
			"message": reason,
			"upgrade": map[string]any{"reason": reason},
		},
	})
}
