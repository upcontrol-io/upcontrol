// Shared helpers for the api package.

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func newUUID() pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

func uuidStr(u pgtype.UUID) string {
	return fmt.Sprintf("%x", u.Bytes[:])
}

// decodeStrict is the decoder every management PATCH body goes through: an
// unknown field is a 400 that NAMES the field, never a silent no-op.
func decodeStrict(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	err := dec.Decode(into)
	if err == nil {
		return true
	}
	// Go's message is `json: unknown field "intervl"` — unquote it so the
	// caller learns WHICH key it mistyped, the way the docs' example promises.
	if rest, ok := strings.CutPrefix(err.Error(), `json: unknown field `); ok {
		if field, uerr := strconv.Unquote(rest); uerr == nil {
			writeAPIJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"code": "unknown_field", "field": field},
			})
			return false
		}
	}
	writeAPIErr(w, http.StatusBadRequest, "bad_body")
	return false
}
