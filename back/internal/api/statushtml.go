// The crawler surfaces of the permanent status pages (plan part 4): the
// HTML door GET /status/{slug}, the directory GET /status, the sitemap
// GET /sitemap-status.xml and the OG image GET /public/status/{slug}/og.png,
// plus the directory's JSON GET /public/status-directory, which the browser's
// /status page reads (the edge splits /status by User-Agent like a slug page).
// Everything here renders from the SAME shared assembly as the JSON door
// (writeAPI.publicStatusData), so the sentence a crawler reads is the one
// the JSON door serves; the parity is pinned by test. The React page reads
// the same response and prints the components and the same FAQ.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"go.upcontrol.io/back/internal/ogrender"
)

// faqItem is one question a host page answers, worded the way people search
// for somebody else's site ("is X down", "X not working", "down for everyone
// or just me") and answered from this page's own measurements, never from
// copy written for a scanner. Both the crawler's HTML and the React page
// print the same list, so a search engine and a reader see one text.
type faqItem struct {
	Q string `json:"q"`
	A string `json:"a"`
}

// brandOf is the name people type without the TLD: datrade.io and
// app.datrade.io both read Datrade. A host the suffix list cannot fold, or a
// punycoded label, stays as it is.
func brandOf(host string) string {
	reg, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	label, _, _ := strings.Cut(reg, ".")
	if label == "" || strings.HasPrefix(label, "xn--") {
		return host
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

// statusFAQ builds a host page's questions from its measured state, its
// components and its published incidents. claimed decides the alerts
// question: only a page nobody owns offers Get alerts to a stranger.
func statusFAQ(host string, state map[string]any, comps, incidents []map[string]any, claimed bool) []faqItem {
	kind, _ := state["kind"].(string)
	sentence, _ := state["sentence"].(string)
	asOf, _ := state["asOf"].(string)
	name := brandOf(host)
	var faq []faqItem
	add := func(q, a string) { faq = append(faq, faqItem{Q: q, A: a}) }

	switch kind {
	case "ok":
		add("Is "+name+" down right now?", "No. "+sentence)
		add("Why is "+host+" not working or not loading for me?",
			"Our check from outside reached "+host+" at "+asOf+" and it answered normally, so the site itself is up. "+
				"If it still fails for you, the cause is likely between you and the site: reload the page, try another "+
				"browser or network, turn off a VPN or ad blocker, or clear your DNS cache.")
		add("Is "+name+" down for everyone or just me?",
			"Not for everyone: "+host+" answered our check from outside at "+asOf+". If it fails for you, the problem is "+
				"likely on your side or your network's.")
	case "down":
		add("Is "+name+" down right now?", "Yes. "+sentence)
		add("Why is "+host+" not working?",
			"It fails from outside too, so the cause is on "+name+"'s side, not yours. "+sentence)
		add("Is "+name+" down for everyone or just me?",
			"Not just you: "+host+" fails our check from outside as well.")
	default: // could_not_measure, nodata: the sentence says why there is no verdict
		add("Is "+name+" down right now?", sentence)
	}

	var ongoing, last map[string]any
	for _, i := range incidents {
		if on, _ := i["ongoing"].(bool); on && ongoing == nil {
			ongoing = i
		}
		if last == nil {
			last = i
		}
	}
	incidentLine := func(i map[string]any) string {
		title, _ := i["title"].(string)
		since, _ := i["since"].(string)
		return title + ", since " + since
	}
	switch {
	case ongoing != nil:
		add("Is there a "+name+" outage?", "Yes, one is ongoing: "+incidentLine(ongoing)+".")
	case kind == "down":
		add("Is there a "+name+" outage?", "Our latest check failed and the outage is being confirmed. "+sentence)
	case last != nil:
		add("Is there a "+name+" outage?", "Not now. The last one: "+incidentLine(last)+", resolved.")
	default:
		add("Is there a "+name+" outage?", "No outage has been recorded on "+host+" since we started checking it.")
	}

	if len(comps) > 0 {
		c := comps[0]
		uptime, _ := c["uptime"].(string)
		bars, _ := c["bars"].([]string)
		span, _ := c["barSpanSec"].(int)
		if uptime != "" && uptime != "—" && len(bars) > 0 {
			add("What is "+name+"'s uptime?",
				uptime+" over the last "+stripWindowLabel(time.Duration(span)*time.Second, len(bars))+
					", measured from outside by UpControl.")
		}
	}

	if !claimed {
		add("How do I get alerted when "+host+" goes down?",
			"Press Get alerts on this page. UpControl messages you the moment "+host+" stops answering our check.")
	}
	return faq
}

// statusPages is the HTML door handler: the write API it borrows carries
// the pool, the knobs and the shared assembly.
type statusPages struct {
	wa   *writeAPI
	tmpl *template.Template
}

// NewStatusPages parses the templates once (they are constants in this
// file; a parse error is a programming fault and fails loudly at wiring).
func NewStatusPages(wa *writeAPI) *statusPages {
	return &statusPages{wa: wa, tmpl: mustParseStatusTemplates()}
}

func (h *statusPages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/status":
		h.directory(w, r)
	case r.URL.Path == "/public/status-directory":
		h.directoryJSON(w, r)
	case strings.HasPrefix(r.URL.Path, "/status/"):
		h.slugPage(w, r)
	case r.URL.Path == "/hosted-status":
		h.hostedPage(w, r)
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

// componentLine is one row of the components list: the name, the uptime
// measured over the window its strip shows, and the text summary of the bars. The uptime's JSON "no
// data" rendering (an em-dash) is translated here: this page's visible copy
// carries none.
type componentLine struct {
	Name    string
	Uptime  string
	Window  string
	Summary string
}

type incidentLine struct {
	Title string
	Since string
	State string
}

// slugPageData is everything the slug template renders.
type slugPageData struct {
	Title       string
	Robots      string
	Description string
	Canonical   string
	OGTitle     string
	OGDesc      string
	OGURL       string
	OGImage     string
	JSONLD      template.JS
	Host        string
	Slug        string
	Claimed     bool
	Hosted      bool
	Origin      string
	HasState    bool
	Sentence    string
	Components  []componentLine
	Incidents   []incidentLine
	FAQ         []faqItem
	CheckHref   string
	ClaimHref   string
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
			notFoundPage(w)
			return
		}
		if qerr := h.wa.pool.Raw().QueryRow(ctx,
			`SELECT p.tenant_id, (t.claim_token_hash IS NULL),
			       (SELECT sp.removed_at FROM status_page sp WHERE sp.project_id = p.id LIMIT 1)
			   FROM project p JOIN tenant t ON t.id = p.tenant_id
			  WHERE p.id = $1`, projectID).Scan(&tenantID, &claimed, &removedAt); qerr != nil || tenantID == 0 {
			notFoundPage(w)
			return
		}
	}
	if removedAt != nil {
		removedPage(w)
		return
	}
	h.renderPage(w, ctx, projectID, claimed, "")
}

// hostedPage is the same page on a customer's own verified domain: the edge
// rewrites a crawler's request there to /hosted-status, and the Host header
// names the page, exactly as the JSON door's slug-less read resolves it.
func (h *statusPages) hostedPage(w http.ResponseWriter, r *http.Request) {
	projectID, claimed, removedAt, ok := h.wa.pageByHost(r.Context(), r.Host)
	if !ok {
		notFoundPage(w)
		return
	}
	if removedAt != nil {
		removedPage(w)
		return
	}
	h.renderPage(w, r.Context(), projectID, claimed, strings.ToLower(bareHost(r.Host)))
}

// renderPage writes one page's HTML. hosted is the customer's own host when
// the page is read there: the page is then theirs, worded plainly, with
// every address on their host and no doors of ours but the credit line.
func (h *statusPages) renderPage(w http.ResponseWriter, ctx context.Context, projectID int64, claimed bool, hosted string) {
	resp, meta := h.wa.publicStatusData(ctx, projectID, claimed)

	// The robots meta mirrors the JSON door's indexable exactly.
	robots := "noindex, nofollow"
	if meta.indexable {
		robots = "index, follow"
	}

	origin := strings.TrimRight(h.wa.statusKnobs.StatusOrigin, "/")
	host := meta.host
	if host == "" {
		if t, _ := resp["title"].(string); t != "" {
			host = t
		} else {
			host = "this site"
		}
	}
	name := brandOf(host)
	pageURL, assetOrigin := origin+"/status/"+meta.slug, origin
	if hosted != "" {
		pageURL, assetOrigin = "https://"+hosted+"/", "https://"+hosted
	}
	data := slugPageData{
		Title:  "Is " + name + " down? " + host + " status right now",
		Robots: robots,
		Description: "Is " + host + " down or not working? Live " + name +
			" status, outages and uptime, checked from outside by UpControl.",
		OGTitle:   host + " status",
		OGDesc:    "The status of " + host + ", measured by UpControl.",
		OGURL:     pageURL,
		OGImage:   assetOrigin + "/public/status/" + meta.slug + "/og.png",
		Host:      host,
		Slug:      meta.slug,
		Claimed:   claimed,
		Hosted:    hosted != "",
		Origin:    origin,
		CheckHref: "/?check=" + template.URLQueryEscaper(host),
		ClaimHref: "/status/" + meta.slug + "#claim",
	}
	if hosted != "" {
		data.Title = host + " status"
		data.Description = "Live status, uptime and recent incidents for " + host + ", measured from outside."
	}
	// A canonical link belongs to listed pages only (plan part 4): a
	// suffixed page with noindex and a canonical would drag its noindex onto
	// the host. A page with a verified domain of its own names THAT address
	// on both of its URLs: the plan buys the customer's address, and two
	// copies of one page must not split its ranking.
	if meta.indexable {
		data.Canonical = pageURL
		if meta.customURL != "" {
			data.Canonical = meta.customURL
		}
		data.OGURL = data.Canonical
	}
	if state, ok := resp["state"].(map[string]any); ok {
		data.HasState = true
		if s, ok := state["sentence"].(string); ok {
			data.Sentence = s
			data.OGDesc = s
			data.Description += " " + s
		}
	}
	data.FAQ, _ = resp["faq"].([]faqItem)
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
			span, _ := c["barSpanSec"].(int)
			data.Components = append(data.Components, componentLine{
				Name: name, Uptime: uptime, Window: stripWindowLabel(time.Duration(span)*time.Second, total),
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
	graph := []map[string]any{{
		"@type":       "WebPage",
		"name":        data.Title,
		"url":         data.OGURL,
		"description": data.OGDesc,
	}}
	if len(data.FAQ) > 0 {
		questions := make([]map[string]any, 0, len(data.FAQ))
		for _, f := range data.FAQ {
			questions = append(questions, map[string]any{
				"@type":          "Question",
				"name":           f.Q,
				"acceptedAnswer": map[string]any{"@type": "Answer", "text": f.A},
			})
		}
		graph = append(graph, map[string]any{"@type": "FAQPage", "mainEntity": questions})
	}
	ld, _ := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": graph})
	data.JSONLD = template.JS(ld)

	htmlHeaders(w)
	if err := h.tmpl.ExecuteTemplate(w, "slug", data); err != nil {
		return // the headers are sent; a half page is the honest failure
	}
}

// directoryRow is one listed page: the host links to the page, the sentence
// is the same measured line the page itself prints.
type directoryRow struct {
	Host     string `json:"host"`
	Slug     string `json:"slug"`
	Sentence string `json:"sentence,omitempty"`
}

// directoryData is everything the directory template renders.
type directoryData struct {
	Rows      []directoryRow
	Canonical string
	JSONLD    template.JS
}

// listedPagesSQL is writeAPI.indexable in SQL: the one predicate the
// directory and the sitemap share, so a listed page is always one whose own
// robots meta says index.
const listedPagesSQL = `sp.removed_at IS NULL AND (sp.is_host_page OR sp.root_target_id IS NULL)`

// indexedList reads the listed pages, newest first. The kill switch empties it.
// ponytail: one state read per row, uncached; page the list if it outgrows
// a few thousand pages.
func (h *statusPages) indexedList(ctx context.Context) []directoryRow {
	if h.wa.statusKnobs.IndexDisabled {
		return nil
	}
	rows, err := h.wa.pool.Raw().Query(ctx,
		`SELECT sp.slug, COALESCE(NULLIF(p.domain, ''), sp.title, sp.slug), sp.root_target_id
		   FROM status_page sp JOIN project p ON p.id = sp.project_id
		  WHERE `+listedPagesSQL+`
		  ORDER BY sp.created_at DESC`)
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
	origin := strings.TrimRight(h.wa.statusKnobs.StatusOrigin, "/")
	data := directoryData{Rows: rows, Canonical: origin + "/status"}
	// ItemList structured data for the rows actually shown; an empty
	// directory has nothing to list and gets none.
	if len(rows) > 0 {
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

// directoryJSON serves the same list to the browser's /status page. An empty
// directory is `[]`: the contract never sends null for "no value".
func (h *statusPages) directoryJSON(w http.ResponseWriter, r *http.Request) {
	rows := h.indexedList(r.Context())
	if rows == nil {
		rows = []directoryRow{}
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"pages": rows})
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
			  WHERE `+listedPagesSQL+`
			  ORDER BY sp.created_at DESC`)
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
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="{{.Robots}}">
{{- if .Canonical}}
<link rel="canonical" href="{{.Canonical}}">
{{- end}}
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
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
.components { list-style: none; margin: 8px 0; padding: 0; }
.components li { padding: 10px 0; border-bottom: 1px solid #eee; }
.components .name { font-weight: 600; }
.components .meta { color: #555; font-size: 14px; }
.incidents { list-style: none; margin: 8px 0; padding: 0; }
.incidents li { padding: 8px 0; border-bottom: 1px solid #eee; }
.incidents .ongoing { font-weight: 600; }
.faq h3 { font-size: 16px; margin: 18px 0 4px; }
.faq p { margin: 0; color: #333; }
footer { padding: 24px 20px 40px; color: #444; font-size: 14px; }
footer nav a { margin-right: 14px; }
</style>
</head>
<body>
<main>
<section class="banner" id="claim">
<h1>{{.Host}} status</h1>
{{- if not .Claimed}}
<p>Nobody is alerted when {{.Host}} goes down.</p>
<a href="/">Get alerts</a>
{{- end}}
</section>
{{- if .HasState}}
<p class="state">{{.Sentence}}</p>
{{- end}}
{{- if .Components}}
<ul class="components">
{{- range .Components}}
<li><span class="name">{{.Name}}</span><br>
<span class="meta">Uptime over {{.Window}}: {{.Uptime}}. {{.Summary}}.</span></li>
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
{{- if .FAQ}}
<section class="faq">
<h2>Questions about {{.Host}}</h2>
{{- range .FAQ}}
<h3>{{.Q}}</h3>
<p>{{.A}}</p>
{{- end}}
</section>
{{- end}}
</main>
<footer>
<p>Powered by <a href="{{.Origin}}/">UpControl</a></p>
<p>Measured from one location outside {{.Host}} by UpControl.</p>
{{- if not .Hosted}}
<nav>
<a href="{{.ClaimHref}}">Site owner? Claim this page.</a>
<a href="/status">Status directory</a>
<a href="{{.CheckHref}}">Check your own site</a>
</nav>
{{- end}}
</footer>
</body>
</html>`

const directoryTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="index, follow">
<link rel="canonical" href="{{.Canonical}}">
<title>Status pages directory | UpControl</title>
<meta name="description" content="Is a site down? Every public status page UpControl measures, with each site's live state, in one index.">
{{- if .JSONLD}}
<script type="application/ld+json">{{.JSONLD}}</script>
{{- end}}
<style>
body { margin: 0; font: 16px/1.6 system-ui, sans-serif; color: #111; background: #fafafa; }
main { max-width: 720px; margin: 0 auto; padding: 24px 20px 40px; }
ul { list-style: none; margin: 0; padding: 0; }
li { padding: 10px 0; border-bottom: 1px solid #eee; }
.state { color: #555; font-size: 14px; }
a { color: #111; }
</style>
</head>
<body>
<main>
<h1>Status pages</h1>
{{- if .Rows}}
<ul>
{{- range .Rows}}
<li><a href="/status/{{.Slug}}">{{.Host}}</a><br>
<span class="state">{{.Sentence}}</span></li>
{{- end}}
</ul>
{{- else}}
<p>The directory is temporarily empty.</p>
{{- end}}
</main>
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
