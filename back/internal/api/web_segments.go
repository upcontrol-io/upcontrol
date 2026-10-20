// GET and PUT /v1/web/segments: the names a project accepts as segments from
// its site tag. /w stores a beacon's g only when the name is declared here, so
// a page holding the public key can never invent one.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"go.upcontrol.io/back/internal/ingest"
)

// maxSegments caps a project's declared segments: the cap is what keeps a
// segment from becoming a user id.
const maxSegments = 4

// segmentStore is the doors' Postgres half, so the tests run them on a fake.
type segmentStore interface {
	Segments(ctx context.Context, projectID int64) ([]string, error)
	ReplaceSegments(ctx context.Context, projectID int64, names []string) error
}

// keyResolver resolves a presented ingest key; *pg.KeyResolver in production.
type keyResolver interface {
	Resolve(ctx context.Context, key string) (ingest.Tenant, error)
}

// segmentsByKey is the CLI's door: the project is the presented key's own,
// and only a SECRET key reaches it. A public key sits in a page, which could
// otherwise declare its own segments: the same 401 as a key that is not ours.
func segmentsByKey(w http.ResponseWriter, r *http.Request, keys keyResolver, store segmentStore) {
	t, err := keys.Resolve(r.Context(), presentedKey(r))
	if err != nil || t.Kind == ingest.KeyKindPublic {
		writeAPIErr(w, http.StatusUnauthorized, "bad_key")
		return
	}
	serveSegments(w, r, store, t.ProjectID)
}

// serveSegments answers both methods for a resolved project; the session door
// has already let only a manager through to PUT. projectID 0 is a session
// reaching no project here: an empty list to read, nothing to write.
func serveSegments(w http.ResponseWriter, r *http.Request, store segmentStore, projectID int64) {
	ctx := r.Context()
	if r.Method == http.MethodPut {
		var req struct {
			Names *[]string `json:"names"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Names == nil {
			writeAPIErr(w, http.StatusBadRequest, "bad_request")
			return
		}
		names, ok := declaredSegments(*req.Names)
		if !ok {
			writeAPIErrMsg(w, http.StatusBadRequest, "bad_segments", "At most 4 segments, each 1-24 of a-z, 0-9, _ and -, never four digits in a row.")
			return
		}
		if projectID == 0 {
			writeAPIErr(w, http.StatusNotFound, "not_found")
			return
		}
		if err := store.ReplaceSegments(ctx, projectID, names); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
		webSegments.forget(projectID)
	}
	names := []string{}
	if projectID != 0 {
		var err error
		if names, err = store.Segments(ctx, projectID); err != nil {
			writeAPIErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"names": names})
}

// declaredSegments validates a PUT's names, dedupes them in order and caps
// the result at maxSegments; false on any bad name or past the cap.
func declaredSegments(in []string) ([]string, bool) {
	out := []string{}
	for _, n := range in {
		if !validSegment(n) {
			return nil, false
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, len(out) <= maxSegments
}
