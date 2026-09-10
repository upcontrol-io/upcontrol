// The two static pages every public status page links to (plan part 4):
// /bot (what our probe does to your site) and /status/policy (why these
// pages exist, how to claim, how to remove or object). Plain HTML from
// constants, no data, no session: honest text, not marketing.

package api

import (
	"html/template"
	"net/http"

	"go.upcontrol.io/back/internal/probe/executor"
)

type staticPages struct {
	tmpl *template.Template
}

// NewStaticPages parses the template set once; the inputs are constants,
// so a parse failure is a programming fault.
func NewStaticPages() *staticPages {
	t := template.New("static")
	for name, body := range map[string]string{
		"bot": botTmpl, "policy": policyTmpl, "gone": goneTmpl, "notfound": notFoundTmpl,
	} {
		if _, err := t.New(name).Parse(body); err != nil {
			panic("staticpages: parse " + name + " template: " + err.Error())
		}
	}
	return &staticPages{tmpl: t}
}

func (h *staticPages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	htmlHeaders(w)
	switch r.URL.Path {
	case "/bot":
		// The User-Agent string prints verbatim from the executor's
		// constant; it is never retyped here. Marked safe because it is our
		// own compile-time constant: html/template would otherwise escape the
		// "+" to "&#43;" and the page's source would stop being verbatim.
		_ = h.tmpl.ExecuteTemplate(w, "bot", template.HTML(executor.UserAgent))
	case "/status/policy":
		_ = h.tmpl.ExecuteTemplate(w, "policy", nil)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// removed answers 410: the page was here, its host asked for it to be gone,
// and the link a visitor still holds should say so.
func (h *staticPages) removed(w http.ResponseWriter) {
	htmlHeaders(w)
	w.WriteHeader(http.StatusGone)
	_ = h.tmpl.ExecuteTemplate(w, "gone", nil)
}

// notFound answers 404 in the same dress.
func (h *staticPages) notFound(w http.ResponseWriter) {
	htmlHeaders(w)
	w.WriteHeader(http.StatusNotFound)
	_ = h.tmpl.ExecuteTemplate(w, "notfound", nil)
}

// The static page templates: each is a full document with the same style
// sheet inline (baseCSS); only the <title> and the body differ.

// baseCSS is the one style sheet all four static pages inline: the same
// achromatic dress for /bot, the policy, 410 and 404.
const baseCSS = `body { margin: 0; font: 16px/1.6 system-ui, sans-serif; color: #111; background: #fafafa; }
main { max-width: 680px; margin: 0 auto; padding: 28px 20px 48px; }
h1 { font-size: 22px; }
h2 { font-size: 17px; margin-top: 28px; }
a { color: #111; }
code { font-family: ui-monospace, monospace; font-size: 14px; background: #f0f0f0; padding: 1px 5px; }
nav a { margin-right: 14px; }
`

// botTmpl: who we are, what we fetch, how often, how to opt out.
const botTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex, follow">
<title>About the UpControl bot</title>
<style>
` + baseCSS + `</style>
</head>
<body>
<main>
<h1>About the UpControl bot</h1>
<p>UpControl is an uptime monitoring service. To measure whether a site is
answering, our probe fetches pages from one location outside the site.</p>

<h2>What we fetch</h2>
<p>One GET request. We send no cookies and run no JavaScript. We store the
HTTP status code, the response time and the TLS certificate expiry. We never
store page content.</p>
<p>Our User-Agent string is exactly:</p>
<p><code>{{.}}</code></p>

<h2>How often we check</h2>
<p>Customer checks run at least every 5 minutes. Free public status pages are
checked every 5 to 15 minutes, and hourly for pages nobody has looked at in
over a month.</p>

<h2>Opting out</h2>
<p>If your site answers our checks with HTTP 403 or 429, we automatically
slow our checks down exponentially. To have a public page removed and your
host never monitored again, see the <a href="/status/policy">removal
policy</a>.</p>

<nav style="margin-top:32px">
<a href="/status">Status directory</a>
<a href="/status/policy">Policy</a>
</nav>
</main>
</body>
</html>`

// policyTmpl: why the pages exist, what is published, claim, removal,
// objection. The removal record name matches the worker's dns-tokens job.
const policyTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex, follow">
<title>Status page policy</title>
<style>
` + baseCSS + `</style>
</head>
<body>
<main>
<h1>Status page policy</h1>

<h2>Why these pages exist</h2>
<p>Anyone can check any publicly reachable site with UpControl. When someone
does, we create a public status page for that site. The page shows what our
probe measured and nothing else. The site's owner can claim the page and make
it their own.</p>

<h2>What is measured and published</h2>
<p>HTTP status, response time, TLS certificate expiry and the uptime derived
from them. All measurements come from one location. We publish no page
content and no personal data.</p>
<p>Until the owner claims a page and verifies control of the host, the page
is not affiliated with the site. It says so on its face, and it is kept out
of search engines.</p>

<h2>Claiming a page</h2>
<p>Use the Claim button on the page. To also list the page in search
engines, publish a DNS TXT record proving you control the host; the page
walks you through the record.</p>

<h2>Removing a page</h2>
<p>Removal is self-serve. The Remove button on the page issues a token.
Publish a DNS TXT record at <code>_upcontrol-remove.&lt;your registrable
domain&gt;</code> with that token as its value. Once we see the record, the
page is removed automatically, and the whole host is never monitored again.</p>
<p>If you cannot edit DNS, write to <code>removal@upcontrol.io</code> from an
address on the domain, and the page is removed within one business day.</p>

<h2>Objecting</h2>
<p>Objections follow the removal path: an objection equals removal, on the
same terms and the same timeline.</p>

<nav style="margin-top:32px">
<a href="/status">Status directory</a>
<a href="/bot">About our bot</a>
</nav>
</main>
</body>
</html>`

const goneTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex, follow">
<title>Page removed</title>
<style>
` + baseCSS + `</style>
</head>
<body>
<main>
<h1>Page removed</h1>
<p>This page was removed at the site owner's request.</p>
<p><a href="/status/policy">See the policy</a>.</p>
</main>
</body>
</html>`

const notFoundTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex, follow">
<title>No such page</title>
<style>
` + baseCSS + `</style>
</head>
<body>
<main>
<h1>No such page</h1>
<p>There is no status page at this address.</p>
<p><a href="/status">Browse the directory</a>.</p>
</main>
</body>
</html>`

// TODO(ops): the removal fallback mailbox removal@upcontrol.io must exist
// and reach an operator before this policy ships publicly.
