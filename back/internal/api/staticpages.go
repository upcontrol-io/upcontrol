// The crawler door's two fixed answers: 410 for a page its host had removed,
// 404 for a slug that never existed. Plain HTML from constants, no data, no
// session.

package api

import "net/http"

// removedPage answers 410: the page was here, its host asked for it to be
// gone, and the link a visitor still holds should say so.
func removedPage(w http.ResponseWriter) {
	htmlHeaders(w)
	w.WriteHeader(http.StatusGone)
	_, _ = w.Write([]byte(goneHTML))
}

// notFoundPage answers 404 in the same dress.
func notFoundPage(w http.ResponseWriter) {
	htmlHeaders(w)
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(notFoundHTML))
}

// baseCSS is the one style sheet both pages inline: the achromatic dress of
// the crawler surfaces.
const baseCSS = `body { margin: 0; font: 16px/1.6 system-ui, sans-serif; color: #111; background: #fafafa; }
main { max-width: 680px; margin: 0 auto; padding: 28px 20px 48px; }
h1 { font-size: 22px; }
a { color: #111; }
`

const goneHTML = `<!DOCTYPE html>
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
</main>
</body>
</html>`

const notFoundHTML = `<!DOCTYPE html>
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
