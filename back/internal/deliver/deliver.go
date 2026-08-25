// Package deliver is the alert delivery pipeline: pending items from
// delivery_queue, sent through a channel, outcomes recorded.
package deliver

import (
	"math/rand/v2"
	"time"
)

// MaxAttempts is the delivery-attempt ceiling before the item goes to the DLQ.
const MaxAttempts = 5

// Backoff returns the delay after `attempt` failures: 1/2/4/8/16s (2^attempt)
// with jitter, so a herd of retries does not land on the same second.
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

// NextTryAt computes the next attempt time; fatal or exhausted returns the
// zero time, which is the DLQ.
func NextTryAt(attempt int, outcome Outcome, now time.Time) time.Time {
	if outcome == OutcomeFatal || attempt >= MaxAttempts {
		return time.Time{} // zero = DLQ
	}
	if outcome == OutcomeOK {
		return time.Time{} // sent, no retry
	}
	return now.Add(Backoff(attempt))
}

// BreakerTimeout is how long a channel must be down before the breaker opens.
const BreakerTimeout = 5 * time.Minute

// Breaker tracks a channel's failure state: open after BreakerTimeout of
// failing, closed on the first success; open means fail over to the backup.
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

// RecordFailure extends or starts the failure streak; the breaker opens on
// the next IsOpen call once the streak exceeds BreakerTimeout.
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
