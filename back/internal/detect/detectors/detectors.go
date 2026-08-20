// Package detector implements the correlation detectors that make upcontrol a
// product and not just an uptime checker (plan §7.4). Each detector examines a
// series of aggregates (from ClickHouse series_1m / checks_1m) and fires when a
// threshold is crossed relative to the baseline.
//
// The detectors are pure-logic: each takes a series of recent observations and a
// baseline, and returns a Decision (fire / don't fire). The ucworker goroutine
// feeds them data and acts on the Decision by opening/closing incidents.
package detector

import (
	"math"
	"strconv"
)

// Decision is the output of a detector pass.
type Decision struct {
	Fire   bool    // true = open an incident
	Reason string  // human-readable explanation for the incident title
	Score  float64 // 0–1, for suppression priority
}

// NoFire is the zero-value Decision.
func NoFire() Decision { return Decision{} }

// --- error_rate detector ---
// Fires when the error rate (errors / total) in the last window exceeds the
// baseline median by more than `multiplier` MADs.

// ErrorRate checks if the current error fraction is anomalous vs the baseline.
func ErrorRate(currentErrors, currentTotal int, baselineMedian, baselineMAD, scale float64) Decision {
	if currentTotal < 10 {
		return NoFire() // not enough data
	}
	rate := float64(currentErrors) / float64(currentTotal)
	if baselineMAD <= 0 || scale <= 0 {
		if rate > 0.1 {
			return Decision{Fire: true, Reason: "error rate above 10%", Score: rate}
		}
		return NoFire()
	}
	// Z-score: (observed - median) / (MAD * scale)
	z := (rate - baselineMedian) / (baselineMAD * scale)
	if z > 3.0 {
		return Decision{
			Fire:   true,
			Reason: "error rate " + formatPct(rate) + " (z=" + formatFloat(z) + ")",
			Score:  math.Min(z/10, 1.0),
		}
	}
	return NoFire()
}

// --- absence detector ---
// Fires when an event that usually arrives at a steady cadence stops entirely.
// "Usually 3 payments/hour, 0 in the last 4 hours" (plan §7.4).

// Absence checks whether a normally-steady event stream has gone silent.
// expectedPerHour is the baseline; observedInWindow is the count in the last
// `windowHours`; fires if observed == 0 AND the baseline is meaningful.
func Absence(expectedPerHour float64, observedInWindow int, windowHours int) Decision {
	if expectedPerHour < 1 {
		return NoFire() // not enough cadence to detect absence
	}
	if windowHours < 2 {
		return NoFire() // need at least 2 hours of silence
	}
	if observedInWindow > 0 {
		return NoFire()
	}
	expected := expectedPerHour * float64(windowHours)
	return Decision{
		Fire:   true,
		Reason: formatFloat(expectedPerHour) + "/hr expected, 0 in " + strconv.Itoa(windowHours) + "h",
		Score:  math.Min(expected/10, 1.0),
	}
}

// --- latency detector ---
// Fires when p95 latency exceeds the baseline by a z-score threshold.

// Latency checks if the current p95 is anomalous.
func Latency(currentP95Ms int, baselineMedianMs int, baselineMADMs int, scale float64) Decision {
	if baselineMADMs <= 0 || scale <= 0 {
		if currentP95Ms > 4000 {
			return Decision{Fire: true, Reason: "p95 above 4s", Score: 0.7}
		}
		return NoFire()
	}
	z := float64(currentP95Ms-baselineMedianMs) / (float64(baselineMADMs) * scale)
	if z > 4.0 {
		return Decision{
			Fire:   true,
			Reason: "p95 " + strconv.Itoa(currentP95Ms) + "ms (z=" + formatFloat(z) + ")",
			Score:  math.Min(z/10, 1.0),
		}
	}
	return NoFire()
}

// --- divergence detector ---
// Fires when two regions disagree (one says up, the other says down). This is
// the "split-brain" check that prevents false positives from a single region's
// network issue.

// Divergence checks if two regions' success rates disagree significantly.
func Divergence(region1OK, region1Total, region2OK, region2Total int) Decision {
	if region1Total < 5 || region2Total < 5 {
		return NoFire()
	}
	r1 := float64(region1OK) / float64(region1Total)
	r2 := float64(region2OK) / float64(region2Total)
	diff := math.Abs(r1 - r2)
	if diff > 0.5 {
		return Decision{
			Fire:   true,
			Reason: "regions disagree (" + formatPct(r1) + " vs " + formatPct(r2) + ")",
			Score:  diff,
		}
	}
	return NoFire()
}

// --- new_fingerprint detector ---
// Fires when a log fingerprint that was never seen before appears in volume.
// This catches "the first time a new error message appears" — often the
// leading indicator of a deploy gone wrong.

// NewFingerprint checks if an unseen fingerprint is appearing at volume.
func NewFingerprint(seenBefore bool, count int) Decision {
	if seenBefore || count < 5 {
		return NoFire()
	}
	return Decision{
		Fire:   true,
		Reason: "new error pattern (" + strconv.Itoa(count) + " occurrences)",
		Score:  math.Min(float64(count)/100, 1.0),
	}
}

// --- burn_rate detector ---
// Burns the error budget at a rate that suggests the SLA will be breached.
// Uses paired windows (plan §7.1): 1h&5m @14.4x, 6h&30m @6x, 3d&6h @1x.

// BurnRate checks if the SLO error budget is burning too fast.
// budget = (1 - target) × period; rate = failedFraction / budget.
func BurnRate(failedFraction, target float64) float64 {
	budget := 1.0 - target
	if budget <= 0 {
		return 0
	}
	return failedFraction / budget
}

// BurnRateDecision evaluates paired windows and returns a Decision + severity.
// page = true means this wakes people (1h&5m @14.4x); false means digest only.
func BurnRateDecision(longRate, shortRate float64, longMultiplier, shortMultiplier float64) (Decision, bool) {
	if longRate >= longMultiplier && shortRate >= shortMultiplier {
		page := longMultiplier >= 14.0
		return Decision{
			Fire:   true,
			Reason: "burn rate " + formatFloat(shortRate) + "x (budget breach)",
			Score:  math.Min(shortRate/20, 1.0),
		}, page
	}
	return NoFire(), false
}

// --- helpers ---

func formatPct(f float64) string {
	return formatFloat(f*100) + "%"
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return strconv.Itoa(int(f))
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}
