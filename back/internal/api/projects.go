// The projects plan axis (docs/plans/projects-axis.md): Free 1, Indie 2,
// Growth 5, Agency 10 projects in the one tenant a person has; Self-hosted's
// NULL row means unlimited, the same contract telegram_recipients carries.
// The gate and the upgrade hint live here so claim, create and the watch door
// read the same ladder, and the ladder reads the entitlement table, never this
// file.

package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"

	"go.upcontrol.io/back/internal/storage/pg"
)

// planLadder is the upgrade order, cheapest first. Self-hosted is not a cloud
// upgrade target, so a tenant at the top hits the wall with no plan hint.
var planLadder = []string{"Free", "Indie", "Growth", "Agency"}

// upgradePlanForProjects names the cheapest ladder plan with room for one
// more project, lowercased for error.upgrade.plan on the wire. The table
// decides, not this file: limits move without a redeploy. "" means every
// ladder plan is full — the 402 carries no plan field and the front shows the
// message instead of the modal.
func upgradePlanForProjects(ctx context.Context, pool *pg.Pool, count int64) string {
	for _, plan := range planLadder {
		ent, err := pool.Queries().GetPlanEntitlement(ctx, plan)
		if err != nil {
			continue
		}
		if ent.Projects == nil || int64(*ent.Projects) > count {
			return strings.ToLower(plan)
		}
	}
	return ""
}

// currentProjectID resolves the session's current project within tenantID:
// the session's pick, but only when the session belongs to that tenant
// (cross-tenant discipline: a foreign or empty session never bends the
// answer), else the tenant's lowest project id (Decision 14 — single-user
// self-host and an unset or stale pick all land there). The caller's
// gate-authenticated tenantID is the tenant, never s.TenantID: a failed
// session re-read must not turn the query into tenant 0. 0 means the tenant
// has no project; every caller treats that as the no-project miss it already
// tolerates. A dead read is 0, same thing.
func currentProjectID(ctx context.Context, pool *pg.Pool, s sqlc.Session, tenantID int64) int64 {
	var pick int64
	if s.TenantID == tenantID && s.ProjectID != nil {
		pick = *s.ProjectID
	}
	var id int64
	_ = pool.Raw().QueryRow(ctx,
		`SELECT COALESCE((SELECT id FROM project WHERE id = $2 AND tenant_id = $1),
				(SELECT min(id) FROM project WHERE tenant_id = $1), 0)`,
		tenantID, pick).Scan(&id)
	return id
}

// projectsRefusalQ is the one owner of the projects wall (Decision 16's
// message format): "" as long as the tenant may hold another project, else
// the message the 402 shows (mirroring the http_checks wording) and the plan
// that lifts the limit. The count comes through q so a caller holding an
// open transaction (claim's absorb) counts what it has already written; plan
// and entitlement stay pool reads, static rows. A missing tenant plan reads
// as Free; a dead entitlement or count read fails open — the walls do not
// 500 the write.
func projectsRefusalQ(ctx context.Context, pool *pg.Pool, q *sqlc.Queries, tenantID int64) (msg, plan string) {
	plan, _ = pool.Queries().GetTenantPlan(ctx, tenantID)
	if plan == "" {
		plan = "Free"
	}
	ent, err := pool.Queries().GetPlanEntitlement(ctx, plan)
	if err != nil || ent.Projects == nil {
		return "", "" // NULL = unlimited
	}
	limit := int64(*ent.Projects)
	count, err := q.CountProjectsByTenant(ctx, tenantID)
	if err != nil || count < limit {
		return "", ""
	}
	noun := " project"
	if limit != 1 {
		noun = " projects"
	}
	return plan + " allows " + strconv.FormatInt(limit, 10) + noun + ".",
		upgradePlanForProjects(ctx, pool, count)
}

// projectsRefusal is the pool-bound read of the wall; claim's in-transaction
// variant lives in adoptTenant (projectsRefusalQ with WithTx(tx)).
func (h *writeAPI) projectsRefusal(ctx context.Context, tenantID int64) (msg, plan string) {
	return projectsRefusalQ(ctx, h.pool, h.pool.Queries(), tenantID)
}

// listProjects answers GET /v1/projects: every project in the tenant, oldest
// first, no `current` flag — /v1/me owns that fact, and a second source for
// it is how the two start disagreeing. The ids are the same uuidStr encoding
// /v1/me's project id uses, so the front's active row is an equality check.
func (h *writeAPI) listProjects(w http.ResponseWriter, r *http.Request, tenantID int64) {
	rows, err := h.pool.Queries().ListProjectsByTenant(r.Context(), tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{
			"id":        uuidStr(row.PublicID),
			"domain":    row.Domain,
			"createdAt": row.CreatedAt,
		})
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"projects": items})
}

// createProject answers POST /v1/projects: the wall first, then the same
// provisioning a fresh account gets in ensureAccount (auth.go) — project,
// project_seq and an active API key, so the ingest door is open from the
// first minute. The three writes ride one transaction (adoptTenant's
// pattern): unlike signup this call is user-retryable, and a mid-provision
// failure left committed would mint a half-provisioned project that eats a
// plan slot on the retry — on error nothing persists. Success points the
// session at the new project (a single-user session has no row; the UPDATE
// matches nothing there), after the commit: the pick is idempotent and never
// gates the provisioning.
func (h *writeAPI) createProject(w http.ResponseWriter, r *http.Request, tenantID int64, s sqlc.Session) {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Domain == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_domain")
		return
	}
	ctx := r.Context()
	pubID := newUUID()
	var projectID int64
	var createdAt pgtype.Timestamptz
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer tx.Rollback(ctx)
	// The tenant row is the lock: the gate counts projects, and two creates
	// arriving together would each count the other's row as absent and both
	// pass, putting a Free account on two projects. Counting inside the
	// transaction is not enough on its own — READ COMMITTED hides the other
	// writer's uncommitted row — so the count runs behind FOR UPDATE.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tenant WHERE id = $1 FOR UPDATE`, tenantID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if msg, plan := projectsRefusalQ(ctx, h.pool, h.pool.Queries().WithTx(tx), tenantID); msg != "" {
		writeUpgradeRequired(w, msg, plan)
		return
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO project (public_id, tenant_id, domain) VALUES ($1, $2, $3)
		  RETURNING id, created_at`,
		pubID, tenantID, req.Domain).Scan(&projectID, &createdAt); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO project_seq (project_id, next) VALUES ($1, 1) ON CONFLICT DO NOTHING`, projectID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The api_key INSERT is issueKey's, tx-bound: a pool-bound call would
	// autocommit outside the transaction. The full key stays server-side,
	// exactly as the pool-bound call left it.
	secret := randomHex()
	fullKey := "uc_live_" + secret
	hash := sha256.Sum256([]byte(fullKey))
	if _, err := h.pool.Queries().WithTx(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     secret[:12],
		SecretHash: hash[:],
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if s.ID != 0 {
		_ = h.pool.Queries().SetSessionProject(ctx, sqlc.SetSessionProjectParams{
			ID: s.ID, ProjectID: &projectID,
		})
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id":        uuidStr(pubID),
		"domain":    req.Domain,
		"createdAt": createdAt,
	})
}

// switchProject answers POST /v1/project/switch. The id is compared against
// the tenant's own rows as encoded on the wire (uuidStr), never decoded into
// a lookup: an id of another tenant, a stale one and an unknown one all read
// the same 404 unknown_project, so the endpoint confirms nothing about which
// ids exist. A single-user session (no session row) answers 204 without
// writing: the resolver's fallback owns its current project there.
func (h *writeAPI) switchProject(w http.ResponseWriter, r *http.Request, tenantID int64, s sqlc.Session) {
	var req struct {
		ID string `json:"id"`
	}
	// A body that will not parse is a malformed request, not a missing
	// project: 400 says which. Only a well-formed id that names nothing the
	// tenant owns gets the 404 below, and every flavour of that reads alike.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.ID == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_project_id")
		return
	}
	if s.ID == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	rows, err := h.pool.Queries().ListProjectsByTenant(ctx, tenantID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, row := range rows {
		if uuidStr(row.PublicID) != req.ID {
			continue
		}
		if err := h.pool.Queries().SetSessionProject(ctx, sqlc.SetSessionProjectParams{
			ID: s.ID, ProjectID: &row.ID,
		}); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeAPIErr(w, http.StatusNotFound, "unknown_project")
}
