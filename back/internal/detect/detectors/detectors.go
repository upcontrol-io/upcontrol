// Package detector implements the correlation detectors: each examines recent
// aggregates against a baseline and returns a Decision when a threshold is crossed.
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

func formatPct(f float64) string {
	return formatFloat(f*100) + "%"
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return strconv.Itoa(int(f))
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}
