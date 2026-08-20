// Package deliver is the alert delivery pipeline (plan §5.9). It picks up
// pending items from delivery_queue, attempts to send them through the
// appropriate channel (telegram/email/discord/slack), and records the outcome.
//
// Repeatable errors (429, 5xx, timeout, network) get exponential backoff with
// jitter (1/2/4/8/16s, ≤5 attempts). Non-repeatable errors (400, 403, bot
// blocked, address bounced) go straight to the DLQ.
//
// A per-channel circuit breaker opens when the channel has been down >5 min,
// failing over to email (the backup). The breaker closes when the channel
// recovers.
package deliver

import (
	"math/rand/v2"
	"time"
)

// MaxAttempts is the maximum number of delivery attempts before the item goes to
// the DLQ. The plan (§9.1) says ≤5 attempts.
const MaxAttempts = 5

// Backoff returns the delay before the next attempt after `attempt` failures.
// The base delays are 1/2/4/8/16s (2^attempt), each with ±25% jitter so a
// thundering herd of retries does not all land on the same second.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 4 {
		attempt = 4 // cap at 16s
	}
	base := time.Duration(1<<attempt) * time.Second
	jitter := time.Duration(rand.Int64N(int64(base) / 2))
	return base + jitter
}

// Outcome classifies a send attempt's result.
type Outcome string

const (
	OutcomeOK        Outcome = "ok"        // delivered
	OutcomeRetryable Outcome = "retriable" // 429, 5xx, timeout, network → backoff
	OutcomeFatal     Outcome = "fatal"     // 400, 403, bot blocked, bounced → DLQ
)

// ClassifyError maps an HTTP status code to a delivery Outcome.
func ClassifyError(statusCode int) Outcome {
	switch {
	case statusCode == 0:
		return OutcomeRetryable // network error / timeout
	case statusCode == 429:
		return OutcomeRetryable
	case statusCode >= 500:
		return OutcomeRetryable
	case statusCode == 403:
		return OutcomeFatal // bot blocked, auth revoked
	case statusCode >= 400:
		return OutcomeFatal // 400, 404, etc. — non-repeatable
	default:
		return OutcomeOK
	}
}

// NextTryAt computes when the next attempt should happen, given the current
// attempt count and outcome. For fatal outcomes, returns the zero time (the
// item goes to the DLQ immediately — no further attempts).
func NextTryAt(attempt int, outcome Outcome, now time.Time) time.Time {
	if outcome == OutcomeFatal || attempt >= MaxAttempts {
		return time.Time{} // zero = DLQ
	}
	if outcome == OutcomeOK {
		return time.Time{} // sent, no retry
	}
	return now.Add(Backoff(attempt))
}

// --- circuit breaker ---

// BreakerTimeout is how long a channel must be down before the breaker opens.
const BreakerTimeout = 5 * time.Minute

// Breaker tracks a channel's failure state. When the channel has been failing
// for more than BreakerTimeout, the breaker opens and delivery fails over to the
// backup channel (email). The breaker closes on the first success.
type Breaker struct {
	failuresSince time.Time // when the current failure streak started (zero = healthy)
}

// IsOpen reports whether the breaker is currently open (channel is down).
func (b *Breaker) IsOpen(now time.Time) bool {
	return !b.failuresSince.IsZero() && now.Sub(b.failuresSince) >= BreakerTimeout
}

// RecordSuccess closes the breaker — the channel is healthy again.
func (b *Breaker) RecordSuccess() {
	b.failuresSince = time.Time{}
}

// RecordFailure extends or starts the failure streak. If the streak exceeds
// BreakerTimeout, the breaker opens on the next IsOpen call.
func (b *Breaker) RecordFailure(now time.Time) {
	if b.failuresSince.IsZero() {
		b.failuresSince = now
	}
}

// ShouldFailover reports whether the item should be rerouted to the backup
// channel instead of waiting. True when the breaker is open.
func (b *Breaker) ShouldFailover(now time.Time) bool {
	return b.IsOpen(now)
}
