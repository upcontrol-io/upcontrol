// Package availability turns check results into incident transitions: N
// consecutive failures open, the first success after closes ("recovered").
package availability

import "time"

// Status values match monitor_facts.status and the front's HealthStatus.
const (
	StatusNoData = "nodata"
	StatusOK     = "ok"
	StatusCheck  = "check"
	StatusDown   = "down"
)

// DefaultThreshold is the consecutive failures needed to open: 3 at a
// 1-minute interval rides out a blip without holding a real outage back.
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
