// Package dnstokens holds the DNS TXT record name the permanent status pages
// publish (plan part 2), so the worker that resolves the record and the
// removal door that surfaces it to owners can never drift. The constant is
// the label WITH its trailing dot; the caller appends the registrable domain.
package dnstokens

// RemoveRecord is the TXT record a host owner publishes to take a page down
// themselves: _upcontrol-remove.<eTLD+1> carrying the removal token.
const RemoveRecord = "_upcontrol-remove."
