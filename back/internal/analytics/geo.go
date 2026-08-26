package analytics

import (
	"crypto/sha256"
	_ "embed"
	"net"

	"github.com/oschwald/geoip2-golang"
)

//go:embed geoip/dbip-country-lite.mmdb
var mmdbBytes []byte

// geo resolves an IP to an ISO country code. A nil *geo is valid and answers
// "": a missing database degrades to "country unknown".
type geo struct {
	db *geoip2.Reader
}

// openGeo parses the embedded database.
func openGeo() (*geo, error) {
	db, err := geoip2.FromBytes(mmdbBytes)
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
