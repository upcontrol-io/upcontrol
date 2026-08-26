// Package deliver is the alert delivery pipeline: pending items from
// delivery_queue, sent through a channel, outcomes recorded.
package deliver

import (
	"math/rand/v2"
	"time"
)

// maxAttempts is the delivery-attempt ceiling before the item goes to the DLQ.
const maxAttempts = 5

// backoff returns the delay after `attempt` failures: 1/2/4/8/16s (2^attempt)
// with jitter, so a herd of retries does not land on the same second.
func backoff(attempt int) time.Duration {
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

// outcome classifies a send attempt's result.
type outcome string

const (
	outcomeOK        outcome = "ok"        // delivered
	outcomeRetryable outcome = "retriable" // 429, 5xx, timeout, network → backoff
	outcomeFatal     outcome = "fatal"     // 400, 403, bot blocked, bounced → DLQ
)

// classifyError maps an HTTP status code to a delivery outcome.
func classifyError(statusCode int) outcome {
	switch {
	case statusCode == 0:
		return outcomeRetryable // network error / timeout
	case statusCode == 429:
		return outcomeRetryable
	case statusCode >= 500:
		return outcomeRetryable
	case statusCode == 403:
		return outcomeFatal // bot blocked, auth revoked
	case statusCode >= 400:
		return outcomeFatal // 400, 404, etc.: non-repeatable
	default:
		return outcomeOK
	}
}

// nextTryAt computes the next attempt time; fatal or exhausted returns the
// zero time, which is the DLQ.
func nextTryAt(attempt int, outcome outcome, now time.Time) time.Time {
	if outcome == outcomeFatal || attempt >= maxAttempts {
		return time.Time{} // zero = DLQ
	}
	if outcome == outcomeOK {
		return time.Time{} // sent, no retry
	}
	return now.Add(backoff(attempt))
}

// breakerTimeout is how long a channel must be down before the breaker opens.
const breakerTimeout = 5 * time.Minute

// breaker tracks a channel's failure state: open after breakerTimeout of
// failing, closed on the first success; open means fail over to the backup.
type breaker struct {
	failuresSince time.Time // when the current failure streak started (zero = healthy)
}

// IsOpen reports whether the breaker is open: the item should be rerouted
// to the backup channel instead of waiting.
func (b *breaker) IsOpen(now time.Time) bool {
	return !b.failuresSince.IsZero() && now.Sub(b.failuresSince) >= breakerTimeout
}

// RecordSuccess closes the breaker: the channel is healthy again.
func (b *breaker) RecordSuccess() {
	b.failuresSince = time.Time{}
}

// RecordFailure extends or starts the failure streak; the breaker opens on
// the next IsOpen call once the streak exceeds breakerTimeout.
func (b *breaker) RecordFailure(now time.Time) {
	if b.failuresSince.IsZero() {
		b.failuresSince = now
	}
}
