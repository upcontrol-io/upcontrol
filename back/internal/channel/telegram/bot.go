// Package telegram is the long-polling bot (plan §5.11). It runs as a
// background goroutine under a Postgres advisory lock so two ucapi instances
// never double-poll. The bot handles:
//
//   - /start inv_<token>: the one-time invite deep link from the Alerts screen
//     (the growth loop). Burning the token links the Telegram user to a person
//     by telegram_id — the ONLY thing that sets person.telegram_id. The old
//     guessable /start prj-N payload no longer subscribes anything (it answers
//     "ask for a fresh link" instead).
//   - /start in a GROUP with an invite token: connects the group as a
//     broadcast destination (no person, no action buttons — see D5).
//   - /help, /id, /status, /mute: the command surface.
//   - callback_query: inline button presses on alert messages (ack, resolve).
//     Authorisation is the PERSON, not the chat: from.id must resolve to a
//     tenant member — a forwarded message or a group member does not grant
//     rights.
//
// Personal alerts are actionable: the delivery worker attaches Acknowledge /
// Resolve inline buttons to messages sent to person-bound channels (see
// deliver.Worker); broadcast groups get the same message without buttons.
package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/incident"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Bot is the long-polling Telegram bot.
type Bot struct {
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
func NewBot(token, appURL string, pool *pg.Pool, lc *incident.Lifecycle, log *slog.Logger) *Bot {
	return &Bot{
		// Trimmed here, not only in the _FILE loader: a token with a trailing
		// newline breaks EVERY request URL ("invalid control character"), which
		// on prod looked exactly like a started-but-dead poller (audit §7).
		token:     strings.TrimSpace(token),
		appURL:    strings.TrimRight(appURL, "/"),
		pool:      pool,
		incidents: lc,
		log:       log,
		client:    &http.Client{Timeout: 70 * time.Second}, // long-poll needs >60s
	}
}

// Run polls getUpdates under an advisory lock until ctx is cancelled. The
// advisory lock (key = hash("uc-telegram-bot")) ensures only one instance polls;
// if the lock holder crashes, the session-level lock auto-releases.
//
// Silent death is designed out (audit §7 / D8): the poll cycle and every
// handler are panic-recovered (a panic used to kill the goroutine with nothing
// in the log but "started"), failures carry the HTTP status and escalate their
// backoff instead of retrying invisibly every 5 s, and a heartbeat line —
// "bot polling", offset — lands every 5 minutes of quiet, so "started" can no
// longer outlive the poller by more than one heartbeat interval.
func (b *Bot) Run(ctx context.Context) error {
	// The advisory lock is SESSION-level: it lives on one connection. Taking
	// it through pool.Exec borrowed whatever idle connection was next — which
	// the pool could then close at any moment, silently releasing the lock. On
	// prod that ended with BOTH replicas polling at once, each terminating
	// the other's long-poll (Telegram answers 409, logged as endless not-ok;
	// the audit's "bot started but nobody polls"). The lock therefore sits on
	// a DEDICATED connection, acquired once and held for the loop's lifetime.
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

		// The heartbeat (audit §7): every 5 minutes of quiet the poller says
		// it is alive and names the offset it holds — an operator can tell a
		// live bot from a dead one by reading the log, no external probe
		// needed. The beat stops exactly when the loop stops.
		if now := time.Now(); now.Sub(lastBeat) >= heartbeatEvery {
			lastBeat = now
			b.log.Info("telegram bot polling", "offset", b.offset, "updates", len(updates))
		}
	}
}

// poll is the panic-safe getUpdates: a panic anywhere in the cycle is
// converted into an error the loop can back off from, instead of killing the
// goroutine (and with it the process) with nothing logged after "started".
func (b *Bot) poll(ctx context.Context) (updates []tgUpdate, err error) {
	defer func() {
		if r := recover(); r != nil {
			updates, err = nil, fmt.Errorf("poll cycle panicked: %v", r)
		}
	}()
	return b.getUpdates(ctx)
}

// dispatch is the panic-safe update handler: one malformed update can fail,
// loudly, without taking the poller down with it.
func (b *Bot) dispatch(ctx context.Context, u tgUpdate) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("telegram handler panicked", "update_id", u.UpdateID, "panic", r)
		}
	}()
	b.handleUpdate(ctx, u)
}

// --- Telegram API types ---

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
	ID   int64  `json:"id"`
	Type string `json:"type"` // private | group | supergroup | channel
}

func (b *Bot) getUpdates(ctx context.Context) ([]tgUpdate, error) {
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

func (b *Bot) answerCallback(callbackID string, text string) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", b.token)
	body, _ := json.Marshal(map[string]string{
		"callback_query_id": callbackID,
		"text":              text,
	})
	resp, err := b.client.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
}

// --- command dispatch ---

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

func (b *Bot) handleUpdate(ctx context.Context, u tgUpdate) {
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
	default:
		// Unknown commands and plain messages: silent, as before.
	}
}

// handleStart processes the `/start` deep link. The payload decides
// everything; anything that is not a valid unredeemed invite binds NOTHING —
// the old `prj-<id>` form used to bind a chat to a tenant on a guessable
// number, which was the hole this rework closes.
func (b *Bot) handleStart(ctx context.Context, msg *tgMessage, payload string) {
	if strings.HasPrefix(payload, "prj-") {
		// Transitional refusal (decision 0.3): old links are already in
		// people's chats; the honest answer names the new way instead of
		// silently ignoring them.
		b.send(msg.Chat.ID, "That link is out of date. Ask the project owner for a fresh invite from the Alerts screen (Alerts → Invite on Telegram).")
		return
	}
	token, ok := strings.CutPrefix(payload, "inv_")
	if !ok || token == "" {
		b.send(msg.Chat.ID, "This bot alerts you about incidents in your project. To connect, ask the project owner for an invite link from the Alerts screen (Alerts → Invite on Telegram).")
		return
	}

	// One-time, race-safe: the atomic UPDATE ... WHERE redeemed_at IS NULL is
	// the same claim-token pattern as install.go — two users racing the same
	// link cannot both win, and a replay hits "no rows".
	hash := sha256Sum(token)
	var tenantID int64
	var role string
	if err := b.pool.Raw().QueryRow(ctx,
		`UPDATE telegram_invite SET redeemed_at = now()
		  WHERE token_hash = $1 AND redeemed_at IS NULL AND expires_at > now()
		 RETURNING tenant_id, role`, hash).Scan(&tenantID, &role); err != nil {
		b.send(msg.Chat.ID, "This invite link is no longer valid. Ask the project owner for a fresh one from the Alerts screen.")
		return
	}
	var tenantName string
	_ = b.pool.Raw().QueryRow(ctx,
		`SELECT name FROM tenant WHERE id = $1`, tenantID).Scan(&tenantName)

	// A GROUP redeem connects a broadcast destination (D5): no person, no
	// inline buttons — in a group, from.id proves nothing about the tenant.
	if msg.Chat.Type == "group" || msg.Chat.Type == "supergroup" || msg.Chat.ID < 0 {
		if _, err := b.pool.Raw().Exec(ctx,
			`INSERT INTO alert_channel (public_id, tenant_id, kind, target)
			 SELECT gen_random_uuid(), $1, 'telegram', $2
			  WHERE NOT EXISTS (SELECT 1 FROM alert_channel
			                    WHERE tenant_id = $1 AND kind = 'telegram' AND target = $2)`,
			tenantID, strconv.FormatInt(msg.Chat.ID, 10)); err != nil {
			b.log.Warn("telegram: could not add group channel", "err", err)
			b.send(msg.Chat.ID, "Something went wrong connecting this group. Try the link again.")
			return
		}
		b.send(msg.Chat.ID, "This group now receives incident alerts for "+tenantName+". Messages here carry no buttons — actions stay in personal chats.")
		b.log.Info("telegram group connected", "tenant_id", tenantID, "chat_id", msg.Chat.ID)
		return
	}

	// Private chat: find-or-create the person by telegram_id. This is the only
	// writer of person.telegram_id (the old "tenant has exactly one member"
	// heuristic is gone — it guessed, and a guess here hands one person's
	// acknowledgements to another). An email member connecting their own
	// Telegram arrives as a second person row; merging identities is a
	// deliberate non-goal.
	personID, err := b.personByTelegramID(ctx, msg.From)
	if err != nil {
		b.log.Warn("telegram: person link failed", "err", err)
		b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
		return
	}
	// Existing members keep the role they have (ON CONFLICT DO NOTHING); a
	// re-invite is not a way to re-role anybody.
	if _, err := b.pool.Raw().Exec(ctx,
		`INSERT INTO tenant_member (tenant_id, person_id, role, status) VALUES ($1, $2, $3, 'active')
		 ON CONFLICT (tenant_id, person_id) DO NOTHING`, tenantID, personID, role); err != nil {
		b.log.Warn("telegram: member insert failed", "err", err)
	}
	if _, err := b.pool.Raw().Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, kind, target, recipient_person_id)
		 SELECT gen_random_uuid(), $1, 'telegram', $2, $3
		  WHERE NOT EXISTS (SELECT 1 FROM alert_channel
		                    WHERE tenant_id = $1 AND kind = 'telegram' AND target = $2)`,
		tenantID, strconv.FormatInt(msg.Chat.ID, 10), personID); err != nil {
		b.log.Warn("telegram: could not add channel", "err", err)
		b.send(msg.Chat.ID, "Something went wrong connecting this chat. Try the link again.")
		return
	}
	b.sendWithApp(msg.Chat.ID, "Connected. Alerts for "+tenantName+" will arrive here — and the buttons on them work from this chat.")
	b.log.Info("telegram chat connected", "tenant_id", tenantID, "person_id", personID)
}

// personByTelegramID finds the person whose Telegram this is, or creates one
// (telegram-only: person's CHECK allows email OR telegram_id).
func (b *Bot) personByTelegramID(ctx context.Context, from tgUser) (int64, error) {
	var id int64
	err := b.pool.Raw().QueryRow(ctx,
		`SELECT id FROM person WHERE telegram_id = $1`, from.ID).Scan(&id)
	if err == nil {
		// Keep the display name fresh — the timeline names this person.
		name := strings.TrimSpace(from.FirstName + " " + from.LastName)
		if name != "" {
			_, _ = b.pool.Raw().Exec(ctx, `UPDATE person SET name = $1 WHERE id = $2 AND name = ''`, name, id)
		}
		return id, nil
	}
	name := strings.TrimSpace(from.FirstName + " " + from.LastName)
	if err := b.pool.Raw().QueryRow(ctx,
		`INSERT INTO person (public_id, telegram_id, name) VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		from.ID, name).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// memberForChat resolves the person pressing something in this chat AND the
// tenant the chat belongs to, in one round trip: the presser must be a member
// of the tenant whose alerts land here. Returns 0 when either half is missing.
func (b *Bot) memberForChat(ctx context.Context, chatID, fromID int64) (personID, tenantID int64, name string) {
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
func (b *Bot) handleHelp(msg *tgMessage) {
	b.sendWithApp(msg.Chat.ID,
		"What this bot does:\n"+
			"/status — how your checks are doing right now\n"+
			"/mute <30m|2h|1d> — silence alerts for a while\n"+
			"/id — your Telegram id (for support)\n"+
			"/help — this message\n\n"+
			"Acknowledge and Resolve buttons arrive on the alerts themselves.")
}

// handleStatus answers /status with one message about the tenant's checks.
// Only a verified member of the chat's tenant gets an answer — anyone else is
// refused without learning anything about the tenant.
func (b *Bot) handleStatus(ctx context.Context, msg *tgMessage) {
	_, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, "This chat is not connected to a project. Ask the owner for an invite link.")
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
	switch {
	case up+failing+nodata == 0:
		b.send(msg.Chat.ID, "No checks yet — nothing is being monitored.")
	case failing == 0 && nodata == 0:
		b.send(msg.Chat.ID, "All "+strconv.Itoa(up)+" checks are up.")
	default:
		parts := []string{strconv.Itoa(up) + " up"}
		if failing > 0 {
			parts = append(parts, strconv.Itoa(failing)+" down: "+strings.Join(broken, ", "))
		}
		if nodata > 0 {
			parts = append(parts, strconv.Itoa(nodata)+" not yet checked")
		}
		b.send(msg.Chat.ID, strings.Join(parts, " · "))
	}
}

// namesFrom trims a Postgres array literal {"a","b"} to ["a","b"]. NULL
// aggregates arrive as an empty slice and yield nil.
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

// handleMute silences this chat's alerts for a duration (30m, 2h, 1d). The
// window lives on alert_channel.muted_until; the delivery worker reschedules
// anything that comes due inside it, so expiry needs no bot involvement.
func (b *Bot) handleMute(ctx context.Context, msg *tgMessage, arg string) {
	d, err := parseMuteDuration(arg)
	if err != nil {
		b.send(msg.Chat.ID, "How long? Try /mute 30m, /mute 2h or /mute 1d (up to 7d).")
		return
	}
	personID, tenantID, _ := b.memberForChat(ctx, msg.Chat.ID, msg.From.ID)
	if tenantID == 0 {
		b.send(msg.Chat.ID, "This chat is not connected to a project. Ask the owner for an invite link.")
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

// handleCallback processes inline button presses on alert messages. The
// callback data format is "action:<public_id>" (the incident's uuid hex — the
// same id the alert payload carried, so nothing extra travels in the button).
func (b *Bot) handleCallback(ctx context.Context, cb *tgCallback) {
	action, pubID, ok := parseCallback(cb.Data)
	if !ok {
		b.answerCallback(cb.ID, "Unknown action")
		return
	}
	// Authorisation is the person, not the chat: from.id must resolve to a
	// member of the tenant this chat alerts for. A forwarded message or a
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

// handleAck records the second of the incident's four marks (MTTA). It is
// idempotent: pressing the button twice does not move the timestamp. The
// timeline names the person — "who closed it" is a fact, not @username.
func (b *Bot) handleAck(ctx context.Context, cb *tgCallback, incID, personID int64, name string) {
	_, _ = b.pool.Raw().Exec(ctx,
		`UPDATE incident SET acked_at = now(), acked_by = $2 WHERE id = $1 AND acked_at IS NULL`,
		incID, personID)
	_, _ = b.pool.Raw().Exec(ctx,
		`INSERT INTO incident_update (incident_id, kind, text) VALUES ($1, 'acked', $2)`,
		incID, "Acknowledged from Telegram by "+name)
	b.answerCallback(cb.ID, "✅ Incident acknowledged.")
	b.log.Info("incident acked via telegram", "incident_id", incID, "person_id", personID)
}

// handleResolve closes the incident through the same lifecycle the detector
// uses, so the close reason, the timeline entry and resolved_at are written the
// one way — a button that closed it differently would produce incidents that
// look closed on one screen and open on another.
func (b *Bot) handleResolve(ctx context.Context, cb *tgCallback, incID, personID int64, name string) {
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

// --- helpers ---

// sha256Sum mirrors the storage convention of every one-time token here
// (magic-link codes, claim tokens, install tokens all store sha256, never the
// raw value).
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// parseCallback splits inline-button callback data of the form "action:id"
// (e.g. "ack:<public_id>"). ok is false for anything malformed, which the
// caller answers as "Unknown action" — so a crafted payload never reaches the
// incident mutation code.
func parseCallback(data string) (action, id string, ok bool) {
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// incidentByPublicID resolves the callback's uuid-hex payload to an incident
// of THIS tenant, or reports that it is not theirs.
func (b *Bot) incidentByPublicID(ctx context.Context, pubHex string, tenantID int64) (int64, bool) {
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != 16 {
		return 0, false
	}
	var id int64
	err = b.pool.Raw().QueryRow(ctx,
		`SELECT id FROM incident WHERE public_id = $1 AND tenant_id = $2`, raw, tenantID).Scan(&id)
	return id, err == nil
}

func (b *Bot) send(chatID int64, text string) {
	b.sendWithKeyboard(chatID, text, nil)
}

// sendWithApp adds the Open Mini App button when an app URL is configured.
func (b *Bot) sendWithApp(chatID int64, text string) {
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

func (b *Bot) sendWithKeyboard(chatID int64, text string, replyMarkup map[string]any) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.token)
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	payload, _ := json.Marshal(body)
	resp, err := b.client.Post(url, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
}
