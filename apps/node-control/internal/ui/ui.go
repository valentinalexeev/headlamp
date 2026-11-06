// Package ui holds the shared console assets: one stylesheet, the script (window.hl), the shell and theme scripts, the
// fonts and the icon set. Every admin page links them under /_ui/ and uses PageCSP.
// The package imports nothing from the rest of the app, so any package may import it.
package ui

import (
	"embed"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// PageCSP is the Content-Security-Policy of every admin page: same-origin script, style, font
// and fetch only, no inline script or style, no framing.
const PageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

//go:embed static/hl.css
var hlCSS []byte

//go:embed static/hl.js
var hlJS []byte

//go:embed static/theme.js
var themeJS []byte

//go:embed static/shell.js
var shellJS []byte

//go:embed static/activity-text.js
var activityTextJS []byte

//go:embed static/palette.js
var paletteJS []byte

//go:embed static/fonts
var fontFS embed.FS

const prefix = "/_ui/"

// Handler serves the assets under /_ui/. Mount it at "/_ui/". Only GET and HEAD, only files
// that exist, never a directory listing.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, ctype := lookup(r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ctype)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body) // a failed write is a client that left: nothing is left to tell it
		}
	})
}

func lookup(p string) ([]byte, string) {
	if !strings.HasPrefix(p, prefix) {
		return nil, ""
	}
	name := p[len(prefix):]
	if name == "" || path.Clean(name) != name {
		return nil, ""
	}
	switch name {
	case "hl.css":
		return hlCSS, "text/css; charset=utf-8"
	case "hl.js":
		return hlJS, "text/javascript; charset=utf-8"
	case "theme.js":
		return themeJS, "text/javascript; charset=utf-8"
	case "shell.js":
		return shellJS, "text/javascript; charset=utf-8"
	case "activity-text.js":
		return activityTextJS, "text/javascript; charset=utf-8"
	case "palette.js":
		return paletteJS, "text/javascript; charset=utf-8"
	}
	file, ok := strings.CutPrefix(name, "fonts/")
	if !ok || strings.Contains(file, "/") {
		return nil, ""
	}
	var ctype string
	switch path.Ext(file) {
	case ".woff2":
		ctype = "font/woff2"
	case ".txt":
		ctype = "text/plain; charset=utf-8"
	default:
		return nil, ""
	}
	b, err := fontFS.ReadFile("static/fonts/" + file)
	if err != nil {
		return nil, ""
	}
	return b, ctype
}
