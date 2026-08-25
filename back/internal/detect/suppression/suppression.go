// Package suppression implements the four rules that prevent noise without
// missing real incidents: cooldown, dedup, maintenance window, post-deploy.
package suppression

import "time"

// SuppressionDecision tells the caller what to do with a detector fire.
type SuppressionDecision struct {
	Suppress bool
	Reason   string // why it was suppressed (for logging)
}

// Allow lets the fire through.
func Allow() SuppressionDecision { return SuppressionDecision{} }

// Suppress blocks the fire.
func Suppress(reason string) SuppressionDecision {
	return SuppressionDecision{Suppress: true, Reason: reason}
}

// Input is what the caller passes to the suppression engine.
type Input struct {
	Fingerprint     int64
	LastFireAt      time.Time // when this fingerprint last fired (zero = never)
	HasOpenIncident bool      // an incident for this fingerprint is already open
	InMaintenance   bool      // the monitor/tenant is in a maintenance window
	LastDeployAt    time.Time // when the last deploy happened (zero = never)
	Now             time.Time
}

// CooldownPeriod is how long after a fire the same fingerprint is suppressed.
const CooldownPeriod = 30 * time.Minute

// PostDeployWindow is the silence after a deploy.
const PostDeployWindow = 90 * time.Second

// Evaluate runs all four suppression rules. The first one that matches wins.
func Evaluate(in Input) SuppressionDecision {
	// 1. Post-deploy: suppress 90s after any deploy.
	if !in.LastDeployAt.IsZero() && in.Now.Sub(in.LastDeployAt) <= PostDeployWindow {
		return Suppress("post_deploy_blip")
	}

	// 2. Maintenance window.
	if in.InMaintenance {
		return Suppress("maintenance_window")
	}

	// 3. Dedup: an incident for this fingerprint is already open.
	if in.HasOpenIncident {
		return Suppress("duplicate_open_incident")
	}

	// 4. Cooldown: same fingerprint fired recently.
	if !in.LastFireAt.IsZero() && in.Now.Sub(in.LastFireAt) <= CooldownPeriod {
		return Suppress("cooldown")
	}

	return Allow()
}
