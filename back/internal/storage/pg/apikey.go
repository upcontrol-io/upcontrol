// api_key resolver — turns a presented client key into a tenant+project for the
// POST /i ingest handler. The key format is `uc_live_<prefix><secret>`: the
// prefix is the first KeyPrefixLen chars after "uc_live_" (stored in the
// api_key.prefix column, indexed), the rest is the secret. We look up by prefix,
// then verify sha256(full key) == secret_hash, then check the key's state.

package pg

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/ingest"
)

// KeyScheme prefixes the visible key. It is what the CLI prints and the Sources
// #key screen shows.
const KeyScheme = "uc_live_"

// KeyPrefixLen is the number of chars after the scheme used as the lookup
// prefix. The remainder is the secret. Short enough to display, long enough to
// avoid accidental collisions across a tenant's keys.
const KeyPrefixLen = 12

// ErrInvalidKey is the 401 sentinel. It carries no detail to the client.
var ErrInvalidKey = errors.New("pg: invalid api key")

// KeyResolver implements ingest.KeyResolver over the api_key table.
type KeyResolver struct {
	pool *Pool
	now  func() time.Time
}

// NewKeyResolver builds a resolver. now is overridable for tests (rotation
// window expiry); nil uses the real clock.
func NewKeyResolver(p *Pool, now func() time.Time) *KeyResolver {
	if now == nil {
		now = time.Now
	}
	return &KeyResolver{pool: p, now: now}
}

// Resolve verifies the key and returns the tenant+project it authenticates.
func (r *KeyResolver) Resolve(ctx context.Context, fullKey string) (ingest.Tenant, error) {
	prefix, ok := extractPrefix(fullKey)
	if !ok {
		return ingest.Tenant{}, ErrInvalidKey
	}
	row, err := r.pool.Queries().GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		return ingest.Tenant{}, ErrInvalidKey
	}
	// Verify the full key against the stored hash. A wrong secret with a known
	// prefix must not authenticate.
	sum := sha256.Sum256([]byte(fullKey))
	if subtle.ConstantTimeCompare(sum[:], row.SecretHash) != 1 {
		return ingest.Tenant{}, ErrInvalidKey
	}
	// State check: revoked never works; rotating works only inside the window.
	switch row.State {
	case "active":
	case "rotating":
		if row.RotatingUntil.Valid && r.now().After(row.RotatingUntil.Time) {
			return ingest.Tenant{}, ErrInvalidKey
		}
	default: // "revoked" or unknown
		return ingest.Tenant{}, ErrInvalidKey
	}
	return ingest.Tenant{TenantID: row.TenantID, ProjectID: row.ProjectID}, nil
}

// extractPrefix returns the lookup prefix from a full key, validating the scheme
// and minimum length. The key must be `uc_live_<prefix><secret>` with a non-empty
// secret after the prefix.
func extractPrefix(fullKey string) (string, bool) {
	if !strings.HasPrefix(fullKey, KeyScheme) {
		return "", false
	}
	rest := fullKey[len(KeyScheme):]
	if len(rest) < KeyPrefixLen+1 {
		return "", false
	}
	return rest[:KeyPrefixLen], true
}
