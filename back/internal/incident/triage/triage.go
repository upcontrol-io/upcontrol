// Package triage builds the incident card's verdict (plan §7.6): facts computed
// by code, a hypothesis labelled as a guess, a derived verdict, and a runnable
// command rather than advice. The card never changes retroactively — once the
// verdict is set at open time, it stays.
//
// The triage feeds the incident title ("Checkout returning 502"), the timeline
// (deploy join, error spike, check confirmation), and the result strip
// ("fixed" / "still-down") that the Dashboard and incident card render.
package triage

import "fmt"

// Fact is one piece of computed evidence. Facts are labelled as facts — they
// are never a guess.
type Fact struct {
	Kind   string // deploy|error_spike|check_failure|absence|latency
	Detail string // human-readable
}

// Verdict is the incident card's assessment.
type Verdict struct {
	Title      string // the incident title (e.g. "Checkout returning 502")
	Facts      []Fact
	Hypothesis string // a labelled guess (empty if no hypothesis)
	Command    string // a runnable command (empty if none)
	Status     string // fixed|still-down|unknown
}

// DeployContext is the most recent deploy before the incident (if any).
type DeployContext struct {
	Hash    string // git short hash
	Message string // commit message
	At      string // timestamp string
}

// Build constructs a Verdict from the available evidence. The plan (§7.6) is
// explicit: facts computed by code, the guess labelled as a guess, the deploy
// joined in exactly once.
func Build(monitorName string, errorClass string, statusCode int, deploy *DeployContext) Verdict {
	v := Verdict{Status: "unknown"}

	// Title: derived from the monitor name + error class.
	v.Title = buildTitle(monitorName, errorClass, statusCode)

	// Facts: the check result is always a fact.
	v.Facts = append(v.Facts, Fact{
		Kind:   "check_failure",
		Detail: fmt.Sprintf("%s: %s", monitorName, errorLabel(errorClass, statusCode)),
	})

	// Deploy join: if a deploy happened recently, it's a fact (joined once).
	if deploy != nil {
		v.Facts = append(v.Facts, Fact{
			Kind:   "deploy",
			Detail: fmt.Sprintf("Deploy %s — %q", deploy.Hash, truncate(deploy.Message, 60)),
		})
		// Hypothesis: the deploy is the prime suspect (labelled as a guess).
		v.Hypothesis = fmt.Sprintf("Deploy %s may have caused this — rolling back is the fastest test.", deploy.Hash)
		v.Command = fmt.Sprintf("git revert %s && git push", deploy.Hash)
	}

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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
