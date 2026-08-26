package analytics

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"net"
	"os"

	"github.com/oschwald/geoip2-golang"
)

// defaultGeoDB is where a deployment drops the MMDB country database. The file
// is an 8 MB monthly download, not part of the source tree: see infra/README.md.
const defaultGeoDB = "/var/lib/upcontrol/geoip/country.mmdb"

// geo resolves an IP to an ISO country code. A nil *geo is valid and answers
// "": a missing database degrades to "country unknown".
type geo struct {
	db *geoip2.Reader
}

// openGeo opens the database named by UC_GEOIP_DB. No file installed is the
// normal state, not an error: (nil, nil) means every country reads "".
func openGeo() (*geo, error) {
	path := os.Getenv("UC_GEOIP_DB")
	if path == "" {
		path = defaultGeoDB
	}
	db, err := geoip2.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &geo{db: db}, nil
}

// Country returns the ISO 3166-1 alpha-2 code for ip, or "" when unknown;
// the return value is the only thing kept from the raw IP.
func (g *geo) Country(ip string) string {
	if g == nil || g.db == nil || ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	rec, err := g.db.Country(parsed)
	if err != nil {
		return ""
	}
	return rec.Country.IsoCode
}

// ipHash is the grouping key for a client IP: the first 8 bytes of sha256.
// Truncated on purpose: grouping, not brute-forcing the IP back.
func ipHash(ip string) [8]byte {
	h := sha256.Sum256([]byte(ip))
	var out [8]byte
	copy(out[:], h[:8])
	return out
}
