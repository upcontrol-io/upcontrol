// Telegram invite endpoints: mint a one-time inv_<token> link, burn an
// unredeemed invite. The token is the credential: sha256, shown exactly once.

package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/account/session"
	tgbot "go.upcontrol.io/back/internal/channel/telegram"
	"go.upcontrol.io/back/internal/storage/pg"
)

// inviteTTL: the link has to survive being forwarded to a colleague, so days,
// not the magic link's minutes. One-time bounds a leak more than a short TTL.
const inviteTTL = 7 * 24 * time.Hour

// telegram serves the invite endpoints.
type telegram struct {
	pool *pg.Pool
	sess *session.Manager
	// A func, not a string: the bot username can arrive at runtime from the
	// Settings screen, and invites have to mint without a restart.
	botUsername func(context.Context) string
}

func NewTelegram(p *pg.Pool, sm *session.Manager, botUsername func(context.Context) string) *telegram {
	return &telegram{pool: p, sess: sm, botUsername: botUsername}
}

func (h *telegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case strings.HasPrefix(r.URL.Path, "/v1/telegram/invites/") && r.Method == http.MethodDelete:
		h.deleteInvite(w, r, s.TenantID)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

func (h *telegram) createInvite(w http.ResponseWriter, r *http.Request, tenantID, personID int64) {
	ctx := r.Context()
	// The body is optional: a POST with no body mints an unbound invite, and
	// decodeStrict would read absence as bad_body.
	var req struct {
		PersonID string `json:"personId"`
	}
	if r.ContentLength != 0 && !decodeStrict(w, r, &req) {
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
	// token independently made every invite unredeemable.
	hash := tgbot.InviteTokenHash(token)
	expires := time.Now().UTC().Add(inviteTTL)
	var id int64
	var personRowID any // nil for the unbound invite, the person row id when bound
	// A personId binds the link to one teammate's row: the redeem links THAT
	// person, not a new one. Cross-tenant ids 404; already-linked is 409.
	if req.PersonID != "" {
		var linked *int64
		if err := h.pool.Raw().QueryRow(ctx,
			`SELECT p.id, p.telegram_id FROM person p
			  JOIN tenant_member tm ON tm.person_id = p.id
			 WHERE p.public_id = $1 AND tm.tenant_id = $2`,
			parseUUID(req.PersonID), tenantID).Scan(&personRowID, &linked); err != nil {
			writeAPIErr(w, http.StatusNotFound, "no_such_person")
			return
		}
		if linked != nil {
			writeAPIErr(w, http.StatusConflict, "already_linked")
			return
		}
	}
	// 'notify' as a literal: the invite grants no role choice; an Admin must
	// have an e-mail, and a Telegram invitee has none.
	if err := h.pool.Raw().QueryRow(ctx,
		`INSERT INTO telegram_invite (tenant_id, role, invited_by, token_hash, expires_at, person_id)
		 VALUES ($1, 'notify', $2, $3, $4, $5) RETURNING id`,
		tenantID, personID, hash, expires, personRowID).Scan(&id); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	resp := map[string]any{
		"id":        strconv.FormatInt(id, 10),
		"link":      "https://t.me/" + botUsername + "?start=" + token,
		"expiresAt": expires.Format(time.RFC3339),
	}
	// The echo lets the caller tie the link to the row it was minted for; the
	// list emission is the person's public id, so the same string round-trips.
	if req.PersonID != "" {
		resp["personId"] = req.PersonID
	}
	writeAPIJSON(w, http.StatusCreated, resp)
}

func (h *telegram) deleteInvite(w http.ResponseWriter, r *http.Request, tenantID int64) {
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

// countTelegramRecipients is THE definition of the telegram_recipients axis:
// active members with a linked telegram_id, plus pending unredeemed invites.
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
	return "Free allows 3 Telegram recipients; paid plans carry 10 and up. Your plan allows " + strconv.FormatInt(int64(max), 10) + "."
}

// roleAtLeastLogin answers whether this person may manage this tenant: notify
// gets alerts; login changes things. Covers owner and login teammates.
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
