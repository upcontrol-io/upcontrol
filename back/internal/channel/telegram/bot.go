// Package telegram is the long-polling bot under a Postgres advisory lock (one
// poller across replicas). Buttons are authorised by the person, not the chat.
package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
)

// notConnected is the one refusal every command shares: the chat has no
// channel, or the presser is not a member.
const notConnected = "This chat is not connected to a project. Ask the owner for an invite link."

// bot is the long-polling Telegram bot.
type bot struct {
	token     string
	appURL    string // public origin — the Open Mini App web_app button's target
	pool      *pg.Pool
	incidents *incident.Lifecycle
	log       *slog.Logger
	client    *http.Client
	offset    int64 // last processed update_id + 1
}

// NewBot builds a bot. The token is the Bot API token from @BotFather; appURL
// is the deployment's public origin ("" hides the Open App button).
func NewBot(token, appURL string, pool *pg.Pool, lc *incident.Lifecycle, log *slog.Logger) *bot {
	return &bot{
		// Trimmed here: a token with a trailing newline breaks every request
		// URL and looks like a started-but-dead poller.
		token:     strings.TrimSpace(token),
		appURL:    strings.TrimRight(appURL, "/"),
		pool:      pool,
		incidents: lc,
		log:       log,
		client:    &http.Client{Timeout: 70 * time.Second}, // long-poll needs >60s
	}
}

// Run polls getUpdates under an advisory lock until ctx is cancelled; the
// session-level lock auto-releases if the holder crashes.
func (b *bot) Run(ctx context.Context) error {
	// The lock must sit on a dedicated connection: a borrowed idle
	// connection the pool may close would silently release the lock.
	const lockKey = 0x75637467 // "uctg" — stable, unique per bot
	lockConn, err := b.pool.Raw().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("telegram: lock connection: %w", err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("telegram: advisory lock: %w", err)
	}
	b.log.Info("telegram bot started (advisory lock acquired)")
	b.register()

	const heartbeatEvery = 5 * time.Minute
	var lastBeat time.Time
	failures := 0
	for {
		select {
		case <-ctx.Done():
			// Release the lock on graceful shutdown, on the connection that
			// holds it (any other session's unlock would be a no-op).
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = lockConn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey)
			cancel()
			b.log.Info("telegram bot stopped (lock released)")
			return nil
		default:
		}

		updates, err := b.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled — clean exit
			}
			failures++
			backoff := time.Duration(failures) * 5 * time.Second
			if backoff > time.Minute {
				backoff = time.Minute
			}
			// Loud AND labelled: a dead bot reads as a growing `consecutive`
			// count in the log, not as silence after "started".
			b.log.Warn("telegram getUpdates failed", "err", err, "consecutive", failures, "retry_in", backoff.String())
			time.Sleep(backoff)
			continue
		}
		failures = 0

		for _, u := range updates {
			b.dispatch(ctx, u)
			b.offset = u.UpdateID + 1
		}

		// The heartbeat: every 5 minutes of quiet the poller says it is alive
		// and names the offset, so a dead bot is tellable from the log.
		if now := time.Now(); now.Sub(lastBeat) >= heartbeatEvery {
			lastBeat = now
			b.log.Info("telegram bot polling", "offset", b.offset, "updates", len(updates))
		}
	}
}

// register tells Telegram the command list and the menu button; both are
// idempotent and best-effort, a failure is logged and polling continues.
func (b *bot) register() {
	if err := b.call("setMyCommands", map[string]any{"commands": []map[string]string{
		{"command": "status", "description": "How your checks and incidents are doing"},
		{"command": "mute", "description": "Silence alerts for a while (30m, 2h, 1d)"},
		{"command": "unmute", "description": "Lift the mute early"},
		{"command": "stop", "description": "Disconnect this chat for good"},
		{"command": "help", "description": "What this bot does"},
		{"command": "id", "description": "Your Telegram id, for support"},
	}}); err != nil {
		b.log.Warn("telegram setMyCommands failed", "err", err)
	}
	// web_app needs https (Telegram refuses other schemes); an empty appURL
	// is legal: no public origin, no app to open.
	if !strings.HasPrefix(b.appURL, "https://") {
		return
	}
	if err := b.call("setChatMenuButton", map[string]any{"menu_button": map[string]any{
		"type": "web_app",
		"text": "Dashboard",
		"web_app": map[string]string{
			"url": b.appURL + "/app",
		},
	}}); err != nil {
		b.log.Warn("telegram setChatMenuButton failed", "err", err)
	}
}

// poll is the panic-safe getUpdates: a panic becomes an error the loop
// backs off from, instead of killing the goroutine.
func (b *bot) poll(ctx context.Context) (updates []tgUpdate, err error) {
	defer func() {
		if r := recover(); r != nil {
			updates, err = nil, fmt.Errorf("poll cycle panicked: %v", r)
		}
	}()
	return b.getUpdates(ctx)
}

// dispatch is the panic-safe update handler: one malformed update can fail,
// loudly, without taking the poller down with it.
func (b *bot) dispatch(ctx context.Context, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("telegram handler panicked", "update_id", u.UpdateID, "panic", r)
		}
	}()
	b.handleUpdate(ctx, u)
}

type tgUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *tgMessage  `json:"message,omitempty"`
	CallbackQuery *tgCallback `json:"callback_query,omitempty"`
}

type tgMessage struct {
	Text string `json:"text"`
	From tgUser `json:"from"`
	Chat tgChat `json:"chat"`
}

type tgCallback struct {
	ID      string    `json:"id"`
	From    tgUser    `json:"from"`
	Data    string    `json:"data"`    // "ack:<public_id>" or "resolve:<public_id>"
	Message tgMessage `json:"message"` // the message the button is on
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type tgChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private | group | supergroup | channel
	Title string `json:"title"`
}

func (b *bot) getUpdates(ctx context.Context) ([]tgUpdate, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=60",
		b.token, b.offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API returned not-ok")
	}
	return result.Result, nil
}

// call is the one Bot API door: POST JSON and report both transport failures
// and API refusals (a bad token answers 401, a malformed request 400).
func (b *bot) call(method string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := b.client.Post(
		fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.token, method),
		"application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (b *bot) answerCallback(callbackID string, text string) {
	_ = b.call("answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	})
}

// command splits "/mute@mybot 30m" into ("mute", "30m"). The @bot suffix is
// how Telegram addresses a command inside a group.
func command(text string) (name, rest string) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", ""
	}
	name = strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return name, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

func (b *bot) handleUpdate(ctx context.Context, u tgUpdate) {
	if u.CallbackQuery != nil {
		b.handleCallback(ctx, u.CallbackQuery)
		return
	}
	if u.Message == nil {
		return
	}
	name, rest := command(u.Message.Text)
	switch name {
	case "start":
		b.handleStart(ctx, u.Message, rest)
	case "help":
		b.handleHelp(u.Message)
	case "id":
		b.send(u.Message.Chat.ID, "Your Telegram id: "+strconv.FormatInt(u.Message.From.ID, 10))
	case "status":
		b.handleStatus(ctx, u.Message)
	case "mute":
		b.handleMute(ctx, u.Message, rest)
	case "unmute":
		b.handleUnmute(ctx, u.Message)
	case "stop":
		b.handleStop(ctx, u.Message)
	default:
		// Unknown commands and plain messages: silent.
	}
}

// handleStart processes the /start deep link: anything that is not a valid
// unredeemed invite binds nothing. The redeem runs in one transaction.
func (b *bot) handleStart(ctx context.Context, msg *tgMessage, payload string) {
	if strings.HasPrefix(payload, "prj-") {
		// Old links are already in people's chats; the honest answer names
		// the new way instead of silently ignoring them.
		b.send(msg.Chat.ID, "That link is out of date. Ask the project owner for a fresh invite from the Alerts screen (Alerts → Invite on Telegram).")
		return
	}
	token, ok := strings.CutPrefix(payload, "inv_")
	if !ok || token == "" {
		b.send(msg.Chat.ID, "This bot alerts you about incidents in your project. To connect, ask the project owner for an invite link from the Alerts screen (Alerts → Invite on Telegram).")
		return
	}

	// One-time, race-safe: the atomic UPDATE ... WHERE redeemed_at IS NULL;
	// hashed as the FULL payload, the form the mint side stored.
	hash := InviteTokenHash(payload)
	tx, err := b.pool.Raw().Begin(ctx)
	if err != nil {
		b.log.Warn("telegram: invite redeem could not open a transaction", "err", err)
		b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	var tenantID int64
	var role string
	var invitePersonID *int64 // the person row a bound link was minted for
	if err := tx.QueryRow(ctx,
		`UPDATE telegram_invite SET redeemed_at = now()
		  WHERE token_hash = $1 AND redeemed_at IS NULL AND expires_at > now()
		 RETURNING tenant_id, role, person_id`, hash).Scan(&tenantID, &role, &invitePersonID); err != nil {
		b.send(msg.Chat.ID, "This invite link is no longer valid. Ask the project owner for a fresh one from the Alerts screen.")
		return
	}
	var tenantName string
	_ = tx.QueryRow(ctx,
		`SELECT name FROM tenant WHERE id = $1`, tenantID).Scan(&tenantName)

	// A GROUP redeem connects a broadcast destination: no person attached;
	// web_app is dropped there, the Bot API refuses it outside private chats.
	if msg.Chat.Type == "group" || msg.Chat.Type == "supergroup" || msg.Chat.ID < 0 {
		// A person-bound link names one person, and a group is nobody's
		// private chat: refuse, roll back, keep the link valid.
		if invitePersonID != nil {
			b.send(msg.Chat.ID, "This link is personal. Open it in a private chat with the bot.")
			return
		}
		// The channel's label is the group's own title; a titleless group
		// stores NULL, so the row falls back to the target.
		if _, err := tx.Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, kind, target, label)
			 SELECT gen_random_uuid(), $1, 'telegram', $2, $3
			  WHERE NOT EXISTS (SELECT 1 FROM alert_channel
			                    WHERE tenant_id = $1 AND kind = 'telegram' AND target = $2)`,
			tenantID, strconv.FormatInt(msg.Chat.ID, 10), nullableLabel(msg.Chat.Title)); err != nil {
			b.log.Warn("telegram: could not add group channel", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this group. Try the link again.")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			b.log.Warn("telegram: group connect commit failed", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this group. Try the link again.")
			return
		}
		b.send(msg.Chat.ID, "This group now receives incident alerts for "+tenantName+". Anyone on the project can press the buttons; anyone else is told they are not connected.")
		b.log.Info("telegram group connected", "tenant_id", tenantID, "chat_id", msg.Chat.ID)
		return
	}

	// A BOUND link (person_id set) links the account to that person; an
	// UNBOUND link finds-or-creates by telegram_id. The one merge is the fork
	// an unbound redeem leaves behind (mergeFork), never two real accounts.
	name := strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
	var personID int64
	if invitePersonID != nil {
		personID = *invitePersonID
		// Refusal one, or a merge: another person row may hold this
		// telegram_id. A row is a FORK only when telegram_id is its ONLY
		// identity — person carries e-mail, telegram_id AND google_sub, and
		// the table's CHECK lets a google_sub row live without an e-mail, so
		// either one is a login identity somebody signs in with and is never
		// absorbed. A tenant_member row ANYWHERE, this tenant included, says
		// the same thing: that row is a live teammate, and absorbing it would
		// delete their person row, cascade their membership and session, and
		// hand their Telegram to the invitee — who would then sign in AS them
		// through /v1/auth/telegram, which resolves a session by telegram_id
		// alone. What is left is the ORPHAN: deleteRecipient drops the
		// membership, both channels, the unused invites and the sessions and
		// deliberately keeps the person row, so the fork this merge recovers
		// from has ZERO memberships anywhere — and since telegram_id is
		// UNIQUE, refusing that one would leave the account permanently
		// unlinkable. It costs the reader a step: after an unbound redeem
		// forked them, they remove the duplicate from the team FIRST, and
		// only then does Link Telegram absorb the leftover row. That step is
		// deliberate — silently absorbing a live teammate is the worse answer.
		var holderID int64
		var holderHasLogin, holderIsMember bool
		holderErr := tx.QueryRow(ctx,
			`SELECT p.id, p.email IS NOT NULL OR p.google_sub IS NOT NULL,
			        EXISTS (SELECT 1 FROM tenant_member tm WHERE tm.person_id = p.id)
			   FROM person p WHERE p.telegram_id = $1 AND p.id <> $2`,
			msg.From.ID, personID).Scan(&holderID, &holderHasLogin, &holderIsMember)
		switch {
		case holderErr == nil && (holderHasLogin || holderIsMember):
			b.send(msg.Chat.ID, "This Telegram account already belongs to someone else on the project.")
			return
		case holderErr == nil:
			if err := mergeFork(ctx, tx, tenantID, holderID, personID); err != nil {
				b.log.Warn("telegram: fork merge failed", "err", err,
					"tenant_id", tenantID, "person_id", personID, "merged_person_id", holderID)
				b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
				return
			}
			b.log.Info("telegram fork merged into the invited person",
				"tenant_id", tenantID, "person_id", personID, "merged_person_id", holderID)
		case !errors.Is(holderErr, pgx.ErrNoRows):
			b.log.Warn("telegram: bound person telegram read failed", "err", holderErr)
			b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
			return
		}
		// Refusal two: the bound person already has a DIFFERENT Telegram — a
		// second link would silently move their alerts to whoever redeems it.
		var boundTG *int64
		if err := tx.QueryRow(ctx,
			`SELECT telegram_id FROM person WHERE id = $1`, personID).Scan(&boundTG); err != nil {
			b.log.Warn("telegram: bound person read failed", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
			return
		}
		if boundTG != nil && *boundTG != msg.From.ID {
			b.send(msg.Chat.ID, "That person already has a different Telegram account linked.")
			return
		}
		// Bind the account, keep the username current, fill a name only when
		// the person has none.
		if _, err := tx.Exec(ctx,
			`UPDATE person
			    SET telegram_id = $1, telegram_username = $2,
			        name = CASE WHEN name = '' THEN $3 ELSE name END
			  WHERE id = $4`,
			msg.From.ID, msg.From.Username, name, personID); err != nil {
			b.log.Warn("telegram: person link failed", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
			return
		}
		// No membership INSERT: the mint already resolved membership; a link
		// is not a way to re-role anybody. No status write either: a Telegram
		// redeem proves control of a Telegram account and says nothing about
		// an address, while status is the field createChannel gates e-mail
		// channels on. The seat this once repaired is counted where the axis
		// is defined instead — countTelegramRecipients filters on no status.
	} else {
		personID, err = b.personByTelegramID(ctx, tx, msg.From)
		if err != nil {
			b.log.Warn("telegram: person link failed", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
			return
		}
		// Existing members keep the role they have (ON CONFLICT DO NOTHING);
		// a re-invite is not a way to re-role anybody.
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, $3, 'active')
			 ON CONFLICT (tenant_id, person_id) DO NOTHING`, tenantID, personID, role); err != nil {
			b.log.Warn("telegram: member insert failed", "err", err)
		}
	}
	// The channel's label is the person's name plus @username; neither stores
	// NULL, so the row falls back to the target.
	if _, err := tx.Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, recipient_person_id, label)
		 SELECT gen_random_uuid(), $1, 'telegram', $2, $3, $4
		  WHERE NOT EXISTS (SELECT 1 FROM alert_channel
		                    WHERE tenant_id = $1 AND kind = 'telegram' AND target = $2)`,
		tenantID, strconv.FormatInt(msg.Chat.ID, 10), personID, nullableLabel(inviteLabel(name, msg.From.Username))); err != nil {
		b.log.Warn("telegram: could not add channel", "err", err)
		b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		b.log.Warn("telegram: chat connect commit failed", "err", err)
		b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
		return
	}
	b.sendWithApp(msg.Chat.ID, "Connected. Alerts for "+tenantName+" will arrive here — and the buttons on them work from this chat.")
	b.log.Info("telegram chat connected", "tenant_id", tenantID, "person_id", personID)
}

// mergeFork folds a telegram-only person row into the person the invite is
// bound to, inside the caller's transaction. Seven FKs reference person(id):
// tenant_member.person_id, session.person_id, telegram_invite.invited_by and
// telegram_invite.person_id all CASCADE and leave with the row, and
// web_visitor.person_id is ON DELETE SET NULL, so the analytics row survives
// the DELETE and reads as anonymous again. The merge does NOT carry that
// identity across: web_events.person_id holds the same human with no FK and
// nothing to null it, so moving only the directory row would leave the two
// disagreeing, and admin's only reader asks whether person_id is set, not
// which person it names. The two that block the DELETE outright are
// incident.acked_by and alert_channel.recipient_person_id — no action at all,
// so each is dealt with by hand below.
//
// An invite grants authority over ONE tenant, so identity does not follow the
// fork into tenants the invite never named — deleteRecipient drops a member's
// tenant_member row and telegram channel while leaving their acks behind, so
// a fork CAN still hold rows in a tenant this invite says nothing about.
// Inside tenantID rows are repointed at the survivor; outside it the channel
// is deleted and the ack is NULLed, each for the reason at its statement.
func mergeFork(ctx context.Context, tx pgx.Tx, tenantID, forkID, keepID int64) error {
	// Outside the invite's tenant the destination is DELETED, not NULLed: a
	// NULL recipient_person_id is not "unowned", it is "broadcast group" —
	// what the delivery worker reads it as (payload.Group) — so NULLing would
	// silently re-type somebody's private chat into a group destination
	// instead of merely unblocking the DELETE. A private destination whose
	// person row is going away has no owner left, and deleteRecipient already
	// drops exactly this shape for exactly that reason.
	if _, err := tx.Exec(ctx,
		`DELETE FROM alert_channel WHERE recipient_person_id = $1 AND tenant_id <> $2`,
		forkID, tenantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE alert_channel SET recipient_person_id = $2
		  WHERE recipient_person_id = $1 AND tenant_id = $3`,
		forkID, keepID, tenantID); err != nil {
		return err
	}
	// Outside the invite's tenant this is a destructive write into a second
	// tenant's incident history, decided by a link that tenant's owner never
	// saw. NULL is still the only honest answer: the person who acked is being
	// deleted, so the ack keeps its timestamp and loses its name — attributing
	// it to somebody from another tenant would be worse. acked_by has no
	// reader in back/ today; only this statement and the ack button write it.
	if _, err := tx.Exec(ctx,
		`UPDATE incident SET acked_by = CASE WHEN tenant_id = $3 THEN $2::bigint END
		  WHERE acked_by = $1`,
		forkID, keepID, tenantID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM person WHERE id = $1`, forkID)
	return err
}

// personByTelegramID finds-or-creates the person by telegram_id inside the
// caller's transaction, keeping telegram_username current.
func (b *bot) personByTelegramID(ctx context.Context, tx pgx.Tx, from tgUser) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM person WHERE telegram_id = $1`, from.ID).Scan(&id)
	name := strings.TrimSpace(from.FirstName + " " + from.LastName)
	if err == nil {
		// Keep the display name fresh (the timeline names this person); an
		// existing name is never overwritten by a possibly-sparser one.
		_, _ = tx.Exec(ctx,
			`UPDATE person
			    SET name = CASE WHEN name = '' THEN $2 ELSE name END,
			        telegram_username = $3
			  WHERE id = $1`,
			id, name, from.Username)
		return id, nil
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO person (public_id, telegram_id, telegram_username, name)
		 VALUES (gen_random_uuid(), $1, $2, $3) RETURNING id`,
		from.ID, from.Username, name).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// inviteLabel builds the label a channel row shows on Alerts: name plus
// " @username"; neither falls back through nullableLabel.
func inviteLabel(name, username string) string {
	if username == "" {
		return name
	}
	if name == "" {
		return "@" + username
	}
	return name + " @" + username
}

// nullableLabel stores the label as NULL, not "", when nothing was computed:
// the front's `label ?? target` does not fall back on an empty string.
func nullableLabel(label string) any {
	if label != "" {
		return label
	}
	return nil
}

// memberForChat resolves the presser AND the chat's tenant in one round trip:
// the presser must be a member of the tenant whose alerts land here.
func (b *bot) memberForChat(ctx context.Context, chatID, fromID int64) (personID, tenantID int64, name string) {
	err := b.pool.Raw().QueryRow(ctx,
		`SELECT p.id, ac.tenant_id, p.name
		   FROM alert_channel ac
		   JOIN person p ON p.telegram_id = $2
		   JOIN tenant_member tm ON tm.person_id = p.id AND tm.tenant_id = ac.tenant_id
		  WHERE ac.kind = 'telegram' AND ac.target = $1 LIMIT 1`,
		strconv.FormatInt(chatID, 10), fromID).Scan(&personID, &tenantID, &name)
	if err != nil {
		return 0, 0, ""
	}
	return personID, tenantID, name
}

// handleHelp answers /help: the command list plus the Open Mini App button.
func (b *bot) handleHelp(msg *tgMessage) {
	b.sendWithApp(msg.Chat.ID,
		"What this bot does:\n"+
			"/status — your checks and any open incidents\n"+
			"/mute <30m|2h|1d> — silence alerts for a while\n"+
			"/unmute — lift the mute early\n"+
			"/stop — disconnect this chat for good\n"+
			"/id — your Telegram id (for support)\n"+
			"/help — this message\n\n"+
			"Acknowledge and Resolve buttons arrive on the alerts themselves.")
}

// handleStatus answers /status; only a member of the chat's tenant gets an
// answer, anyone else learns nothing.
func (b *bot) handleStatus(ctx context.Context, msg *tgMessage) {
	_, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, notConnected)
		return
	}
	rows, err := b.pool.Raw().Query(ctx,
		`SELECT mf.status, count(*), array_agg(m.name ORDER BY m.name) FILTER (WHERE mf.status <> 'ok')
		   FROM monitor m LEFT JOIN monitor_facts mf ON mf.monitor_id = m.id
		  WHERE m.tenant_id = $1 AND m.paused = false
		  GROUP BY mf.status`, tenantID)
	if err != nil {
		b.send(msg.Chat.ID, "Could not read the checks right now.")
		return
	}
	defer rows.Close()
	var up, failing, nodata int
	var broken []string
	for rows.Next() {
		var status *string
		var n int
		var names []byte
		if rows.Scan(&status, &n, &names) != nil {
			continue
		}
		switch {
		case status == nil || *status == "nodata":
			nodata += n
		case *status == "ok":
			up += n
		default:
			failing += n
			broken = append(broken, namesFrom(names)...)
		}
	}
	// Incidents are their own question: a detector incident has no monitor,
	// so zero checks can still be on fire.
	incidents, incidentsOK := b.openIncidentTitles(ctx, tenantID)
	switch {
	case up+failing+nodata == 0:
		b.send(msg.Chat.ID, "No checks yet — nothing is being monitored."+incidentsLine(incidents, incidentsOK))
	case failing == 0 && nodata == 0:
		b.send(msg.Chat.ID, "All "+strconv.Itoa(up)+" checks are up."+incidentsLine(incidents, incidentsOK))
	default:
		parts := []string{strconv.Itoa(up) + " up"}
		if failing > 0 {
			parts = append(parts, strconv.Itoa(failing)+" down: "+strings.Join(broken, ", "))
		}
		if nodata > 0 {
			parts = append(parts, strconv.Itoa(nodata)+" not yet checked")
		}
		b.send(msg.Chat.ID, strings.Join(parts, " · ")+incidentsLine(incidents, incidentsOK))
	}
}

// openIncidentTitles reads the still-open incidents; ok=false means the read
// failed, which /status says rather than implying "there are none".
func (b *bot) openIncidentTitles(ctx context.Context, tenantID int64) (titles []string, ok bool) {
	rows, err := b.pool.Raw().Query(ctx,
		`SELECT title FROM incident
		  WHERE tenant_id = $1 AND resolved_at IS NULL
		  ORDER BY detected_at DESC`, tenantID)
	if err == nil {
		titles, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil {
		b.log.Warn("telegram: open incident read failed", "err", err, "tenant_id", tenantID)
		return nil, false
	}
	return titles, true
}

// incidentsLine renders the open-incident tail: nothing when none, the three
// newest otherwise. A failed read says so, never silence.
func incidentsLine(titles []string, ok bool) string {
	if !ok {
		return "\nCould not read the incident list."
	}
	if len(titles) == 0 {
		return ""
	}
	shown := titles
	extra := ""
	if len(titles) > 3 {
		shown = titles[:3]
		extra = fmt.Sprintf(" (+%d more)", len(titles)-3)
	}
	return "\nOpen incidents: " + strings.Join(shown, "; ") + extra
}

// ponytail: monitor names containing commas would split wrong — cosmetic
// display only; switch to array_to_string server-side if that ever bites.
func namesFrom(arr []byte) []string {
	s := strings.Trim(string(arr), "{}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}
	return parts
}

// handleMute silences this chat's alerts; the window lives on muted_until and
// the delivery worker reschedules around it, expiry needs no bot involvement.
func (b *bot) handleMute(ctx context.Context, msg *tgMessage, arg string) {
	d, err := parseMuteDuration(arg)
	if err != nil {
		b.send(msg.Chat.ID, "How long? Try /mute 30m, /mute 2h or /mute 1d (up to 7d).")
		return
	}
	personID, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, notConnected)
		return
	}
	until := time.Now().UTC().Add(d)
	if _, err := b.pool.Raw().Exec(ctx,
		`UPDATE alert_channel SET muted_until = $1
		  WHERE tenant_id = $2 AND kind = 'telegram'
		    AND (recipient_person_id = $3 OR target = $4)`,
		until, tenantID, personID, strconv.FormatInt(msg.Chat.ID, 10)); err != nil {
		b.send(msg.Chat.ID, "Could not set the mute window. Try again.")
		return
	}
	b.send(msg.Chat.ID, "Muted until "+until.Format("15:04 MST")+" ("+arg+") — alerts resume automatically after that.")
}

// handleUnmute lifts the mute early AND un-parks what the window deferred:
// the worker defers muted alerts to the mute's end, it does not drop them.
func (b *bot) handleUnmute(ctx context.Context, msg *tgMessage) {
	personID, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, notConnected)
		return
	}
	// Release keyed on the window, not on "scheduled in the future": failed
	// sends also sit in the future on retry backoff and must not be dragged.
	var lifted int
	if err := b.pool.Raw().QueryRow(ctx,
		`WITH muted AS (
		   SELECT id, muted_until FROM alert_channel
		    WHERE tenant_id = $1 AND kind = 'telegram'
		      AND (recipient_person_id = $2 OR target = $3)
		      AND muted_until IS NOT NULL
		 ), cleared AS (
		   UPDATE alert_channel SET muted_until = NULL
		    WHERE id IN (SELECT id FROM muted)
		 ), released AS (
		   UPDATE delivery_queue d SET next_try_at = now()
		     FROM muted m
		    WHERE d.channel_id = m.id AND d.state = 'pending'
		      AND d.leased_by IS NULL AND d.class <> 'followup'
		      AND d.next_try_at >= m.muted_until
		 )
		 SELECT count(*) FROM muted`,
		tenantID, personID, strconv.FormatInt(msg.Chat.ID, 10)).Scan(&lifted); err != nil {
		b.send(msg.Chat.ID, "Could not lift the mute. Try again.")
		return
	}
	if lifted == 0 {
		b.send(msg.Chat.ID, "Nothing was muted here — alerts are already arriving.")
		return
	}
	b.send(msg.Chat.ID, "Unmuted — alerts flow again.")
}

// handleStop disconnects THIS chat: the destination goes, membership and
// role stay. A fresh invite link brings the chat back.
func (b *bot) handleStop(ctx context.Context, msg *tgMessage) {
	_, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, notConnected)
		return
	}
	// No row-count branch: memberForChat only answers when this row exists;
	// if it vanished in between, the chat is disconnected either way.
	if _, err := b.pool.Raw().Exec(ctx,
		`DELETE FROM alert_channel
		  WHERE tenant_id = $1 AND kind = 'telegram' AND target = $2`,
		tenantID, strconv.FormatInt(msg.Chat.ID, 10)); err != nil {
		b.send(msg.Chat.ID, "Could not disconnect this chat. Try again.")
		return
	}
	b.send(msg.Chat.ID, "Disconnected. No more alerts arrive here. You are still on the project — a fresh invite link from the Alerts screen brings this chat back.")
	b.log.Info("telegram chat disconnected", "tenant_id", tenantID, "chat_id", msg.Chat.ID)
}

// parseMuteDuration accepts <n>m, <n>h, <n>d up to 7 days.
func parseMuteDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(strings.TrimRight(s, "mhd"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad number")
	}
	var d time.Duration
	switch unit {
	case 'm':
		d = time.Duration(n) * time.Minute
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	default:
		return 0, fmt.Errorf("bad unit")
	}
	if d > 7*24*time.Hour {
		return 0, fmt.Errorf("too long")
	}
	return d, nil
}

// handleCallback processes button presses; the data is "action:<public_id>",
// the same id the alert payload carried.
func (b *bot) handleCallback(ctx context.Context, cb *tgCallback) {
	action, pubID, ok := parseCallback(cb.Data)
	if !ok {
		b.answerCallback(cb.ID, "Unknown action")
		return
	}
	// Authorisation is the person, not the chat: a forwarded message or a
	// stranger pressing a screenshot's button stops here.
	personID, tenantID, name := b.memberForChat(ctx, cb.Message.Chat.ID, cb.From.ID)
	if personID == 0 {
		b.answerCallback(cb.ID, "Your Telegram account is not connected to this project.")
		return
	}
	// The id in the payload is attacker-controlled: the incident must belong
	// to this tenant, whatever the button claims.
	incID, ok := b.incidentByPublicID(ctx, pubID, tenantID)
	if !ok {
		b.answerCallback(cb.ID, "That incident is not yours.")
		return
	}

	switch action {
	case "ack":
		b.handleAck(ctx, cb, incID, personID, name)
	case "resolve":
		b.handleResolve(ctx, cb, incID, personID, name)
	default:
		b.answerCallback(cb.ID, "Unknown action: "+action)
	}
}

// handleAck records the incident's MTTA mark, idempotent; the timeline
// names the person: "who closed it" is a fact.
func (b *bot) handleAck(ctx context.Context, cb *tgCallback, incID, personID int64, name string) {
	_, _ = b.pool.Raw().Exec(ctx,
		`UPDATE incident SET acked_at = now(), acked_by = $2 WHERE id = $1 AND acked_at IS NULL`,
		incID, personID)
	_, _ = b.pool.Raw().Exec(ctx,
		`INSERT INTO incident_update (incident_id, kind, text) VALUES ($1, 'acked', $2)`,
		incID, "Acknowledged from Telegram by "+name)
	b.answerCallback(cb.ID, "✅ Incident acknowledged.")
	b.log.Info("incident acked via telegram", "incident_id", incID, "person_id", personID)
}

// handleResolve closes through the same lifecycle the detector uses, so the
// close reason and resolved_at are written the one way.
func (b *bot) handleResolve(ctx context.Context, cb *tgCallback, incID, personID int64, name string) {
	var monitorID *int64
	if err := b.pool.Raw().QueryRow(ctx,
		`SELECT monitor_id FROM incident WHERE id = $1`, incID).Scan(&monitorID); err != nil || monitorID == nil {
		b.answerCallback(cb.ID, "That incident cannot be closed from here.")
		return
	}
	if b.incidents == nil {
		b.answerCallback(cb.ID, "Closing is unavailable right now.")
		return
	}
	if err := b.incidents.Close(ctx, *monitorID, incident.ReasonByHuman); err != nil {
		b.answerCallback(cb.ID, "Could not close it — it is still open.")
		return
	}
	_, _ = b.pool.Raw().Exec(ctx,
		`INSERT INTO incident_update (incident_id, kind, text) VALUES ($1, 'resolved', $2)`,
		incID, "Resolved from Telegram by "+name)
	b.answerCallback(cb.ID, "✅ Incident resolved.")
	b.log.Info("incident resolved via telegram", "incident_id", incID, "person_id", personID)
}

// sha256Sum mirrors the storage convention of every one-time token here:
// sha256 is stored, never the raw value.
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// InviteTokenHash is THE hash of an invite token, shared by the mint and
// redeem sides; the input is the FULL deep-link payload, prefix included.
func InviteTokenHash(payload string) []byte {
	return sha256Sum(payload)
}

// parseCallback splits "action:id"; ok=false for anything malformed, answered
// as "Unknown action" so crafted payloads never reach the mutation code.
func parseCallback(data string) (action, id string, ok bool) {
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// incidentByPublicID resolves the callback's uuid-hex payload to an incident
// of THIS tenant, or reports that it is not theirs.
func (b *bot) incidentByPublicID(ctx context.Context, pubHex string, tenantID int64) (int64, bool) {
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != 16 {
		return 0, false
	}
	var id int64
	err = b.pool.Raw().QueryRow(ctx,
		`SELECT id FROM incident WHERE public_id = $1 AND tenant_id = $2`, raw, tenantID).Scan(&id)
	return id, err == nil
}

func (b *bot) send(chatID int64, text string) {
	b.sendWithKeyboard(chatID, text, nil)
}

// sendWithApp adds the Open Mini App button when an app URL is configured.
func (b *bot) sendWithApp(chatID int64, text string) {
	if b.appURL == "" {
		b.send(chatID, text)
		return
	}
	kb := map[string]any{
		"inline_keyboard": [][]map[string]any{{
			{"text": "Open app", "web_app": map[string]string{"url": b.appURL + "/app"}},
		}},
	}
	b.sendWithKeyboard(chatID, text, kb)
}

func (b *bot) sendWithKeyboard(chatID int64, text string, replyMarkup map[string]any) {
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	_ = b.call("sendMessage", body)
}
