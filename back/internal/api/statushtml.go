// The crawler surfaces of the permanent status pages (plan part 4): the
// HTML door GET /status/{slug}, the directory GET /status, the sitemap
// GET /sitemap-status.xml and the OG image GET /public/status/{slug}/og.png.
// Everything here renders from the SAME shared assembly as the JSON door
// (writeAPI.publicStatusData), so the sentence a crawler reads is the one
// the JSON door serves; the parity is pinned by test. The React page reads
// the same response but prints the components, not the sentence.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"go.upcontrol.io/back/internal/ogrender"
)

// statusPages is the HTML door handler: the write API it borrows carries
// the pool, the knobs and the shared assembly.
type statusPages struct {
	wa    *writeAPI
	tmpl  *template.Template
	stats *staticPages
}

// NewStatusPages parses the templates once (they are constants in this
// file; a parse error is a programming fault and fails loudly at wiring).
func NewStatusPages(wa *writeAPI) *statusPages {
	return &statusPages{wa: wa, tmpl: mustParseStatusTemplates(), stats: NewStaticPages()}
}

func (h *statusPages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/status":
		h.directory(w, r)
	case strings.HasPrefix(r.URL.Path, "/status/"):
		h.slugPage(w, r)
	case r.URL.Path == "/sitemap-status.xml":
		h.sitemap(w, r)
	case strings.HasPrefix(r.URL.Path, "/public/status/") && strings.HasSuffix(r.URL.Path, "/og.png"):
		h.ogImage(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// htmlHeaders are the door's response headers: the crawler gets its own
// copy per UA (Caddy routes by User-Agent), and a minute of public cache
// keeps a hot link cheap without serving stale states for long.
func htmlHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "User-Agent")
	w.Header().Set("Cache-Control", "public, max-age=60")
}

// pageLookup is the slug resolution every door here starts with: the page's
// holder, its claim state and the removal stamp. It is the same read
// publicStatus performs; it lives here so the HTML door can decide
// 410/301/404 before any assembly runs.
func (h *statusPages) pageLookup(ctx context.Context, slug string) (tenantID, projectID int64, claimed bool, removedAt *time.Time, ok bool) {
	err := h.wa.pool.Raw().QueryRow(ctx,
		`SELECT sp.tenant_id, sp.project_id, (t.claim_token_hash IS NULL), sp.removed_at
		   FROM status_page sp JOIN tenant t ON t.id = sp.tenant_id
		  WHERE sp.slug = $1`, slug).Scan(&tenantID, &projectID, &claimed, &removedAt)
	return tenantID, projectID, claimed, removedAt, err == nil
}

// aliasCanonical folds a www-shaped slug to the host page's canonical slug:
// the write API's own fold (canonicalAliasSlug), so the HTML door and the
// JSON door can never drift.
func (h *statusPages) aliasCanonical(ctx context.Context, slug string) string {
	return h.wa.canonicalAliasSlug(ctx, slug)
}

// componentLine is one row of the components list: the name, the measured
// 24 h uptime, and the text summary of the bars. The uptime's JSON "no
// data" rendering (an em-dash) is translated here: this page's visible copy
// carries none.
type componentLine struct {
	Name    string
	Uptime  string
	Summary string
}

type incidentLine struct {
	Title string
	Since string
	State string
}

// slugPageData is everything the slug template renders.
type slugPageData struct {
	Title      string
	Robots     string
	Canonical  string
	OGTitle    string
	OGDesc     string
	OGURL      string
	OGImage    string
	JSONLD     template.JS
	Host       string
	Slug       string
	HasState   bool
	Sentence   string
	AsOf       string
	Components []componentLine
	Incidents  []incidentLine
	CheckHref  string
	ClaimHref  string
}

// noDataUptime is what the HTML door prints where the JSON answer carries
// its em-dash marker: plain words, no glyph the page's copy bans.
const noDataUptime = "no data yet"

func (h *statusPages) slugPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	if slug == "" {
		slug = pathLast(r.URL.Path)
	}
	tenantID, projectID, claimed, removedAt, ok := h.pageLookup(ctx, slug)
	if !ok {
		// The alias fold first (one query, only on a miss): a www-shaped slug
		// is the same page, forever.
		if canon := h.aliasCanonical(ctx, slug); canon != "" && canon != slug {
			w.Header().Set("Location", "/status/"+canon)
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		// The prj-N fallback: a page not yet configured resolves by its
		// project, exactly like publicStatus.
		if _, perr := fmt.Sscanf(slug, "prj-%d", &projectID); perr != nil || projectID == 0 {
			h.stats.notFound(w)
			return
		}
		if qerr := h.wa.pool.Raw().QueryRow(ctx,
			`SELECT p.tenant_id, (t.claim_token_hash IS NULL),
			       (SELECT sp.removed_at FROM status_page sp WHERE sp.project_id = p.id LIMIT 1)
			   FROM project p JOIN tenant t ON t.id = p.tenant_id
			  WHERE p.id = $1`, projectID).Scan(&tenantID, &claimed, &removedAt); qerr != nil || tenantID == 0 {
			h.stats.notFound(w)
			return
		}
	}
	if removedAt != nil {
		h.stats.removed(w)
		return
	}
	resp, meta := h.wa.publicStatusData(ctx, projectID, claimed)

	// The robots meta mirrors the JSON door's indexable exactly: a stamp
	// plus the switch off means index; an unstamped host page is qualified
	// but quiet; everything else (prj-N, suffixed) is out entirely.
	indexable := meta.indexedAt != nil && !h.wa.statusKnobs.IndexDisabled
	robots := "noindex, nofollow"
	if meta.isHostPage {
		if indexable {
			robots = "index, follow"
		} else {
			robots = "noindex, follow"
		}
	}

	origin := h.wa.statusKnobs.StatusOrigin
	host := meta.host
	if host == "" {
		if t, _ := resp["title"].(string); t != "" {
			host = t
		} else {
			host = "this site"
		}
	}
	data := slugPageData{
		Title:     host + " status: is " + host + " answering right now?",
		Robots:    robots,
		OGTitle:   host + " status",
		OGDesc:    "The status of " + host + ", measured by UpControl.",
		OGURL:     origin + "/status/" + meta.slug,
		OGImage:   origin + "/public/status/" + meta.slug + "/og.png",
		Host:      host,
		Slug:      meta.slug,
		CheckHref: "/?check=" + template.URLQueryEscaper(host),
		ClaimHref: "/status/" + meta.slug + "#claim",
	}
	// A canonical link belongs to host pages only (plan part 4): a suffixed
	// page with noindex and a canonical would drag its noindex onto the host.
	if meta.isHostPage {
		data.Canonical = data.OGURL
	}
	if state, ok := resp["state"].(map[string]any); ok {
		data.HasState = true
		if s, ok := state["sentence"].(string); ok {
			data.Sentence = s
			data.OGDesc = s
		}
		if asOf, ok := state["asOf"].(string); ok {
			data.AsOf = asOf
		}
	}
	if comps, ok := resp["components"].([]map[string]any); ok {
		for _, c := range comps {
			name, _ := c["name"].(string)
			uptime, _ := c["uptime"].(string)
			if uptime == "—" || uptime == "" {
				uptime = noDataUptime
			}
			okBars, total := 0, 0
			if bars, ok := c["bars"].([]string); ok {
				total = len(bars)
				for _, b := range bars {
					if b == "ok" {
						okBars++
					}
				}
			}
			data.Components = append(data.Components, componentLine{
				Name: name, Uptime: uptime,
				Summary: fmt.Sprintf("ok in %d of %d bars", okBars, total),
			})
		}
	}
	if incs, ok := resp["incidents"].([]map[string]any); ok {
		for _, i := range incs {
			title, _ := i["title"].(string)
			since, _ := i["since"].(string)
			state := "resolved"
			if ongoing, _ := i["ongoing"].(bool); ongoing {
				state = "ongoing"
			}
			data.Incidents = append(data.Incidents, incidentLine{Title: title, Since: since, State: state})
		}
	}
	ld, _ := json.Marshal(map[string]any{
		"@context":    "https://schema.org",
		"@type":       "WebPage",
		"name":        data.Title,
		"url":         data.OGURL,
		"description": data.OGDesc,
	})
	data.JSONLD = template.JS(ld)

	htmlHeaders(w)
	if err := h.tmpl.ExecuteTemplate(w, "slug", data); err != nil {
		return // the headers are sent; a half page is the honest failure
	}
}

// directoryRow is one listed page: the host links to the page, the sentence
// is the same measured line the page itself prints.
type directoryRow struct {
	Host     string
	Slug     string
	Sentence string
}

// directoryData is everything the directory template renders.
type directoryData struct {
	Rows   []directoryRow
	JSONLD template.JS
}

// indexedList reads the sitemap's predicate: pages with a stamp, live, the
// index gate's own ordering (newest first). The kill switch empties it.
func (h *statusPages) indexedList(ctx context.Context) []directoryRow {
	if h.wa.statusKnobs.IndexDisabled {
		return nil
	}
	rows, err := h.wa.pool.Raw().Query(ctx,
		`SELECT sp.slug, p.domain, sp.root_target_id
		   FROM status_page sp JOIN project p ON p.id = sp.project_id
		  WHERE sp.indexed_at IS NOT NULL AND sp.removed_at IS NULL
		  ORDER BY sp.indexed_at DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []directoryRow
	for rows.Next() {
		var row directoryRow
		var root *int64
		if rows.Scan(&row.Slug, &row.Host, &root) != nil {
			continue
		}
		if root != nil && row.Host != "" {
			if state, ok := h.wa.hostPageState(ctx, row.Host, *root); ok {
				if s, ok := state["sentence"].(string); ok {
					row.Sentence = s
				}
			}
		}
		out = append(out, row)
	}
	return out
}

func (h *statusPages) directory(w http.ResponseWriter, r *http.Request) {
	rows := h.indexedList(r.Context())
	data := directoryData{Rows: rows}
	// ItemList structured data for the rows actually shown; an empty
	// directory has nothing to list and gets none.
	if len(rows) > 0 {
		origin := strings.TrimRight(h.wa.statusKnobs.StatusOrigin, "/")
		items := make([]map[string]any, 0, len(rows))
		for i, row := range rows {
			items = append(items, map[string]any{
				"@type":    "ListItem",
				"position": i + 1,
				"url":      origin + "/status/" + row.Slug,
				"name":     row.Host,
			})
		}
		ld, _ := json.Marshal(map[string]any{
			"@context":        "https://schema.org",
			"@type":           "ItemList",
			"itemListElement": items,
		})
		data.JSONLD = template.JS(ld)
	}
	htmlHeaders(w)
	_ = h.tmpl.ExecuteTemplate(w, "directory", data)
}

// sitemap mirrors the directory predicate exactly, one URL per indexed
// page, lastmod the day of the root target's newest check.
func (h *statusPages) sitemap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	if !h.wa.statusKnobs.IndexDisabled {
		rows, err := h.wa.pool.Raw().Query(r.Context(),
			`SELECT sp.slug, (SELECT max(ts) FROM checks c WHERE c.target_id = sp.root_target_id)
			   FROM status_page sp
			  WHERE sp.indexed_at IS NOT NULL AND sp.removed_at IS NULL
			  ORDER BY sp.indexed_at DESC`)
		if err == nil {
			defer rows.Close()
			origin := strings.TrimRight(h.wa.statusKnobs.StatusOrigin, "/")
			for rows.Next() {
				var slug string
				var last *time.Time
				if rows.Scan(&slug, &last) != nil {
					continue
				}
				b.WriteString("  <url>\n    <loc>" + origin + "/status/" + template.HTMLEscapeString(slug) + "</loc>\n")
				if last != nil {
					b.WriteString("    <lastmod>" + last.UTC().Format("2006-01-02") + "</lastmod>\n")
				}
				b.WriteString("  </url>\n")
			}
		}
	}
	b.WriteString(`</urlset>`)
	_, _ = w.Write([]byte(b.String()))
}

// ogImage serves the page's Open Graph picture from the same shared
// assembly the HTML door renders (plan part 4): the drawer lives in
// internal/ogrender, the data is publicStatusData's.
func (h *statusPages) ogImage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/public/status/"), "/og.png")
	_, projectID, claimed, removedAt, ok := h.pageLookup(ctx, slug)
	if !ok {
		writeAPIErr(w, http.StatusNotFound, "no_such_page")
		return
	}
	if removedAt != nil {
		writeAPIErr(w, http.StatusGone, "page_removed")
		return
	}
	resp, meta := h.wa.publicStatusData(ctx, projectID, claimed)

	page := ogrender.Page{Host: meta.host}
	if state, ok := resp["state"].(map[string]any); ok {
		if s, ok := state["sentence"].(string); ok {
			page.StateSentence = s
		}
	}
	if page.StateSentence == "" {
		page.StateSentence = "The status of " + meta.host + ", measured by UpControl."
	}
	// "checked N min ago" from the root target's newest check: the assembly
	// formats asOf as a clock string, and a relative line needs the raw time.
	if meta.rootTargetID != 0 {
		var last *time.Time
		if err := h.wa.pool.Raw().QueryRow(ctx,
			`SELECT max(ts) FROM checks WHERE target_id = $1`, meta.rootTargetID).Scan(&last); err == nil && last != nil {
			mins := int(time.Since(*last).Minutes())
			if mins < 1 {
				mins = 1
			}
			page.CheckedLine = fmt.Sprintf("checked %d min ago", mins)
		}
	}
	if comps, ok := resp["components"].([]map[string]any); ok {
		for _, c := range comps {
			if len(page.Components) == 4 {
				break
			}
			comp := ogrender.Component{}
			comp.Name, _ = c["name"].(string)
			if bars, ok := c["bars"].([]string); ok {
				comp.Bars = bars
			}
			page.Components = append(page.Components, comp)
		}
	}
	png, err := ogrender.Render(page)
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "internal")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_, _ = w.Write(png)
}

// The templates: parsed once from these constants (the house style keeps
// ucapi free of an asset pipeline). The styling is a minimal achromatic
// sheet; the markup is semantic so the page reads without CSS at all.

const slugTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="{{.Robots}}">
{{- if .Canonical}}
<link rel="canonical" href="{{.Canonical}}">
{{- end}}
<title>{{.Title}}</title>
<meta property="og:title" content="{{.OGTitle}}">
<meta property="og:description" content="{{.OGDesc}}">
<meta property="og:url" content="{{.OGURL}}">
<meta property="og:image" content="{{.OGImage}}">
<meta name="twitter:card" content="summary_large_image">
<script type="application/ld+json">{{.JSONLD}}</script>
<style>
:root { color-scheme: light; }
* { box-sizing: border-box; }
body { margin: 0; font: 16px/1.6 system-ui, sans-serif; color: #111; background: #fafafa; }
main, footer { max-width: 720px; margin: 0 auto; padding: 0 20px; }
.banner { padding: 28px 0 20px; border-bottom: 1px solid #e5e5e5; }
.banner h1 { font-size: 22px; margin: 0 0 6px; }
.banner p { margin: 0 0 10px; color: #444; }
a { color: #111; }
h2 { font-size: 19px; margin: 28px 0 8px; }
.state { font-size: 17px; margin: 4px 0 8px; }
.asof { color: #666; font-size: 14px; margin: 0; }
.components { list-style: none; margin: 8px 0; padding: 0; }
.components li { padding: 10px 0; border-bottom: 1px solid #eee; }
.components .name { font-weight: 600; }
.components .meta { color: #555; font-size: 14px; }
.incidents { list-style: none; margin: 8px 0; padding: 0; }
.incidents li { padding: 8px 0; border-bottom: 1px solid #eee; }
.incidents .ongoing { font-weight: 600; }
footer { padding: 24px 20px 40px; color: #444; font-size: 14px; }
footer nav a { margin-right: 14px; }
</style>
</head>
<body>
<main>
<section class="banner" id="claim">
<h1>Powered by UpControl</h1>
<p>Claim it to customize it and put it on your own domain.</p>
<a href="/">Claim this page</a>
</section>
<h2>{{.Host}} status</h2>
{{- if .HasState}}
<p class="state">{{.Sentence}}</p>
{{- end}}
{{- if .Components}}
<ul class="components">
{{- range .Components}}
<li><span class="name">{{.Name}}</span><br>
<span class="meta">24h uptime: {{.Uptime}}. {{.Summary}}.</span></li>
{{- end}}
</ul>
{{- end}}
{{- if .Incidents}}
<h2>Recent incidents</h2>
<ul class="incidents">
{{- range .Incidents}}
<li>{{.Title}}, since {{.Since}} <span class="{{.State}}">({{.State}})</span></li>
{{- end}}
</ul>
{{- end}}
{{- if .HasState}}
<h3>Is {{.Host}} answering right now?</h3>
<p class="state">{{.Sentence}}</p>
<p class="asof">as of {{.AsOf}}</p>
{{- end}}
</main>
<footer>
<p>Measured from one location outside {{.Host}} by UpControl.</p>
<nav>
<a href="{{.ClaimHref}}">Site owner? Claim this page.</a>
<a href="/status/policy">Want it removed? See the policy.</a>
<a href="/bot">About our bot</a>
<a href="/status">Status directory</a>
<a href="{{.CheckHref}}">Check your own site</a>
</nav>
</footer>
</body>
</html>`

const directoryTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, follow">
<title>Status pages directory · UpControl</title>
<meta name="description" content="Every public status page UpControl measures, in one index.">
{{- if .JSONLD}}
<script type="application/ld+json">{{.JSONLD}}</script>
{{- end}}
<style>
:root {
color-scheme: dark;
--bg: #16181b; --line: #2a2f35;
--ink: #e7e9eb; --ink-body: #c2c7cd; --ink-muted: #878e97;
--accent: #8fa9d8;
}
@media (prefers-color-scheme: light) {
:root {
color-scheme: light;
--bg: #f2f2ef; --line: #deded7;
--ink: #191b1d; --ink-body: #3a3e42; --ink-muted: #6b7178;
--accent: #3e5c96;
}
}
* { box-sizing: border-box; }
body { margin: 0; font: 16px/1.6 -apple-system, system-ui, sans-serif; color: var(--ink-body); background: var(--bg); }
header, main, footer { max-width: 640px; margin: 0 auto; padding-left: 20px; padding-right: 20px; }
header { display: flex; align-items: baseline; gap: 10px; padding-top: 20px; padding-bottom: 18px; }
.brand { font-weight: 700; font-size: 15px; letter-spacing: -0.01em; color: var(--ink); text-decoration: none; }
.tag { color: var(--ink-muted); font-size: 14px; }
main { padding-top: 8px; padding-bottom: 40px; }
h1 { font-size: 22px; margin: 4px 0 20px; color: var(--ink); }
ul { list-style: none; margin: 0; padding: 0; border-top: 1px solid var(--line); }
li { padding: 14px 0; border-bottom: 1px solid var(--line); }
li a { color: var(--ink); font-weight: 600; text-decoration: none; }
li a:hover { color: var(--accent); }
.state { display: block; margin-top: 2px; color: var(--ink-muted); font-size: 14px; }
.empty { color: var(--ink-muted); }
footer { margin-top: 20px; padding-top: 24px; padding-bottom: 40px; border-top: 1px solid var(--line); }
footer nav { display: flex; flex-wrap: wrap; gap: 4px 16px; }
footer a { color: var(--ink-muted); font-size: 14px; text-decoration: none; }
footer a:hover { color: var(--ink); }
footer p { margin: 16px 0 0; color: var(--ink-muted); font-size: 13px; }
</style>
</head>
<body>
<header>
<a class="brand" href="/">UpControl</a>
<span class="tag">status directory</span>
</header>
<main>
<h1>Status pages</h1>
{{- if .Rows}}
<ul>
{{- range .Rows}}
<li><a href="/status/{{.Slug}}">{{.Host}}</a>
<span class="state">{{.Sentence}}</span></li>
{{- end}}
</ul>
{{- else}}
<p class="empty">The directory is temporarily empty.</p>
{{- end}}
</main>
<footer>
<nav>
<a href="/">Home</a>
<a href="/docs">Docs</a>
<a href="/pricing">Pricing</a>
<a href="/privacy">Privacy</a>
<a href="/terms">Terms</a>
</nav>
<p>UpControl is operated by an independent sole trader based in Finland.</p>
</footer>
</body>
</html>`

// mustParseStatusTemplates parses the door's template set; the inputs are
// constants in this file, so a failure is a build fault, not a runtime one.
func mustParseStatusTemplates() *template.Template {
	t := template.New("status")
	for name, body := range map[string]string{"slug": slugTmpl, "directory": directoryTmpl} {
		if _, err := t.New(name).Parse(body); err != nil {
			panic("statushtml: parse " + name + " template: " + err.Error())
		}
	}
	return t
}
