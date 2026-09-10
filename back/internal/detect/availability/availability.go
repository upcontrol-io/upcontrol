// Package availability turns check results into incident transitions: N
// consecutive failures open, the first success after closes ("recovered").
// A could-not-measure reading (HTTP 401/403/429, a bot filter) is a third
// verdict: it never fails the target, never opens or closes an incident, and
// after three in a row the state says could_not_measure instead of guessing.
package availability

import "time"

// Status values match target_facts.status and the front's HealthStatus.
const (
	StatusNoData          = "nodata"
	StatusOK              = "ok"
	StatusCheck           = "check"
	StatusDown            = "down"
	StatusCouldNotMeasure = "could_not_measure"
)

// DefaultThreshold is the consecutive failures needed to open: 3 at a
// 1-minute interval rides out a blip without holding a real outage back.
const DefaultThreshold = 3

// unmeasuredThreshold is the consecutive could-not-measure readings after
// which the state admits it cannot see the target.
const unmeasuredThreshold = 3

// State is the per-target detector state, persisted in target_facts.
type State struct {
	Status                string
	ConsecutiveFailures   int
	ConsecutiveUnmeasured int
	LastCheckAt           time.Time
}

// Outcome is the measured verdict of one check.
type Outcome int

const (
	OutcomeOK Outcome = iota
	OutcomeFail
	OutcomeUnmeasured
)

// Transition tells the caller what to DO after processing a result.
type Transition struct {
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

// Process applies one check result to the state and returns the transition.
// The state is mutated in place (the caller persists it).
func (d *Detector) Process(s *State, outcome Outcome, at time.Time) Transition {
	s.LastCheckAt = at

	switch outcome {
	case OutcomeOK:
		return d.processOK(s)
	case OutcomeFail:
		return d.processFail(s)
	default:
		return d.processUnmeasured(s)
	}
}

func (d *Detector) processOK(s *State) Transition {
	wasDown := s.Status == StatusDown
	s.ConsecutiveFailures = 0
	s.ConsecutiveUnmeasured = 0

	if wasDown {
		s.Status = StatusOK
		return Transition{Close: true, CloseReason: "recovered"}
	}
	// Any non-down status transitions to ok on a successful check (covers
	// nodata → ok, check → ok, could_not_measure → ok, and ok → ok is a no-op).
	s.Status = StatusOK
	return Transition{}
}

func (d *Detector) processFail(s *State) Transition {
	s.ConsecutiveFailures++
	s.ConsecutiveUnmeasured = 0

	if s.Status == StatusDown {
		// Already down: the incident is open, this is another failure. No new
		// transition (the caller may update the incident's affected_count).
		return Transition{}
	}

	if s.ConsecutiveFailures >= d.threshold {
		s.Status = StatusDown
		return Transition{Open: true}
	}

	// Not yet at the threshold: mark as "check" (something is wrong, not yet
	// confirmed as an outage).
	s.Status = StatusCheck
	return Transition{}
}

// processUnmeasured records a reading the probe could not take. It is not a
// failure (uptime never counts it), not a recovery (nothing closes), and not
// a blip: only the unmeasured streak moves, until it is long enough that the
// honest state is could_not_measure. A down target stays down: the word
// "down" was earned by real failures and an unreadable host does not unearn
// it.
func (d *Detector) processUnmeasured(s *State) Transition {
	s.ConsecutiveUnmeasured++
	if s.ConsecutiveUnmeasured >= unmeasuredThreshold && s.Status != StatusDown {
		s.Status = StatusCouldNotMeasure
	}
	return Transition{}
}
