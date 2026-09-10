// Package dnstokens holds the DNS TXT record names the permanent status
// pages publish (plan parts 2 and 4). One home for both arms so the worker
// that resolves the records and the API that surfaces them to owners can
// never drift: the dns-tokens job (internal/worker) looks these up, the page
// settings answer carries the verification record, and the removal door
// answers with the removal record. Each constant is the label WITH its
// trailing dot; the caller appends the registrable domain.
package dnstokens

// VerifyRecord is the TXT record a page owner publishes to prove control of
// the host: _upcontrol-verify.<eTLD+1> carrying the verification token.
const VerifyRecord = "_upcontrol-verify."

// RemoveRecord is the TXT record a host owner publishes to take a page down
// themselves: _upcontrol-remove.<eTLD+1> carrying the removal token.
const RemoveRecord = "_upcontrol-remove."
