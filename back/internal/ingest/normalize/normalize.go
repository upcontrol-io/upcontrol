// Package normalize maps a client event name to its canonical tier in the closed
// 24-event dictionary (plan §4.3, cli/SPEC §4). The dictionary is frozen: field
// additions are a major version. Everything outside it is T4 — an ordinary log
// line, shown in the window/slice, never the trigger of anything.
//
// The reserved prefix "uc.*" belongs to upcontrol itself: client-sent events
// with it are dropped and a `reserved_prefix` warning rides the receipt (the
// receipt's closed code list). Classify never errors — it returns a tier and a
// reserved flag, and the caller decides what to do.
package normalize

// Tier is the event's place in the alerting/correlation ladder.
type Tier uint8

const (
	TierUnknown Tier = 0
	// Tier1: baseline + alerting, may wake (12 events).
	Tier1 Tier = 1
	// Tier2: baseline + dashboard, do not wake by default (10 events).
	Tier2 Tier = 2
	// Tier3: never alert, highest correlation value — deploy and install_verified (2).
	Tier3 Tier = 3
	// Tier4: everything else — an ordinary log line.
	Tier4 Tier = 4
	// Tier5: the reserved "uc.*" prefix — rejected from clients.
	Tier5 Tier = 5
)

// ReservedPrefix is the namespace upcontrol owns; clients may not send it.
const ReservedPrefix = "uc."

// canonical is the frozen 24-name dictionary → tier. Lookup is case-insensitive
// on the canonical snake_case form; the returned Name is always the canonical
// spelling so a tenant who sends "Payment_Succeeded" stores "payment_succeeded".
var canonical = map[string]struct {
	tier Tier
	name string
}{
	// T1 — may wake.
	"payment_succeeded":      {Tier1, "payment_succeeded"},
	"payment_failed":         {Tier1, "payment_failed"},
	"refund_issued":          {Tier1, "refund_issued"},
	"subscription_cancelled": {Tier1, "subscription_cancelled"},
	"job_failed":             {Tier1, "job_failed"},
	"heartbeat":              {Tier1, "heartbeat"},
	"unhandled_exception":    {Tier1, "unhandled_exception"},
	"request_failed":         {Tier1, "request_failed"},
	"external_api_failed":    {Tier1, "external_api_failed"},
	"email_failed":           {Tier1, "email_failed"},
	"login_failed":           {Tier1, "login_failed"},
	"app_started":            {Tier1, "app_started"},

	// T2 — baseline + dashboard, do not wake.
	"job_started":          {Tier2, "job_started"},
	"job_done":             {Tier2, "job_done"},
	"checkout_started":     {Tier2, "checkout_started"},
	"subscription_created": {Tier2, "subscription_created"},
	"external_api_slow":    {Tier2, "external_api_slow"},
	"email_sent":           {Tier2, "email_sent"},
	"signup":               {Tier2, "signup"},
	"upload_finished":      {Tier2, "upload_finished"},
	"import_finished":      {Tier2, "import_finished"},
	"app_stopped":          {Tier2, "app_stopped"},

	// T3 — never alert, highest correlation value.
	"deploy":           {Tier3, "deploy"},
	"install_verified": {Tier3, "install_verified"},
}

// Event is the classification result. Name is the canonical spelling (empty for
// T4/T5, where there is no canonical name to normalize to).
type Event struct {
	Name string // canonical spelling; empty unless Tier1-T3
	Tier Tier
}

// Classify maps a client event name to its tier. It never errors:
//   - "uc." prefix → Tier5 (reserved); caller drops + warns reserved_prefix.
//   - one of the 24 → Tier1-T3 with the canonical Name.
//   - anything else → Tier4 (ordinary line).
//
// Lookup is case-insensitive and ignores leading/trailing whitespace, so a
// tenant's "  Payment_Failed  " stores as "payment_failed".
func Classify(name string) Event {
	n := trimLower(name)
	if n == "" {
		return Event{Tier: Tier4}
	}
	// Reserved prefix check first: a client cannot claim the upcontrol namespace
	// even if (somehow) it collides with a canonical name.
	if hasReservedPrefix(n) {
		return Event{Tier: Tier5}
	}
	if e, ok := canonical[n]; ok {
		return Event{Name: e.name, Tier: e.tier}
	}
	return Event{Tier: Tier4}
}

func hasReservedPrefix(n string) bool {
	return len(n) >= len(ReservedPrefix) && n[:len(ReservedPrefix)] == ReservedPrefix
}

func trimLower(s string) string {
	// Manual trim+lower to avoid pulling strings (keeps the hot path alloc-free
	// for the common T4 path where we only need to lowercase before lookup).
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	out := make([]byte, 0, end-start)
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
