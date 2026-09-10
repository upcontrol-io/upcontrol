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

	"go.upcontrol.io/back/internal/account/auth"
	"go.upcontrol.io/back/internal/storage/pg"
)

// planLadder is the upgrade order, cheapest first. Self-hosted is not a cloud
// upgrade target, so a tenant at the top hits the wall with no plan hint.
var planLadder = []string{"Free", "Indie", "Growth", "Agency"}

// cheapestPlan walks the ladder and names the first plan whose entitlement row
// satisfies `fits`, as the ladder spells it. The table decides, not this file:
// limits move without a redeploy. "" means no ladder plan fits: the 402
// carries no plan field and the front shows the message instead of the modal.
// One walk for every axis: two copies is how two walls start pointing at
// different plans for the same table.
func cheapestPlan(ctx context.Context, pool *pg.Pool, fits func(sqlc.PlanEntitlement) bool) string {
	for _, plan := range planLadder {
		ent, err := pool.Queries().GetPlanEntitlement(ctx, plan)
		if err != nil {
			continue
		}
		if fits(ent) {
			return plan
		}
	}
	return ""
}

// upgradePlanForProjects names the cheapest ladder plan with room for one
// more project, lowercased for error.upgrade.plan on the wire.
func upgradePlanForProjects(ctx context.Context, pool *pg.Pool, count int64) string {
	return strings.ToLower(cheapestPlan(ctx, pool, func(ent sqlc.PlanEntitlement) bool {
		return ent.Projects == nil || int64(*ent.Projects) > count
	}))
}

// scope is what a request acts on: the session's person, the current
// project and its workspace. ProjectID is 0 when the person reaches no
// project in the session's workspace (removed from every team, or a
// workspace with no project yet).
type scope struct {
	PersonID  int64
	TenantID  int64 // the current project's workspace: s.TenantID
	ProjectID int64
	Owned     bool   // PersonID owns TenantID
	Role      string // "login" for the owner or an Admin member, "notify" for a Member, "" when ProjectID is 0
}

// resolveScope answers all four facts in two reads. Every failure reads as
// "reaches nothing", which is what the gates below already refuse on.
func resolveScope(ctx context.Context, pool *pg.Pool, s sqlc.Session) scope {
	sc := scope{PersonID: s.PersonID, TenantID: s.TenantID}
	q := pool.Queries()
	var pick int64
	if s.ProjectID != nil {
		pick = *s.ProjectID
	}
	sc.ProjectID, _ = q.ReachableProjectInTenant(ctx, sqlc.ReachableProjectInTenantParams{
		Pick: pick, TenantID: s.TenantID, PersonID: &sc.PersonID,
	})
	// ProjectScope answers ownership and role together; only a person who
	// reaches no project here needs the ownership question on its own.
	if sc.ProjectID != 0 {
		if row, err := q.ProjectScope(ctx, sqlc.ProjectScopeParams{
			PersonID: &sc.PersonID, ProjectID: sc.ProjectID,
		}); err == nil {
			sc.Owned, sc.Role = row.Owned, row.Role
		}
		return sc
	}
	sc.Owned, _ = q.IsTenantOwner(ctx, sqlc.IsTenantOwnerParams{
		TenantID: s.TenantID, PersonID: &sc.PersonID,
	})
	return sc
}

// canManage: the owner of the workspace, or an Admin of the current project.
// A Member (notify) reads.
func (sc scope) canManage() bool { return sc.Owned || sc.Role == "login" }

// canManage is the same question for a handler holding only the session.
func canManage(ctx context.Context, pool *pg.Pool, s sqlc.Session) bool {
	return resolveScope(ctx, pool, s).canManage()
}

// isOwner gates the two acts nobody but the workspace's owner may perform:
// deleting the project and taking the account's data out.
func isOwner(ctx context.Context, pool *pg.Pool, s sqlc.Session) bool {
	personID := s.PersonID
	owned, _ := pool.Queries().IsTenantOwner(ctx, sqlc.IsTenantOwnerParams{
		TenantID: s.TenantID, PersonID: &personID,
	})
	return owned
}

// currentProjectID resolves the session's current project within tenantID:
// the session's pick, but only when the session belongs to that tenant
// (cross-tenant discipline: a foreign or empty session never bends the
// answer) and the person still reaches it, else the lowest project there
// they reach. The caller's gate-authenticated tenantID is the tenant, never
// s.TenantID: a failed session re-read must not turn the query into tenant 0.
// 0 means the person reaches no project there; every caller treats that as
// the no-project miss it already tolerates. A dead read is 0, same thing.
func currentProjectID(ctx context.Context, pool *pg.Pool, s sqlc.Session, tenantID int64) int64 {
	var pick int64
	if s.TenantID == tenantID && s.ProjectID != nil {
		pick = *s.ProjectID
	}
	personID := s.PersonID
	id, _ := pool.Queries().ReachableProjectInTenant(ctx, sqlc.ReachableProjectInTenantParams{
		Pick: pick, TenantID: tenantID, PersonID: &personID,
	})
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

// listProjects answers GET /v1/projects: every project this PERSON reaches —
// their own workspace's first, then the ones they were invited to — no
// `current` flag, since /v1/me owns that fact and a second source for it is
// how the two start disagreeing. The ids are the same uuidStr encoding
// /v1/me's project id uses, so the front's active row is an equality check.
func (h *writeAPI) listProjects(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	personID := s.PersonID
	rows, err := h.pool.Queries().ListProjectsForPerson(r.Context(), &personID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item := map[string]any{
			"id":        uuidStr(row.PublicID),
			"domain":    row.Domain,
			"createdAt": row.CreatedAt,
			"owned":     row.Owned,
			"role":      row.Role,
			"frozen":    row.Frozen,
		}
		// Zero is silence: a workspace with no owner row names nobody.
		if row.OwnerEmail != nil && *row.OwnerEmail != "" {
			item["ownerEmail"] = *row.OwnerEmail
		}
		items = append(items, item)
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
func (h *writeAPI) createProject(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Domain == "" {
		writeAPIErr(w, http.StatusBadRequest, "missing_domain")
		return
	}
	ctx := r.Context()
	// A project is always created in the caller's OWN workspace against their
	// own plan, whatever guest project they happen to stand in.
	tenantID, err := auth.OwnTenantID(ctx, h.pool, s.PersonID, h.selfHosted)
	if err != nil || tenantID == 0 {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	pubID := newUUID()
	var projectID int64
	var createdAt pgtype.Timestamptz
	tx, err := h.pool.Raw().Begin(ctx)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	// The api_key INSERT is issueKeyOfKind's, tx-bound: a pool-bound call would
	// autocommit outside the transaction. The full key stays server-side,
	// exactly as the pool-bound call left it.
	//
	// It is a SECOND copy of that mint and the two must be kept in step. Kind and
	// Origins are here because they were not: `origins` is `text[] NOT NULL`, a
	// missing field encodes as NULL, and every project creation answered 500. The
	// scheme is keyScheme(keyKindSecret) rather than a literal for the same reason —
	// a hand-written prefix here and a derived one there is how a key gets minted
	// under one scheme and looked up under the other.
	secret := randomHex()
	fullKey := keyScheme(keyKindSecret) + secret
	hash := sha256.Sum256([]byte(fullKey))
	if _, err := h.pool.Queries().WithTx(tx).CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     secret[:12],
		SecretHash: hash[:],
		Kind:       keyKindSecret,
		Origins:    []string{},
	}); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// Sign-up and the invitation redeem both seed one (auth.seedEmailChannel);
	// a project created by hand is the third door, and one that reaches nobody
	// is a silent alerting hole.
	if _, err := tx.Exec(ctx,
		`INSERT INTO alert_channel (public_id, tenant_id, project_id, kind, target)
		 SELECT gen_random_uuid(), $1, $2, 'email', lower(btrim(p.email))
		   FROM person p
		  WHERE p.id = $3 AND btrim(COALESCE(p.email, '')) <> ''`,
		tenantID, projectID, s.PersonID); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	// The caller may have been standing in somebody else's project: the switch
	// carries the workspace too, or the session would read the new project
	// against the guest workspace it came from.
	if s.ID != 0 {
		_ = h.pool.Queries().SetSessionScope(ctx, sqlc.SetSessionScopeParams{
			ID: s.ID, TenantID: tenantID, ProjectID: &projectID,
		})
	}
	// Their own workspace, by construction; the front names an owner only on
	// somebody else's card, so no ownerEmail travels here.
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id":        uuidStr(pubID),
		"domain":    req.Domain,
		"createdAt": createdAt,
		"owned":     true,
		"role":      "login",
	})
}

// switchProject answers POST /v1/project/switch. The id is compared against
// the rows this PERSON reaches as encoded on the wire (uuidStr), never
// decoded into a lookup: a stranger's id, a stale one and an unknown one all
// read the same 404 unknown_project, so the endpoint confirms nothing about
// which ids exist. The workspace follows the project — a project reached by
// invite lives in somebody else's. A single-user session (no session row)
// answers 204 without writing: the resolver's fallback owns its current
// project there.
func (h *writeAPI) switchProject(w http.ResponseWriter, r *http.Request, s sqlc.Session) {
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
	personID := s.PersonID
	rows, err := h.pool.Queries().ListProjectsForPerson(ctx, &personID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	for _, row := range rows {
		if uuidStr(row.PublicID) != req.ID {
			continue
		}
		// A frozen project is a snapshot (docs/plans/trial-and-freeze.md): the
		// switch is the one door into it, so the door is where the wall lives.
		// Same answer for owner and guest — the front words the difference.
		if row.Frozen {
			count, _ := h.pool.Queries().CountProjectsByTenant(ctx, row.TenantID)
			writeUpgradeRequired(w,
				"This project is frozen. Reactivate it by upgrading your plan.",
				upgradePlanForProjects(ctx, h.pool, count))
			return
		}
		if err := h.pool.Queries().SetSessionScope(ctx, sqlc.SetSessionScopeParams{
			ID: s.ID, TenantID: row.TenantID, ProjectID: &row.ID,
		}); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeAPIErr(w, http.StatusNotFound, "unknown_project")
}
