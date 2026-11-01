package funnel

import (
	_ "embed"
	"net/http"

	"example.com/node-control/internal/ui"
)

//go:embed static/funnel.html
var pageHTML []byte

//go:embed static/funnel.js
var pageJS []byte

//go:embed static/config.html
var configHTML []byte

//go:embed static/config.js
var configJS []byte

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or style
// that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", ui.PageCSP)
		w.Write(body)
	}
}
