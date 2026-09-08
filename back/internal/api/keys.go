// Key management: issuance on signup and on demand, revocation, rotation,
// and the GET /v1/keys response listing the project's key set + recent usage.

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// maxLiveAPIKeys caps how many working keys one project holds. A fixed
// ceiling, never a plan axis: no plan row sells more keys, so the full
// refusal is a 409, never a 402.
const maxLiveAPIKeys = 5

// keys handles GET/POST /v1/keys, DELETE /v1/keys/{id} and POST /v1/keys/rotate.
type keys struct {
	pool *pg.Pool
	sess *session.Manager
}

func NewKeys(p *pg.Pool, sm *session.Manager) *keys {
	return &keys{pool: p, sess: sm}
}

func (h *keys) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	// The key is the project's, not the workspace's: a sibling project keeps
	// its own.
	projectID := currentProjectID(r.Context(), h.pool, s, s.TenantID)
	switch r.URL.Path {
	case "/v1/keys":
		switch r.Method {
		case http.MethodGet:
			h.get(w, r, projectID)
		case http.MethodPost:
			// A new key reaches the ingest API like any other: a settings act.
			if !canManage(r.Context(), h.pool, s) {
				writeAPIErr(w, http.StatusForbidden, "notify_role")
				return
			}
			h.issue(w, r, s.TenantID, projectID)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case "/v1/keys/rotate": // exact arm first: as an {id}, "rotate" would be a key id
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Rotating the key breaks every deployed SDK: a settings act.
		if !canManage(r.Context(), h.pool, s) {
			writeAPIErr(w, http.StatusForbidden, "notify_role")
			return
		}
		h.rotate(w, r, s.TenantID, projectID)
	default:
		id, ok := strings.CutPrefix(r.URL.Path, "/v1/keys/")
		if !ok || r.Method != http.MethodDelete {
			writeAPIErr(w, http.StatusNotFound, "not_found")
			return
		}
		// Withdrawing a key is what you do when it leaked: a settings act.
		if !canManage(r.Context(), h.pool, s) {
			writeAPIErr(w, http.StatusForbidden, "notify_role")
			return
		}
		h.revoke(w, r, projectID, id)
	}
}

func (h *keys) get(w http.ResponseWriter, r *http.Request, projectID int64) {
	ctx := r.Context()
	key, keyErr := h.pool.Queries().GetAPIKeyForProject(ctx, projectID)

	usageRows, _ := h.pool.Queries().ListKeyUsage(ctx, projectID)
	usage := make([]map[string]any, 0, len(usageRows))
	for _, u := range usageRows {
		ts := ""
		if u.At.Valid {
			ts = u.At.Time.Format("15:04")
		}
		usage = append(usage, map[string]any{
			"time":     ts,
			"endpoint": u.Source,
			"status":   202,
		})
		if u.Outcome == "rejected" {
			usage[len(usage)-1]["status"] = 401
		}
	}

	rows, _ := h.pool.Queries().ListAPIKeysForProject(ctx, projectID)
	list := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		list = append(list, map[string]any{
			"id":         "key_" + strconv.FormatInt(k.ID, 10),
			"prefix":     keyScheme(k.Kind) + k.Prefix, // identifier only — the secret is never stored, never returned here
			"createdAt":  keyTime(k.CreatedAt),
			"name":       k.Name,
			"state":      k.State,
			"kind":       k.Kind,
			"origins":    k.Origins,
			"lastUsedAt": keyTime(k.LastUsedAt),
			"revokedAt":  keyTime(k.RevokedAt),
		})
	}

	// `key` keeps exactly what it always meant — the newest key that is not
	// revoked, or null — so a front older than the list keeps working.
	var current map[string]any
	if keyErr == nil {
		current = map[string]any{
			"id":        "key_" + strconv.FormatInt(key.ID, 10),
			"prefix":    keyScheme(key.Kind) + key.Prefix,
			"createdAt": key.CreatedAt,
		}
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"key":   current,
		"keys":  list,
		"usage": usage,
	})
}

func (h *keys) issue(w http.ResponseWriter, r *http.Request, tenantID, projectID int64) {
	ctx := r.Context()
	name := ""
	kind := keyKindSecret
	var origins []string
	if r.ContentLength != 0 { // the body is optional: a bare POST issues an unnamed secret key
		var body struct {
			Name    string   `json:"name"`
			Kind    string   `json:"kind"`
			Origins []string `json:"origins"`
		}
		if !decodeStrict(w, r, &body) {
			return
		}
		name = strings.TrimSpace(body.Name)
		if runes := []rune(name); len(runes) > 60 { // the contract's maxLength, rune-safe
			name = string(runes[:60])
		}
		switch strings.TrimSpace(body.Kind) {
		case "", keyKindSecret:
		case keyKindPublic:
			kind = keyKindPublic
		default:
			writeAPIErrMsg(w, http.StatusBadRequest, "bad_kind", "A key is either secret or public.")
			return
		}
		for _, o := range body.Origins {
			if o = strings.TrimSpace(o); o != "" {
				origins = append(origins, o)
			}
		}
	}
	// A public key with no origin is the unscoped key it exists to replace, so it is refused
	// at the door rather than minted and quietly useless. Origins on a secret key are dropped:
	// no browser ever presents one, so honouring them would only imply a limit that is not there.
	if kind == keyKindPublic && len(origins) == 0 {
		writeAPIErrMsg(w, http.StatusBadRequest, "origins_required",
			"A public key must list the origins it may be sent from — it is shipped in a browser bundle, and the origin list is its whole scope.")
		return
	}
	if kind != keyKindPublic {
		origins = nil
	}
	live, err := h.pool.Queries().CountLiveAPIKeys(ctx, projectID)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "issue_failed")
		return
	}
	if live >= maxLiveAPIKeys {
		writeAPIErrMsg(w, http.StatusConflict, "key_limit",
			fmt.Sprintf("This project already holds %d keys that still work; revoke one first.", maxLiveAPIKeys))
		return
	}
	row, fullKey, err := issueKeyOfKind(ctx, h.pool, tenantID, projectID, name, kind, origins)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "issue_failed")
		return
	}
	writeAPIJSON(w, http.StatusCreated, map[string]any{
		"id":        "key_" + strconv.FormatInt(row.ID, 10),
		"prefix":    keyScheme(row.Kind) + row.Prefix, // what GET /v1/keys will list from now on
		"createdAt": keyTime(row.CreatedAt),
		"name":      row.Name,
		"kind":      row.Kind,
		"origins":   row.Origins,
		"value":     fullKey, // shown exactly once
	})
}

func (h *keys) revoke(w http.ResponseWriter, r *http.Request, projectID int64, id string) {
	// The caller learns only that this project has no such key: an id from
	// another project and an unparseable one answer the same 404.
	raw, ok := strings.CutPrefix(id, "key_")
	n, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil {
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	rows, err := h.pool.Queries().RevokeAPIKey(r.Context(), sqlc.RevokeAPIKeyParams{ID: n, ProjectID: projectID})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "revoke_failed")
		return
	}
	if rows == 0 { // already revoked, another project's, or no such row
		writeAPIErr(w, http.StatusNotFound, "not_found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *keys) rotate(w http.ResponseWriter, r *http.Request, tenantID, projectID int64) {
	ctx := r.Context()

	secret := randomHex() // 32 hex chars; first 12 = prefix, rest = secret
	prefix := secret[:12]
	fullKey := "uc_live_" + secret
	hash := sha256.Sum256([]byte(fullKey))

	row, err := h.pool.Queries().RotateAPIKey(ctx, sqlc.RotateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     prefix,
		SecretHash: hash[:],
	})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "rotate_failed")
		return
	}

	writeAPIJSON(w, http.StatusOK, map[string]any{
		"id":        "key_" + strconv.FormatInt(row.ID, 10),
		"prefix":    "uc_live_" + prefix, // what GET /v1/keys will list from now on
		"value":     fullKey,             // shown exactly once
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
}

// issueKey creates an API key for a new project. Called from the signup flow.
func issueKey(ctx context.Context, pool *pg.Pool, tenantID, projectID int64) (fullKey string, err error) {
	_, fullKey, err = issueNamedKey(ctx, pool, tenantID, projectID, "")
	return fullKey, err
}

// issueNamedKey is the one mint, named or not: a random secret, its first
// twelve chars the display prefix, sha256 of the whole key what lands in the
// row. Nothing else in this package mints.
func issueNamedKey(ctx context.Context, pool *pg.Pool, tenantID, projectID int64, name string) (row sqlc.CreateAPIKeyRow, fullKey string, err error) {
	return issueKeyOfKind(ctx, pool, tenantID, projectID, name, keyKindSecret, nil)
}

// issueKey mints a key of either kind. The scheme is part of what is hashed, so
// it is chosen here and nowhere else: a key hashed under one scheme and
// presented under the other never authenticates.
func issueKeyOfKind(ctx context.Context, pool *pg.Pool, tenantID, projectID int64, name, kind string, origins []string) (row sqlc.CreateAPIKeyRow, fullKey string, err error) {
	secret := randomHex()
	prefix := secret[:12]
	fullKey = keyScheme(kind) + secret
	hash := sha256.Sum256([]byte(fullKey))
	if origins == nil {
		origins = []string{}
	}
	row, err = pool.Queries().CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     prefix,
		SecretHash: hash[:],
		Name:       name,
		Kind:       kind,
		Origins:    origins,
	})
	return row, fullKey, err
}

const (
	keyKindSecret = "secret"
	keyKindPublic = "public"
)

// keyScheme is the visible prefix a kind is minted and presented under. Anything
// that is not explicitly public is secret: an unknown kind must never widen a
// key's reach by accident.
func keyScheme(kind string) string {
	if kind == keyKindPublic {
		return "uc_pub_"
	}
	return "uc_live_"
}

// randomHex mints the 16 random bytes (32 hex chars) behind a uc_live_ key:
// 12 hex of prefix for display, the rest the secret itself.
func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// keyTime answers a timestamp as RFC3339, null when it never happened: a key
// nothing has presented has no lastUsedAt, a live one no revokedAt.
func keyTime(t pgtype.Timestamptz) any {
	if !t.Valid {
		return nil
	}
	return t.Time.UTC().Format(time.RFC3339)
}
