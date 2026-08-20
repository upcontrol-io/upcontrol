package analytics

import (
	"crypto/sha256"
	_ "embed"
	"net"

	"github.com/oschwald/geoip2-golang"
)

// The DBIP Country Lite database, committed to the repo (§Decision 3). It is
// rebuilt monthly by DBIP; the copy ships with the binary so a runtime
// download (and a runtime dependency on an external service) never exists.
// License: CC BY 4.0 — see geoip/README.md.
//
//go:embed geoip/dbip-country-lite.mmdb
var mmdbBytes []byte

// Geo resolves an IP to an ISO country code. A nil *Geo is valid and answers
// "" to everything: a corrupt or missing database degrades to "country
// unknown", it never takes the analytics path down.
type Geo struct {
	db *geoip2.Reader
}

// OpenGeo parses the embedded database.
func OpenGeo() (*Geo, error) {
	db, err := geoip2.FromBytes(mmdbBytes)
	if err != nil {
		return nil, err
	}
	return &Geo{db: db}, nil
}

// Country returns the ISO 3166-1 alpha-2 code for ip, or "" when unknown
// (unparseable IP, private range, database miss). The input is the raw IP
// string; the return value is the only thing kept from it.
func (g *Geo) Country(ip string) string {
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

// IPHash is the grouping key derived from a client IP: the first 8 bytes of
// sha256(ip). Truncated on purpose — enough to group a visitor's networks,
// not enough to brute-force the IP back out of the dashboard.
func IPHash(ip string) [8]byte {
	h := sha256.Sum256([]byte(ip))
	var out [8]byte
	copy(out[:], h[:8])
	return out
}
