// Package web serves uc.js, the script behind the web door's script tag. The
// file is embedded at build time; another worker owns its contents.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed uc.js
var script []byte

// Script answers GET /uc.js. An hour of public caching: the file is
// content-addressed by release, so every visitor between deploys may share it.
func Script(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(script)
}
