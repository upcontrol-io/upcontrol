// Worker is the delivery queue processor. It runs as a background goroutine,
// periodically picking up pending items, sending them through the appropriate
// channel, and recording the outcome (sent / rescheduled with backoff / dead).
//
// The worker is the production wiring of the deliver package's pure logic
// (Backoff, ClassifyError, Breaker). It holds a pg.Pool for queue state and a
// map of Channel implementations (telegram, email, discord, slack).

package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Worker processes the delivery queue.
type Worker struct {
	pool     *pg.Pool
	channels map[string]Channel // keyed by channel kind: telegram|email|discord|slack
	log      *slog.Logger
	nodeID   string
	breakers map[int64]*Breaker // per channel_id
}

// NewWorker builds a delivery worker.
func NewWorker(pool *pg.Pool, log *slog.Logger, nodeID string) *Worker {
	return &Worker{
		pool:     pool,
		channels: map[string]Channel{},
		log:      log,
		nodeID:   nodeID,
		breakers: map[int64]*Breaker{},
	}
}

// RegisterChannel adds a channel implementation. The plan's four channels are
// telegram, email, discord, slack. Each is registered once at startup.
func (w *Worker) RegisterChannel(c Channel) {
	w.channels[c.Kind()] = c
}

// Tick processes one batch of pending deliveries. Called periodically by the
// background loop (every 2 seconds in production).
func (w *Worker) Tick(ctx context.Context) error {
	q := w.pool.Queries()
	items, err := q.LeasePendingDeliveries(ctx, sqlc.LeasePendingDeliveriesParams{
		LeasedBy:   &w.nodeID,
		LeaseUntil: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		Limit:      20,
	})
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}

	for _, item := range items {
		w.processItem(ctx, item)
	}
	return nil
}

func (w *Worker) processItem(ctx context.Context, item sqlc.LeasePendingDeliveriesRow) {
	q := w.pool.Queries()
	queueID := item.ID

	// Look up the channel config.
	ch, err := q.GetChannelForDelivery(ctx, item.ChannelID)
	if err != nil {
		w.dead(ctx, queueID, "channel not found")
		return
	}

	// Get or create the breaker for this channel.
	breaker := w.getBreaker(item.ChannelID)

	// If the breaker is open, failover to email (the backup channel).
	channelKind := ch.Kind
	target := ch.Target
	if breaker.ShouldFailover(time.Now()) && channelKind != "email" {
		channelKind = "email"
		// Fail over to the tenant's email channel. Without this lookup the payload
		// was silently dropped (worker.go target="" — the recipient was lost).
		if t, terr := q.GetEmailChannelTarget(ctx, item.TenantID); terr == nil && t != "" {
			target = t
		} else {
			// No email channel to fail over to: this delivery can't reach anyone.
			w.dead(ctx, queueID, "no email channel to fail over to")
			return
		}
		w.log.Warn("breaker open, failing over to email",
			"channel_id", item.ChannelID, "original_kind", ch.Kind)
	}

	// Find the channel implementation.
	sender, ok := w.channels[channelKind]
	if !ok {
		w.dead(ctx, queueID, "no sender for kind "+channelKind)
		return
	}

	// Deserialize the payload.
	var payload AlertPayload
	if item.Payload != nil {
		_ = json.Unmarshal(item.Payload, &payload)
	}

	// A muted channel (the bot's /mute window) does not refuse the delivery —
	// it defers it: the item comes back when the window ends, so nothing is
	// lost and expiry needs no sweeper. The breaker's reschedule path already
	// proved this shape; attempts only count actual sends because we return
	// before the attempt is recorded.
	if ch.MutedUntil.Valid && ch.MutedUntil.Time.After(time.Now()) {
		_ = q.Reschedule(ctx, sqlc.RescheduleParams{
			NextTryAt: ch.MutedUntil,
			ID:        queueID,
		})
		return
	}

	// The delivery's class travels with the payload from here on: it is queue
	// state, not detector state, and a channel that renders needs it. The same
	// status "down" is an outage page and a fifteen-minute follow-up, and they
	// do not read the same.
	payload.Class = item.Class

	// Personal telegram channels (a recipient person) get Acknowledge/Resolve
	// inline buttons; broadcast groups never do (design D5 — in a group, the
	// presser of a button cannot be name-verified).
	if channelKind == "telegram" {
		payload.Buttons = ch.RecipientPersonID != nil
	}

	// A resolve follow-up (docs/plans/channel-notify-settings.md) is composed
	// HERE, at send time, from the incident's then-current state — enqueue time
	// (incident open, next_try_at +15min) only knew the question, not the
	// answer. Recovered → "back up", so the reader stops running for a laptop;
	// still open → "still down", so they keep running.
	if item.Class == "followup" {
		if item.IncidentID == nil {
			w.dead(ctx, queueID, "followup without incident")
			return
		}
		inc, ierr := q.GetIncidentForFollowUp(ctx, *item.IncidentID)
		if ierr != nil {
			w.dead(ctx, queueID, "followup: incident not found")
			return
		}
		name := payload.MonitorName
		if name == "" {
			name = inc.MonitorName
		}
		if inc.Status == "ok" {
			payload.Title = name + " recovered — it is back up"
			payload.Status = "ok"
			// The duration line, from the incident's own bounds — measured,
			// not composed at enqueue time. Either bound missing = no line:
			// a renderer never invents one to fill the gap.
			if inc.DetectedAt.Valid && inc.ResolvedAt.Valid {
				payload.Summary = downForLine(inc.DetectedAt.Time, inc.ResolvedAt.Time)
			}
		} else {
			payload.Title = name + " is still down after 15 minutes"
			payload.Status = "down"
		}
	}

	// Attempt the send.
	statusCode, sendErr := sender.Send(ctx, target, payload)
	now := time.Now()

	var outcome Outcome
	var detail string
	if sendErr != nil {
		outcome = OutcomeRetryable
		detail = sendErr.Error()
	} else {
		outcome = ClassifyError(statusCode)
		detail = fmt.Sprintf("HTTP %d", statusCode)
	}

	// Record the attempt.
	_ = q.RecordDeliveryAttempt(ctx, sqlc.RecordDeliveryAttemptParams{
		QueueID: queueID,
		Outcome: string(outcome),
		Detail:  detail,
	})

	attempt := int(item.Attempts) + 1

	switch outcome {
	case OutcomeOK:
		// Delivered. Clear the lease, mark sent.
		_ = q.MarkDelivered(ctx, queueID)
		breaker.RecordSuccess()

	case OutcomeFatal:
		// Non-repeatable: straight to DLQ.
		w.dead(ctx, queueID, detail)
		breaker.RecordFailure(now)

	case OutcomeRetryable:
		breaker.RecordFailure(now)
		nextTry := NextTryAt(attempt, outcome, now)
		if nextTry.IsZero() {
			// Max attempts reached.
			w.dead(ctx, queueID, "max retries exceeded")
		} else {
			_ = q.Reschedule(ctx, sqlc.RescheduleParams{
				NextTryAt: pgtype.Timestamptz{Time: nextTry, Valid: true},
				ID:        queueID,
			})
		}
	}
}

func (w *Worker) dead(ctx context.Context, queueID int64, reason string) {
	reasonPtr := reason
	_ = w.pool.Queries().MarkDead(ctx, sqlc.MarkDeadParams{
		DeadReason: &reasonPtr,
		ID:         queueID,
	})
}

func (w *Worker) getBreaker(channelID int64) *Breaker {
	b, ok := w.breakers[channelID]
	if !ok {
		b = &Breaker{}
		w.breakers[channelID] = b
	}
	return b
}

// Run drives the worker until ctx is cancelled. Tick every 2 seconds.
func (w *Worker) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.log.Warn("delivery worker tick error", "err", err)
			}
		}
	}
}

// downForLine is the recovered follow-up's one sentence: how long, bounded by
// when. Under a minute stays honest words, not "0 minutes".
func downForLine(from, to time.Time) string {
	mins := int(to.Sub(from).Minutes())
	span := fmt.Sprintf("%s to %s UTC", from.UTC().Format("15:04"), to.UTC().Format("15:04"))
	if mins < 1 {
		return "Down for under a minute, " + span + "."
	}
	if mins == 1 {
		return "Down for 1 minute, " + span + "."
	}
	return fmt.Sprintf("Down for %d minutes, %s.", mins, span)
}
