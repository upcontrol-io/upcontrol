package discover

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"
)

const (
	// A sitemap is somebody else's file and can name 50 000 URLs. We keep the
	// first sitemapMaxURLs and stop: everything past the ranking's top five is
	// read and thrown away, so reading more only spends memory.
	sitemapMaxURLs  = 500
	sitemapMaxBytes = 512 << 10
	// One nested level: index → child sitemaps. Deeper is legal and rare, and
	// each level multiplies requests against a host that never asked us here.
	sitemapMaxChildren = 2
)

// sitemapDoc parses both shapes with one struct: <urlset><url> and
// <sitemapindex><sitemap>. Both carry <loc>, which is the only field that
// decides what happens next.
type sitemapDoc struct {
	XMLName  xml.Name       `xml:"-"`
	URLs     []sitemapEntry `xml:"url"`
	Sitemaps []sitemapEntry `xml:"sitemap"`
}

type sitemapEntry struct {
	Loc      string `xml:"loc"`
	Priority string `xml:"priority"`
}

// candidate is a URL worth considering, with where it came from. Source ends up
// on the row, because "in sitemap.xml" and "linked from the homepage" are
// different levels of the site's own endorsement.
type candidate struct {
	URL      string
	Source   string
	Priority string
}

// fetchSitemapURLs returns the sitemap's URLs, or nil when the host has no
// sitemap we can read. Costs at most sitemapMaxChildren+1 requests.
func fetchSitemapURLs(ctx context.Context, p Prober, base string, r robots) []candidate {
	locations := r.sitemaps
	if len(locations) == 0 {
		// The convention, tried only when robots.txt named nothing.
		locations = []string{base + "/sitemap.xml"}
	}

	var out []candidate
	budget := sitemapMaxChildren + 1
	for _, loc := range locations {
		if budget <= 0 || ctx.Err() != nil {
			break
		}
		// A sitemap may name maps on other hosts. Following one would have us
		// probing a domain the visitor never typed.
		if !sameHost(base, loc) {
			continue
		}
		budget--
		doc := fetchSitemapDoc(ctx, p, loc)
		if doc == nil {
			continue
		}
		for _, u := range doc.URLs {
			out = append(out, candidate{URL: strings.TrimSpace(u.Loc), Source: "in sitemap.xml", Priority: u.Priority})
		}
		// Index: descend exactly one level, into at most the remaining budget.
		for _, child := range doc.Sitemaps {
			if budget <= 0 || ctx.Err() != nil {
				break
			}
			loc := strings.TrimSpace(child.Loc)
			if !sameHost(base, loc) {
				continue
			}
			budget--
			if childDoc := fetchSitemapDoc(ctx, p, loc); childDoc != nil {
				for _, u := range childDoc.URLs {
					out = append(out, candidate{URL: strings.TrimSpace(u.Loc), Source: "in sitemap.xml", Priority: u.Priority})
				}
			}
		}
		if len(out) >= sitemapMaxURLs {
			break
		}
	}
	if len(out) > sitemapMaxURLs {
		out = out[:sitemapMaxURLs]
	}
	return out
}

// fetchSitemapDoc gets and parses one sitemap. A .xml.gz map is NOT handled:
// Go's client transparently decodes Content-Encoding, but a gzipped *file* is
// content, and the decode belongs with the next round of this work.
func fetchSitemapDoc(ctx context.Context, p Prober, loc string) *sitemapDoc {
	res := p.Execute(ctx, CheckSpec{
		URL: loc, Method: http.MethodGet,
		TimeoutMs:    uint32(perRequestTimeout.Milliseconds()),
		MaxRedirects: 2,
		MaxBodyBytes: sitemapMaxBytes,
		CollectBody:  true,
	})
	if res.StatusCode < 200 || res.StatusCode >= 300 || len(res.Body) == 0 {
		return nil
	}
	var doc sitemapDoc
	if err := xml.Unmarshal(res.Body, &doc); err != nil {
		return nil
	}
	return &doc
}
