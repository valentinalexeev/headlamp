package enroll

import (
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
)

// The app picker of the self-service pages: one import link per app the site offers (enroll.json
// "clients", else the whole catalog), for the platform of the browser. A web page cannot see which
// apps are installed, so each app also has the store link the site gives for that platform. Every
// link names its profile format, so one device can go into any of these apps, and into more than
// one.

type pickApp struct {
	ID    string
	Label string
	Href  template.URL // a custom-scheme import link, built here from quoted parts
	// Direct is the import link when Href is the app page: a way in without the banner, also
	// while the edge of the site does not proxy the app page yet.
	Direct  template.URL
	Icon    bool
	Install []pickInstall
}

type pickInstall struct {
	Label string
	URL   string
}

type pickFormat struct {
	Name string
	URL  string
}

type pickView struct {
	Email    string
	CSRF     string
	DeviceID int64
	Name     string
	// Enrolment is set only right after the device was made: the tailnet key shows this one
	// time.
	Enrolment *Enrolment
	Apps      []pickApp
	// Platform is the name of the detected platform, "" when unknown or when every app is shown.
	Platform   string
	Hidden     int // apps for other platforms, not shown
	All        bool
	Formats    []pickFormat
	ProfileTag string
}

var formatNames = []struct{ format, label string }{
	{FormatSingBox, "sing-box"}, {FormatClash, "Clash"}, {FormatStash, "Stash"},
	{FormatURIList, "share links (base64)"},
}

// renderPick shows the picker of a device. e is set only right after the device was made.
func (s *SelfWeb) renderPick(
	w http.ResponseWriter, r *http.Request, sub, csrf string, d Device, e *Enrolment, name string,
) {
	u, err := s.self.users.User(sub)
	if err != nil {
		s.fail(w, err)
		return
	}
	secret, err := s.self.store.SubscriptionSecret(d.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	link := s.link()
	subURL := SubURL(link, secret)
	all := r.URL.Query().Get("all") == "1"
	platform := PlatformOf(r.Header.Get("User-Agent"))
	if all {
		platform = ""
	}
	v := pickView{Email: u.Email, CSRF: csrf, DeviceID: d.ID, Name: name, Enrolment: e, All: all,
		Platform: PlatformLabel[platform], ProfileTag: link.ProfileName}
	for _, ch := range link.Offered() {
		def, ok := ClientByID(ch.ID)
		if !ok {
			continue
		}
		if !def.RunsOn(platform) {
			v.Hidden++
			continue
		}
		_, icon := s.icons[def.ID]
		app := pickApp{ID: def.ID, Label: def.Label, Icon: icon,
			Href: importHref(def, FormatURL(subURL, def.Format), link.ProfileName)}
		if platform == PlatformIOS && ch.StoreID() != "" {
			// On an iPhone, the app page checks with Safari's banner whether the app is there.
			app.Direct, app.Href = app.Href, appPageHref(d.ID, def.ID)
		}
		installs := ch.installLinks()
		for _, p := range platforms {
			if url, ok := installs[p]; ok && (platform == "" || p == platform) {
				label := "Get " + def.Label
				if platform == "" {
					label += " for " + PlatformLabel[p]
				}
				app.Install = append(app.Install, pickInstall{label, url})
			}
		}
		v.Apps = append(v.Apps, app)
	}
	for _, f := range formatNames {
		v.Formats = append(v.Formats, pickFormat{Name: f.label, URL: FormatURL(subURL, f.format)})
	}
	secHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := pickTmpl.Execute(w, v); err != nil {
		slog.Warn("app picker page failed", "error_type", errType(err))
	}
}

// importHref is the import link of an app as a URL the page template keeps: html/template only lets
// http(s) and mailto links through, and these links use the app's own scheme. Every part that
// comes from data (the subscription URL and the profile name) is percent-encoded by the catalog.
func importHref(def ClientDef, subURL, name string) template.URL {
	return template.URL(def.Link(subURL, name)) // #nosec G203 -- built from encoded parts only
}

// appPageHref is the path of the app page of a device: digits and a catalog id only.
func appPageHref(id int64, app string) template.URL {
	p := "/me/devices/" + strconv.FormatInt(id, 10) + "/app/" + app
	return template.URL(p) // #nosec G203 -- digits and a catalog id only
}

// pickPage answers GET /me/devices/{id}: the picker of an own, active device.
func (s *SelfWeb) pickPage(w http.ResponseWriter, r *http.Request) {
	sub, csrf, d, ok := s.ownDevice(w, r)
	if ok {
		s.renderPick(w, r, sub, csrf, d, nil, d.Name)
	}
}

// ownDevice finds the active device of the signed-in person named in the path. When there is
// none, it has already answered: a redirect without a session, else the devices page with a 404.
func (s *SelfWeb) ownDevice(w http.ResponseWriter, r *http.Request) (string, string, Device, bool) {
	sub, csrf, ok := s.session(r)
	if !ok {
		secHeaders(w)
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return "", "", Device{}, false
	}
	var d Device
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		d, err = s.self.store.GetDevice(id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.fail(w, err)
			return "", "", Device{}, false
		}
	}
	if err != nil || d.Owner != ownerOf(sub) || !s.self.active(d) {
		s.notFoundPage(w, sub, csrf, "There is no such active device of yours.")
		return "", "", Device{}, false
	}
	return sub, csrf, d, true
}

func (s *SelfWeb) notFoundPage(w http.ResponseWriter, sub, csrf, msg string) {
	v, err := s.view(sub, csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Error = msg
	s.render(w, http.StatusNotFound, v)
}

// appView is the app page of one device and one app (#271).
type appView struct {
	DeviceID   int64
	Name       string
	ID         string
	Label      string
	Icon       bool
	AppStoreID string
	Argument   string       // the import link, for the banner (the template escapes it as text)
	Href       template.URL // the same import link, for the button
	GetURL     string
}

// appPage answers GET /me/devices/{id}/app/{app}: Safari's Smart App Banner for the app. Safari
// shows "Open" when the app is installed and "Get" when it is not; "Open" gives the import link to
// the app. A web page cannot find out by itself which apps are installed.
func (s *SelfWeb) appPage(w http.ResponseWriter, r *http.Request) {
	sub, csrf, d, ok := s.ownDevice(w, r)
	if !ok {
		return
	}
	link := s.link()
	var ch ClientChoice
	for _, c := range link.Offered() {
		if c.ID == r.PathValue("app") {
			ch = c
		}
	}
	def, known := ClientByID(ch.ID)
	if !known || ch.StoreID() == "" {
		s.notFoundPage(w, sub, csrf, "This site does not offer that app for iPhone and iPad.")
		return
	}
	secret, err := s.self.store.SubscriptionSecret(d.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	feed := FormatURL(SubURL(link, secret), def.Format)
	imp := def.Link(feed, link.ProfileName)
	_, icon := s.icons[def.ID]
	v := appView{DeviceID: d.ID, Name: d.Name, ID: def.ID, Label: def.Label, Icon: icon,
		AppStoreID: ch.StoreID(), Argument: imp, GetURL: ch.installLinks()[PlatformIOS],
		Href: importHref(def, feed, link.ProfileName)}
	secHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := appTmpl.Execute(w, v); err != nil {
		slog.Warn("app page failed", "error_type", errType(err))
	}
}

// Icon is one app icon of the site.
type Icon struct {
	Type string
	Data []byte
}

const maxIconBytes = 64 << 10

var iconTypes = []struct{ ext, ctype string }{
	{".svg", "image/svg+xml"}, {".png", "image/png"}, {".webp", "image/webp"},
}

// LoadIcons reads the app icons of a site: <dir>/<app id>.svg, .png or .webp, at most 64 KiB each.
// Other files are ignored. The public repository has no app icons: they are the marks of their
// makers, so the site supplies them.
func LoadIcons(dir string) (map[string]Icon, error) {
	out := map[string]Icon{}
	if dir == "" {
		return out, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("ENROLL_CLIENT_ICON_DIR %s is not a readable directory", dir)
	}
	defer root.Close()
	for _, c := range Catalog {
		for _, t := range iconTypes {
			name := c.ID + t.ext
			st, err := root.Stat(name)
			if err != nil {
				continue
			}
			if st.Size() > maxIconBytes {
				return nil, fmt.Errorf("ENROLL_CLIENT_ICON_DIR: %s is larger than %d bytes",
					name, maxIconBytes)
			}
			b, err := root.ReadFile(name)
			if err != nil {
				return nil, fmt.Errorf("ENROLL_CLIENT_ICON_DIR: %s: %w", name, err)
			}
			out[c.ID] = Icon{Type: t.ctype, Data: b}
			break
		}
	}
	return out, nil
}

// icon answers GET /me/icons/{app}. An SVG is served with a sandbox policy, so a script in it never
// runs, even when the file is opened on its own.
func (s *SelfWeb) icon(w http.ResponseWriter, r *http.Request) {
	ic, ok := s.icons[r.PathValue("app")]
	if !ok {
		notFound(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ic.Type)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	_, _ = w.Write(ic.Data)
}
