package tasks

import (
	_ "embed"
	"net/http"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

//go:embed static/tasks.html
var pageHTML []byte

//go:embed static/tasks.js
var pageJS []byte

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or style
// that is not from this origin. The page and its files never depend on the store, so they are served while
// the module is degraded.
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
