package tasks

import (
	_ "embed"
	"net/http"
)

//go:embed static/tasks.html
var pageHTML []byte

//go:embed static/tasks.css
var pageCSS []byte

//go:embed static/tasks.js
var pageJS []byte

// pageCSP limits scripts to same-origin files. style-src allows inline styles only because the
// core's top bar is an inline style block inserted into admin pages; this page itself has none
// and puts no server text into markup or styles.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// asset serves an embedded file with headers that forbid framing, sniffing and caching. The page
// and its files never depend on the store, so they are served while the module is degraded.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", pageCSP)
		w.Write(body)
	}
}
