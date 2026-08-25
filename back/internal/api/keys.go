// Key management: issuance on signup, rotation, and the GET /v1/keys
// response showing prefix + recent usage.

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	sqlc "go.upcontrol.io/back/gen/pg"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pg"
)

// Keys handles GET /v1/keys and POST /v1/keys/rotate.
type Keys struct {
	pool *pg.Pool
	sess *session.Manager
}

func NewKeys(p *pg.Pool, sm *session.Manager) *Keys {
	return &Keys{pool: p, sess: sm}
}

func (h *Keys) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s, err := h.sess.FromRequest(r.Context(), r)
	if err != nil {
		writeAPIErr(w, http.StatusUnauthorized, "no_session")
		return
	}
	switch r.URL.Path {
	case "/v1/keys":
		h.get(w, r, s.TenantID)
	case "/v1/keys/rotate":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Rotating the key breaks every deployed SDK: a settings act.
		if !roleAtLeastLogin(r.Context(), h.pool, s.PersonID, s.TenantID) {
			writeAPIErr(w, http.StatusForbidden, "notify_role")
			return
		}
		h.rotate(w, r, s.TenantID)
	default:
		writeAPIErr(w, http.StatusNotFound, "not_found")
	}
}

func (h *Keys) get(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()
	key, err := h.pool.Queries().GetAPIKeyForTenant(ctx, tenantID)
	if err != nil {
		writeAPIJSON(w, http.StatusOK, map[string]any{"key": nil, "usage": []any{}})
		return
	}
	usageRows, _ := h.pool.Queries().ListKeyUsage(ctx, tenantID)
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
	writeAPIJSON(w, http.StatusOK, map[string]any{
		"key": map[string]any{
			"id":        "key_" + strconv.FormatInt(key.ID, 10),
			"prefix":    "uc_live_" + key.Prefix, // identifier only — the secret is never stored, never returned here
			"createdAt": key.CreatedAt,
		},
		"usage": usage,
	})
}

func (h *Keys) rotate(w http.ResponseWriter, r *http.Request, tenantID int64) {
	ctx := r.Context()

	secret := randomHex() // 32 hex chars; first 12 = prefix, rest = secret
	prefix := secret[:12]
	fullKey := "uc_live_" + secret
	hash := sha256.Sum256([]byte(fullKey))

	row, err := h.pool.Queries().RotateAPIKey(ctx, sqlc.RotateAPIKeyParams{
		TenantID:   tenantID,
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

// IssueKey creates an API key for a new project. Called from the signup flow.
func IssueKey(ctx context.Context, pool *pg.Pool, tenantID, projectID int64) (fullKey string, err error) {
	secret := randomHex()
	prefix := secret[:12]
	fullKey = "uc_live_" + secret
	hash := sha256.Sum256([]byte(fullKey))
	_, err = pool.Queries().CreateAPIKey(ctx, sqlc.CreateAPIKeyParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Prefix:     prefix,
		SecretHash: hash[:],
	})
	return fullKey, err
}

// randomHex mints the 16 random bytes (32 hex chars) behind a uc_live_ key:
// 12 hex of prefix for display, the rest the secret itself.
func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = json.NewEncoder
