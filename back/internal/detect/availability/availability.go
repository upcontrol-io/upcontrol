// Package availability is the detector that turns a stream of check results
// (ok/fail) into incident state transitions. The rule (plan §5.7) is simple:
// N consecutive failures open an incident; the first success after an open
// incident closes it with reason "recovered". Nothing fires on a single failure
// — the plan's "temporary confirmation" IS the consecutive count, which at a
// 1-minute interval means N minutes of confirmed downtime.
//
// The detector is a pure state machine: it takes the current State + one check
// result and returns the new State + an Outcome (open/close incident). It holds
// no timers, no goroutines, no database — the monitor scheduler feeds it one
// result at a time and acts on the Outcome.
package availability

import "time"

// Status values match monitor_facts.status and the front's HealthStatus.
const (
	StatusNoData = "nodata"
	StatusOK     = "ok"
	StatusCheck  = "check"
	StatusDown   = "down"
)

// DefaultThreshold is the number of consecutive failures needed to open an
// incident. 3 at a 1-minute interval = 3 minutes of confirmed downtime before
// the alert fires — long enough to ride out a single blip, short enough that a
// real outage reaches the customer before they notice.
const DefaultThreshold = 3

// State is the per-monitor detector state, persisted in monitor_facts.
type State struct {
	Status              string
	ConsecutiveFailures int
	LastCheckAt         time.Time
}

// Outcome tells the caller what to DO after processing a result.
type Outcome struct {
	// Open is true when the detector crossed the threshold: the caller opens an
	// incident (title, detector, fingerprint are the caller's job).
	Open bool
	// Close is true when an open incident recovered: the caller closes it with
	// reason "recovered".
	Close bool
	// CloseReason is "recovered" (the only auto-close reason for availability).
	CloseReason string
}

// Detector applies the consecutive-failure rule to check results.
type Detector struct {
	threshold int
}

// New builds a Detector with the given threshold (0 = DefaultThreshold).
func New(threshold int) *Detector {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	return &Detector{threshold: threshold}
}

// Process applies one check result to the state and returns the outcome.
// The state is mutated in place (the caller persists it).
func (d *Detector) Process(s *State, ok bool, at time.Time) Outcome {
	s.LastCheckAt = at

	if ok {
		return d.processOK(s)
	}
	return d.processFail(s)
}

func (d *Detector) processOK(s *State) Outcome {
	wasDown := s.Status == StatusDown
	s.ConsecutiveFailures = 0

	if wasDown {
		s.Status = StatusOK
		return Outcome{Close: true, CloseReason: "recovered"}
	}
	// Any non-down status transitions to ok on a successful check (covers
	// nodata → ok, check → ok, and ok → ok is a no-op).
	s.Status = StatusOK
	return Outcome{}
}

func (d *Detector) processFail(s *State) Outcome {
	s.ConsecutiveFailures++

	if s.Status == StatusDown {
		// Already down: the incident is open, this is another failure. No new
		// outcome (the caller may update the incident's affected_count).
		return Outcome{}
	}

	if s.ConsecutiveFailures >= d.threshold {
		s.Status = StatusDown
		return Outcome{Open: true}
	}

	// Not yet at the threshold: mark as "check" (something is wrong, not yet
	// confirmed as an outage).
	s.Status = StatusCheck
	return Outcome{}
}

// Threshold returns the configured consecutive-failure threshold.
func (d *Detector) Threshold() int { return d.threshold }
