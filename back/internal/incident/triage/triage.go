// Package triage builds the incident card's verdict: facts computed by code,
// the title the front renders.
package triage

import "fmt"

// Fact is one piece of computed evidence. Facts are labelled as facts — they
// are never a guess.
type Fact struct {
	Kind   string // check_failure
	Detail string // human-readable
}

// Verdict is the incident card's assessment.
type Verdict struct {
	Title  string // the incident title (e.g. "Checkout returning 502")
	Facts  []Fact
	Status string // fixed|still-down|unknown
}

// Build constructs a Verdict from the available evidence: facts computed by
// code and the title derived from the check result.
func Build(monitorName string, errorClass string, statusCode int) Verdict {
	v := Verdict{Status: "unknown"}

	// Title: derived from the monitor name + error class.
	v.Title = buildTitle(monitorName, errorClass, statusCode)

	// Facts: the check result is always a fact.
	v.Facts = append(v.Facts, Fact{
		Kind:   "check_failure",
		Detail: fmt.Sprintf("%s: %s", monitorName, errorLabel(errorClass, statusCode)),
	})

	return v
}

// buildTitle produces the incident title the front renders.
func buildTitle(monitorName, errorClass string, statusCode int) string {
	switch errorClass {
	case "status":
		return fmt.Sprintf("%s returning %d", monitorName, statusCode)
	case "timeout":
		return monitorName + " timed out"
	case "connect":
		return monitorName + " connection refused"
	case "dns":
		return monitorName + " DNS error"
	case "keyword_missing":
		return monitorName + " keyword not found"
	default:
		return monitorName + " is down"
	}
}

func errorLabel(errorClass string, statusCode int) string {
	switch errorClass {
	case "status":
		return fmt.Sprintf("HTTP %d", statusCode)
	case "timeout":
		return "timed out"
	case "connect":
		return "connection refused"
	case "dns":
		return "DNS resolution failed"
	case "keyword_missing":
		return "keyword assertion failed"
	default:
		return "unreachable"
	}
}
