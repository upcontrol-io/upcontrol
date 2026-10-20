// Package web serves uc.js, the script behind the web door's script tag, and
// overlay.js, the heatmap overlay uc.js loads only for a heatmap link. Both
// files are embedded at build time; another worker owns their contents.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed uc.js
var script []byte

//go:embed overlay.js
var overlay []byte

// Script answers GET /uc.js. An hour of public caching: the file is
// content-addressed by release, so every visitor between deploys may share it.
func Script(w http.ResponseWriter, _ *http.Request) { serve(w, script) }

// Overlay answers GET /w/overlay.js, with Script's headers and caching.
func Overlay(w http.ResponseWriter, _ *http.Request) { serve(w, overlay) }

func serve(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}
