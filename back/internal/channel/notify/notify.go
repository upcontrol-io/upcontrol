// Package notify defines the per-channel notification settings: what a
// destination hears about, and whether a resolve follow-up comes.
package notify

import (
	"encoding/json"
	"time"
)

// FollowUpDelay is how long after the page the resolve follow-up goes out:
// "recovered" or "still down", either way.
const FollowUpDelay = 15 * time.Minute

// Settings is the resolved (never sparse) form; the jsonb column stores keys
// sparsely (absent = default), and every reader goes through Resolve.
type Settings struct {
	// WebsiteDown is the incident-open page — what already sends today, so it
	// defaults on.
	WebsiteDown bool `json:"websiteDown"`
	// ErrorLogs and RepeatingErrorLogs are one axis ("which errors page this
	// address"), kept exclusive; ErrorLogs = a NEW error fingerprint appeared.
	ErrorLogs bool `json:"errorLogs"`
	// RepeatingErrorLogs: only fingerprints repeating at least twice inside
	// RepeatWindowMin: the noise filter where single errors are routine.
	RepeatingErrorLogs bool `json:"repeatingErrorLogs"`
	// RepeatWindowMin is the repeat window in minutes, clamped to 1–120.
	RepeatWindowMin int `json:"repeatWindowMin"`
	// ResolveFollowUp is PAID ONLY (Free gets 402 plan_limit_exceeded): one
	// follow-up 15 minutes after the page, composed at send time.
	ResolveFollowUp bool `json:"resolveFollowUp"`
}

// Defaults is the behaviour a channel has before anyone opens the settings —
// exactly what the product did before the settings existed.
func Defaults() Settings {
	return Settings{WebsiteDown: true, RepeatWindowMin: 5}
}

// Resolve parses the sparse jsonb column; absent keys keep defaults, the
// window clamps so a hand-posted 0 or 10000 breaks nothing.
func Resolve(raw []byte) Settings {
	s := Defaults()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return clamp(s)
}

// Patch overlays present fields onto current; pointers tell "not sent"
// apart from "sent as false".
type Patch struct {
	WebsiteDown        *bool `json:"websiteDown"`
	ErrorLogs          *bool `json:"errorLogs"`
	RepeatingErrorLogs *bool `json:"repeatingErrorLogs"`
	RepeatWindowMin    *int  `json:"repeatWindowMin"`
	ResolveFollowUp    *bool `json:"resolveFollowUp"`
}

// Apply overlays the patch's present fields, clamped. The error-log pair
// stays exclusive: turning one on turns the other off.
func (p Patch) Apply(current Settings) Settings {
	if p.WebsiteDown != nil {
		current.WebsiteDown = *p.WebsiteDown
	}
	if p.ErrorLogs != nil {
		current.ErrorLogs = *p.ErrorLogs
		if current.ErrorLogs {
			current.RepeatingErrorLogs = false
		}
	}
	if p.RepeatingErrorLogs != nil {
		current.RepeatingErrorLogs = *p.RepeatingErrorLogs
		if current.RepeatingErrorLogs {
			current.ErrorLogs = false
		}
	}
	if p.RepeatWindowMin != nil {
		current.RepeatWindowMin = *p.RepeatWindowMin
	}
	if p.ResolveFollowUp != nil {
		current.ResolveFollowUp = *p.ResolveFollowUp
	}
	return clamp(current)
}

func clamp(s Settings) Settings {
	if s.RepeatWindowMin < 1 {
		s.RepeatWindowMin = 1
	}
	if s.RepeatWindowMin > 120 {
		s.RepeatWindowMin = 120
	}
	// A row carrying both resolves to the stricter reading: "only repeating"
	// that gets every error is the surprise; the other way is fewer messages.
	if s.ErrorLogs && s.RepeatingErrorLogs {
		s.ErrorLogs = false
	}
	return s
}
