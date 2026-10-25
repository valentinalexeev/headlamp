package enroll

import (
	"log/slog"
	"net/http"
	"strings"
)

// The public enrolment handler serves ONLY GET /e/{token} and GET /sub/{secret}. It is meant to run
// on a private address behind a reverse proxy, so it exposes nothing else: every other path or
// method answers an empty 404, byte-identical to an unknown or used token. It never logs request
// paths: they carry one-time tokens and subscription secrets.
//
// Clients whose app only opens from a tap (karing, clash) get a small HTML page with an "Open in ..."
// button instead of the 302; the others get the 302. Neither carries the device name or owner: the
// deep link label is the neutral profile_name.
//
// Link previews: messengers fetch a pasted URL to render a preview, which would consume a
// single-use /e/ link before the person taps it. Requests whose User-Agent contains (case-insensitive)
// one of PreviewUAMarkers get the same empty 404 and do NOT consume the token; /sub/ answers them
// the same way. An empty or missing User-Agent is a normal client.

// PreviewUAMarkers are case-insensitive substrings of link-preview and crawler User-Agents.
// iMessage's preview fetch identifies as facebookexternalhit/Twitterbot. "apple" is deliberately
// absent (real Safari).
var PreviewUAMarkers = []string{
	"facebookexternalhit", "twitterbot", "telegrambot", "whatsapp", "slackbot", "discordbot",
	"skypeuripreview", "linkedinbot", "embedly", "bot", "crawler", "spider", "preview",
	"linkexpanding", "google-", "vkshare", "viber",
}

func isPreview(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, m := range PreviewUAMarkers {
		if strings.Contains(ua, m) {
			return true
		}
	}
	return false
}

const landingTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Open in {client}</title>
<style>body{font:18px system-ui,sans-serif;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;text-align:center}
a{display:inline-block;margin-top:1.2em;padding:.9em 1.6em;border-radius:.7em;background:#0a66ff;color:#fff;text-decoration:none;font-weight:600}</style>
</head><body><main><p>Tap the button to add the profile in {client}.</p><a href="{href}">Open in {client}</a></main></body></html>
`

// pyEscape is Python's html.escape(s, quote=True).
var pyEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;")

// publicStore is what the public handler needs from the Store.
type publicStore interface {
	ConsumeToken(token string) (*Device, error)
	VerifySecret(secret string) (*Device, error)
	SubscriptionSecret(deviceID int64) (string, error)
}

// Public is the public enrolment handler.
type Public struct {
	store publicStore
	cfg   IngressConfig
}

// NewPublic returns the public handler over store.
func NewPublic(store *Store, cfg IngressConfig) *Public { return newPublic(store, cfg) }

func newPublic(store publicStore, cfg IngressConfig) *Public { return &Public{store: store, cfg: cfg} }

// Register routes everything on mux to the handler, which answers an empty 404 to all but the two
// routes.
func (p *Public) Register(mux *http.ServeMux) { mux.HandleFunc("/", p.ServeHTTP) }

func notFound(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }

// serverError is an empty 500 and a log line carrying the error class only.
func serverError(w http.ResponseWriter, err error) {
	slog.Warn("enrolment public request failed", "error_type", errType(err))
	w.WriteHeader(http.StatusInternalServerError)
}

func (p *Public) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		notFound(w)
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/e/"):
		p.enrol(w, r, path[len("/e/"):])
	case strings.HasPrefix(path, "/sub/"):
		p.subscription(w, r, path[len("/sub/"):])
	default:
		notFound(w)
	}
}

func (p *Public) enrol(w http.ResponseWriter, r *http.Request, token string) {
	if token == "" || strings.Contains(token, "/") || isPreview(r.Header.Get("User-Agent")) {
		notFound(w) // a preview must be turned away before redeeming
		return
	}
	d, err := p.store.ConsumeToken(token)
	if err != nil {
		serverError(w, err)
		return
	}
	if d == nil {
		notFound(w)
		return
	}
	secret, err := p.store.SubscriptionSecret(d.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	location, err := DeepLinkFor(d.Client, SubURL(p.cfg, secret), p.cfg.ProfileName)
	if err != nil {
		serverError(w, err)
		return
	}
	if d.Client == "karing" || d.Client == "clash" {
		label := map[string]string{"karing": "Karing", "clash": "Clash Mi"}[d.Client]
		body := strings.NewReplacer("{client}", pyEscape.Replace(label), "{href}", pyEscape.Replace(location)).Replace(landingTemplate)
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		w.Write([]byte(body))
		return
	}
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

func (p *Public) subscription(w http.ResponseWriter, r *http.Request, secret string) {
	if secret == "" || strings.Contains(secret, "/") || isPreview(r.Header.Get("User-Agent")) {
		notFound(w)
		return
	}
	d, err := p.store.VerifySecret(secret)
	if err != nil {
		serverError(w, err)
		return
	}
	if d == nil {
		notFound(w)
		return
	}
	var body []byte
	var ctype string
	if d.Client == "clash" {
		body, err = RenderClashProfile(p.cfg, *d)
		ctype = "text/yaml; charset=utf-8"
	} else {
		body, err = RenderProfile(p.cfg, *d)
		ctype = "application/json"
	}
	if err != nil {
		serverError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Profile-Update-Interval", "12")
	w.Write(body)
}
