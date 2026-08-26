// worker is the delivery queue processor: it leases pending items, sends them
// through a channel, records the outcome. The wiring of the pure logic.

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

type worker struct {
	pool     *pg.Pool
	channels map[string]channel // keyed by channel kind: telegram|email|discord|slack
	log      *slog.Logger
	nodeID   string
	breakers map[int64]*breaker // per channel_id
}

// NewWorker builds a delivery worker.
func NewWorker(pool *pg.Pool, log *slog.Logger, nodeID string) *worker {
	return &worker{
		pool:     pool,
		channels: map[string]channel{},
		log:      log,
		nodeID:   nodeID,
		breakers: map[int64]*breaker{},
	}
}

// RegisterChannel adds a channel implementation; each of the four kinds is
// registered once at startup.
func (w *worker) RegisterChannel(c channel) {
	w.channels[c.Kind()] = c
}

// Tick processes one batch of pending deliveries; called periodically by the
// background loop.
func (w *worker) Tick(ctx context.Context) error {
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

func (w *worker) processItem(ctx context.Context, item sqlc.LeasePendingDeliveriesRow) {
	q := w.pool.Queries()
	queueID := item.ID

	ch, err := q.GetChannelForDelivery(ctx, item.ChannelID)
	if err != nil {
		w.dead(ctx, queueID, "channel not found")
		return
	}

	breaker := w.getBreaker(item.ChannelID)

	channelKind := ch.Kind
	target := ch.Target
	if breaker.IsOpen(time.Now()) && channelKind != "email" {
		channelKind = "email"
		// Fail over to the tenant's email channel; without the lookup the
		// payload was silently dropped with the recipient lost.
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

	sender, ok := w.channels[channelKind]
	if !ok {
		w.dead(ctx, queueID, "no sender for kind "+channelKind)
		return
	}

	var payload AlertPayload
	if item.Payload != nil {
		_ = json.Unmarshal(item.Payload, &payload)
	}

	// A muted channel defers the delivery to the window's end: nothing lost,
	// expiry needs no sweeper; attempts count only actual sends.
	if ch.MutedUntil.Valid && ch.MutedUntil.Time.After(time.Now()) {
		_ = q.Reschedule(ctx, sqlc.RescheduleParams{
			NextTryAt: ch.MutedUntil,
			ID:        queueID,
		})
		return
	}

	// The delivery's class travels with the payload: the same status "down"
	// is an outage page and a follow-up, and they do not read the same.
	payload.Class = item.Class

	// Every telegram channel gets the action buttons, authorised by WHO
	// pressed them; a broadcast group drops the web_app row (Bot API rule).
	if channelKind == "telegram" {
		payload.Buttons = true
		payload.Group = ch.RecipientPersonID == nil
	}

	// A resolve follow-up is composed HERE, at send time, from the incident's
	// then-current state; enqueue time only knew the question.
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
			// Nothing left to acknowledge or resolve once it is over; the
			// buttons would act on a closed incident.
			payload.Buttons = false
			// The duration line from the incident's own bounds; either bound
			// missing = no line, a renderer never invents one.
			if inc.DetectedAt.Valid && inc.ResolvedAt.Valid {
				payload.Summary = downForLine(inc.DetectedAt.Time, inc.ResolvedAt.Time)
			}
		} else {
			payload.Title = name + " is still down after 15 minutes"
			payload.Status = "down"
		}
	}

	statusCode, sendErr := sender.Send(ctx, target, payload)
	now := time.Now()

	var outcome outcome
	var detail string
	if sendErr != nil {
		outcome = outcomeRetryable
		detail = sendErr.Error()
	} else {
		outcome = classifyError(statusCode)
		detail = fmt.Sprintf("HTTP %d", statusCode)
	}

	_ = q.RecordDeliveryAttempt(ctx, sqlc.RecordDeliveryAttemptParams{
		QueueID: queueID,
		Outcome: string(outcome),
		Detail:  detail,
	})

	attempt := int(item.Attempts) + 1

	switch outcome {
	case outcomeOK:
		_ = q.MarkDelivered(ctx, queueID)
		breaker.RecordSuccess()

	case outcomeFatal:
		// Non-repeatable: straight to DLQ.
		w.dead(ctx, queueID, detail)
		breaker.RecordFailure(now)

	case outcomeRetryable:
		breaker.RecordFailure(now)
		nextTry := nextTryAt(attempt, outcome, now)
		if nextTry.IsZero() {
			w.dead(ctx, queueID, "max retries exceeded")
		} else {
			_ = q.Reschedule(ctx, sqlc.RescheduleParams{
				NextTryAt: pgtype.Timestamptz{Time: nextTry, Valid: true},
				ID:        queueID,
			})
		}
	}
}

func (w *worker) dead(ctx context.Context, queueID int64, reason string) {
	reasonPtr := reason
	_ = w.pool.Queries().MarkDead(ctx, sqlc.MarkDeadParams{
		DeadReason: &reasonPtr,
		ID:         queueID,
	})
}

func (w *worker) getBreaker(channelID int64) *breaker {
	b, ok := w.breakers[channelID]
	if !ok {
		b = &breaker{}
		w.breakers[channelID] = b
	}
	return b
}

// Run drives the worker until ctx is cancelled. Tick every 2 seconds.
func (w *worker) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
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

// downForLine is the recovered follow-up's one sentence; under a minute stays
// honest words, not "0 minutes".
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
