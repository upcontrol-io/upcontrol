// Shared helpers for the api package.

package api

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

func newUUID() pgtype.UUID {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return pgtype.UUID{Bytes: u, Valid: true}
}

func uuidStr(u pgtype.UUID) string {
	return fmt.Sprintf("%x", u.Bytes[:])
}

// decodeStrict is the decoder every management PATCH body goes through
// (audit §14 / docs' "Management is strict" card): an unknown field is a
// precise 400 that NAMES the field, never a silent no-op. A typo in a
// management call used to answer 200 while changing nothing — the caller
// believed `{"url": …}` had retargeted their check, and nothing had.
//
// The front is on the typed contract (client.ts sends exactly the schema
// fields), so this only bites external clients — which is the point: the
// management API is the side that is allowed to say no.
//
// Returns false (and answers the request) when the body is unacceptable.
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
