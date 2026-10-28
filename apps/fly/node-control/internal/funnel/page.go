package funnel

import (
	_ "embed"
	"net/http"
)

//go:embed static/funnel.html
var pageHTML []byte

//go:embed static/funnel.js
var pageJS []byte

//go:embed static/funnel.css
var pageCSS []byte

// style-src allows inline styles only because the core's top bar is an inline style block inserted into admin
// pages; scripts stay limited to same-origin files, and the page puts no server text into HTML (textContent only).
const cspValue = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or style
// that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", cspValue)
		w.Write(body)
	}
}
