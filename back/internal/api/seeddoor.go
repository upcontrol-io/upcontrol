// The operator's seed door (plan parts 2 and 6): POST /internal/seed-host
// mints the page an outreach link points at. It takes the probe node token
// (the production Caddyfile proxies exactly this path at the edge - the one
// /internal/ route exposed, node-token gated under the same trust model as
// the already-proxied probe RPC), counts against the instance mint ceiling
// only - no per-IP limiter, no per-IP ceiling - and never creates a second
// page or a second probe for a host that already has one.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/targetkey"
)

type seedDoor struct {
	wa        *writeAPI
	nodeToken string
}

// NewSeedDoor builds the door; nodeToken is UC_NODE_TOKEN, the same shared
// secret the probe fleet presents to Lease and SubmitResults.
func NewSeedDoor(wa *writeAPI, nodeToken string) *seedDoor {
	return &seedDoor{wa: wa, nodeToken: nodeToken}
}

func (h *seedDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// The same shape rpc.authReq applies to the fleet's connect calls: a
	// bearer token compared for equality, no prefix tricks.
	if a := r.Header.Get("Authorization"); !strings.HasPrefix(a, "Bearer ") || strings.TrimPrefix(a, "Bearer ") != h.nodeToken {
		writeAPIErr(w, http.StatusUnauthorized, "invalid_node_token")
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	host, cerr := canonicalHost(req.Host)
	if cerr != nil {
		writeAPIErr(w, http.StatusBadRequest, "missing_host")
		return
	}
	ctx := r.Context()
	// The same host gate the anonymous mint doors run: a host that asked for
	// removal is never minted again, and an unregistrable host has no page
	// to hand out.
	if refused, code := blockedHostRefused(ctx, h.wa.pool, host); refused {
		if code == "blocked_host" {
			writeAPIErr(w, http.StatusForbidden, code)
		} else {
			writeAPIErr(w, http.StatusBadRequest, code)
		}
		return
	}
	// A host that already has ANY live page gets that page's slug back: no
	// new page, no new tenant, no event. A removed host never reaches here
	// (blocked_host above).
	if existing := hostLivePage(ctx, h.wa.pool, host); existing != nil {
		writeAPIJSON(w, http.StatusOK, map[string]any{
			"slug": existing.Slug, "statusUrl": "/status/" + existing.Slug,
		})
		return
	}
	slug, err := h.seed(ctx, host)
	if err != nil {
		var code refusalError
		if errors.As(err, &code) {
			writeAPIErr(w, http.StatusTooManyRequests, string(code))
			return
		}
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The seed's own page_minted event, the same shape the watch doors fire.
	h.wa.rec.ServerEvent(ctx, "page_minted", 0, 0, map[string]string{"source": "seed"})
	writeAPIJSON(w, http.StatusOK, map[string]any{"slug": slug, "statusUrl": "/status/" + slug})
}

// seed mints the page row in one transaction: an unclaimed tenant, a
// project named after the host, the shared root target (the page's
// reference alone keeps it due - no monitor rows exist on a seed), and the
// page stamped minted_source='seed'. A refusal travels as refusalError,
// exactly like the watch mint, and a refusal leaves nothing behind.
func (h *seedDoor) seed(ctx context.Context, host string) (string, error) {
	tx, err := h.wa.pool.Raw().Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := h.wa.pool.Queries().WithTx(tx)

	// The instance mint ceiling only (UC_MINT_PER_DAY): the day's pages,
	// whatever minted them. The per-IP count never runs here - the operator
	// door has no per-IP audit to count from.
	_, perDay, _ := h.wa.knobsOrDefaults()
	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM status_page WHERE created_at > now() - interval '1 day'`).Scan(&n); err == nil && n >= perDay {
		return "", refusalError("mint_ceiling")
	}
	// The eternal-population cap (UC_HOST_PAGES_MAX): a seed page is an
	// unclaimed host page, exactly the population the cap guards.
	if code := h.wa.hostPagesCeilingRefused(ctx, tx); code != "" {
		return "", refusalError(code)
	}

	// The unclaimed tenant and its project: the demo mint's triple minus the
	// api key (nothing is handed out here) and minus the monitors.
	tenantID, projectID, err := mintUnclaimedTriple(ctx, tx, host)
	if err != nil {
		return "", err
	}
	// The shared root target of https://{host}, the same door every
	// subscription goes through, so a host someone already watches gets a
	// page on the existing probe, never a second one.
	rootURL := "https://" + host
	rootID, err := q.GetOrCreateProbeTarget(ctx, sqlc.GetOrCreateProbeTargetParams{
		Key: targetkey.Website(rootURL, ""), Kind: "website", Url: rootURL,
	})
	if err != nil {
		return "", err
	}
	if err := q.EnsureTargetSchedule(ctx, sqlc.EnsureTargetScheduleParams{
		TargetID: rootID, Region: scheduleRegion(),
	}); err != nil {
		return "", err
	}
	slug := claimSlugOn(ctx, tx, host, projectID)
	if slug == "" {
		slug = "prj-" + strconv.FormatInt(projectID, 10)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO status_page (tenant_id, project_id, slug, title, root_target_id, is_host_page,
		                         minted_source)
		 VALUES ($1, $2, $3, $4, $5, true, 'seed') ON CONFLICT (slug) DO NOTHING`,
		tenantID, projectID, slug, host, rootID); err != nil {
		return "", err
	}
	_ = tx.QueryRow(ctx,
		`SELECT slug FROM status_page WHERE project_id = $1 ORDER BY id LIMIT 1`, projectID).Scan(&slug)
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return slug, nil
}
