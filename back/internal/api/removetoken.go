// The public removal-token door (plan part 2): POST /public/status/{slug}/
// remove-token issues the DNS TXT token a site owner publishes to take a
// page down themselves. The ucworker dns-tokens job resolves the record and
// performs the removal; this door only hands out and stores the token.

package api

import (
	"net/http"
	"time"

	"go.upcontrol.io/back/internal/analytics"
	"golang.org/x/net/publicsuffix"
)

// removeTXTRecord is the DNS record name the removal flow publishes and the
// worker's dns-tokens job resolves: _upcontrol-remove.<eTLD+1>. The worker
// carries the same literal in its removeByToken loop (internal/worker/
// worker.go); the name lives in exactly one DNS spec, mirrored there.
const removeTXTRecord = "_upcontrol-remove."

type removeTokenDoor struct {
	wa *writeAPI
}

// NewRemoveTokenDoor builds the door on the shared write API (the pool, the
// per-replica throttle map, the page lookups).
func NewRemoveTokenDoor(wa *writeAPI) *removeTokenDoor {
	return &removeTokenDoor{wa: wa}
}

func (h *removeTokenDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// One issue per IP per minute, the watchAllow shape: an in-memory
	// per-replica window, deliberately coarse - the token is idempotent, so
	// a legitimate second click costs nothing but a 429.
	if !h.wa.allowOnce("remove-token", analytics.ClientIP(r), time.Minute) {
		writeAPIErr(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	slug := r.PathValue("slug")
	ctx := r.Context()
	var pageID int64
	var projectID int64
	var removedAt *time.Time
	var isHostPage bool
	var existing *string
	err := h.wa.pool.Raw().QueryRow(ctx,
		`SELECT sp.id, sp.project_id, sp.removed_at, sp.is_host_page, sp.removal_token
		   FROM status_page sp
		  WHERE sp.slug = $1`, slug).Scan(&pageID, &projectID, &removedAt, &isHostPage, &existing)
	if err != nil {
		writeAPIErr(w, http.StatusNotFound, "no_such_page")
		return
	}
	// A removed page answers 410 on every door.
	if removedAt != nil {
		writeAPIErr(w, http.StatusGone, "page_removed")
		return
	}
	// Only host pages are removable here. A claimed non-host page belongs to
	// its owner's project: they delete the project, and there is nothing a
	// stranger should be able to take down.
	if !isHostPage {
		writeAPIErr(w, http.StatusBadRequest, "not_removable")
		return
	}
	var domain string
	if err := h.wa.pool.Raw().QueryRow(ctx,
		`SELECT domain FROM project WHERE id = $1`, projectID).Scan(&domain); err != nil || domain == "" {
		writeAPIErr(w, http.StatusBadRequest, "not_removable")
		return
	}
	registrable, rerr := publicsuffix.EffectiveTLDPlusOne(domain)
	if rerr != nil {
		// A host page whose project domain does not reduce to a registrable
		// domain can publish no TXT record; it cannot be removed by DNS.
		writeAPIErr(w, http.StatusBadRequest, "not_removable")
		return
	}
	// Idempotent issue: return the standing token, or mint one exactly once
	// (the WHERE guard makes a race mint at most one).
	token := ""
	if existing != nil {
		token = *existing
	} else {
		token = randomHex()
		if _, err := h.wa.pool.Raw().Exec(ctx,
			`UPDATE status_page SET removal_token = $2 WHERE id = $1 AND removal_token IS NULL`,
			pageID, token); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		// Another caller may have won the race: the stored value is the one
		// the worker will verify, so read it back.
		_ = h.wa.pool.Raw().QueryRow(ctx,
			`SELECT removal_token FROM status_page WHERE id = $1`, pageID).Scan(&token)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"token":  token,
		"record": removeTXTRecord + registrable,
		"domain": registrable,
	})
}
