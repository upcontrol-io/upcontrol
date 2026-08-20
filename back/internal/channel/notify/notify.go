// Package notify defines the per-channel notification settings
// (docs/plans/channel-notify-settings.md): what a destination hears about, and
// whether a 15-minute resolve follow-up comes. The settings pick which CLASSES
// of alert land on a channel — they are deliberately not a per-monitor routing
// matrix, and must not grow into one (new-plan.md §6, narrowed by the Aug 14,
// 2026 user decision).
package notify

import (
	"encoding/json"
	"time"
)

// FollowUpDelay is how long after the page the resolve follow-up goes out —
// either way: "recovered" tells the reader not to run for a laptop, "still
// down" tells them to keep running.
const FollowUpDelay = 15 * time.Minute

// Settings is the resolved (never sparse) form. The jsonb column stores keys
// sparsely — an absent key means the default, so rows that predate the column
// keep today's behaviour with no backfill — but every reader goes through
// Resolve and only ever sees this.
type Settings struct {
	// WebsiteDown is the incident-open page — what already sends today, so it
	// defaults on.
	WebsiteDown bool `json:"websiteDown"`
	// ErrorLogs and RepeatingErrorLogs are ONE axis, not two switches (user
	// decision, Aug 14, 2026): "which errors page this address" — none, every
	// new one, or only ones that repeat. "Every new error" already contains the
	// repeating ones, so both-on is a contradiction; Apply and Resolve keep the
	// pair exclusive, and the screen renders it as a three-way radio.
	//
	// ErrorLogs: a NEW error fingerprint appeared in the log stream ("an error
	// appeared", never "a line arrived" — the scanner cools down per
	// fingerprint).
	ErrorLogs bool `json:"errorLogs"`
	// RepeatingErrorLogs: ONLY fingerprints repeating at least twice inside
	// RepeatWindowMin — the noise filter for streams where single errors are
	// routine.
	RepeatingErrorLogs bool `json:"repeatingErrorLogs"`
	// RepeatWindowMin is the repeat window in minutes, clamped to 1–120.
	RepeatWindowMin int `json:"repeatWindowMin"`
	// ResolveFollowUp is PAID ONLY (every paid plan; Free gets 402
	// plan_limit_exceeded and the client opens its upgrade prompt): one follow-up 15
	// minutes after the page, composed at send time from the incident's
	// then-current state.
	ResolveFollowUp bool `json:"resolveFollowUp"`
}

// Defaults is the behaviour a channel has before anyone opens the settings —
// exactly what the product did before the settings existed.
func Defaults() Settings {
	return Settings{WebsiteDown: true, RepeatWindowMin: 5}
}

// Resolve parses the sparse jsonb column into a fully-populated Settings.
// Absent keys keep their defaults; the window is clamped so a hand-posted 0
// or 10000 cannot make the scanner query nothing or everything.
func Resolve(raw []byte) Settings {
	s := Defaults()
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	return clamp(s)
}

// Patch overlays the fields present in a PATCH body onto current. Pointer
// fields tell "not sent" apart from "sent as false" — without that, toggling
// one checkbox would silently reset the other four.
type Patch struct {
	WebsiteDown        *bool `json:"websiteDown"`
	ErrorLogs          *bool `json:"errorLogs"`
	RepeatingErrorLogs *bool `json:"repeatingErrorLogs"`
	RepeatWindowMin    *int  `json:"repeatWindowMin"`
	ResolveFollowUp    *bool `json:"resolveFollowUp"`
}

// Apply returns current with the patch's present fields overlaid, clamped.
// The error-log pair stays exclusive: turning one on turns the other off, so a
// client that sends only the key it changed still cannot store both.
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
	// A hand-posted row carrying both is resolved to the stricter reading:
	// asking for "only repeating" and getting every error is the surprise,
	// the other way round is just fewer messages.
	if s.ErrorLogs && s.RepeatingErrorLogs {
		s.ErrorLogs = false
	}
	return s
}
