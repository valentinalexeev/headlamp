package enroll

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// The client catalog: the relay apps that node-control can make a profile and an import link for.
// The catalog is generic and part of the code. Which of these apps a site offers, in which order,
// and where people get them, is site configuration (enroll.json "clients"), because that depends on
// the app stores the site's people can reach.

// Profile formats, also the last path segment of a subscription URL (/sub/<secret>/<format>).
const (
	FormatSingBox = "sing-box" // sing-box JSON
	FormatClash   = "clash"    // Clash / mihomo (JSON, valid YAML)
	FormatStash   = "stash"    // Stash's dialect of the Clash format
	FormatURIList = "uri"      // base64 list of vless:// and hysteria2:// share links, one per line
)

// Platforms a client can run on, as detected from the browser.
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
	PlatformMacOS   = "macos"
	PlatformWindows = "windows"
	PlatformLinux   = "linux"
)

var platforms = []string{
	PlatformIOS, PlatformAndroid, PlatformMacOS, PlatformWindows, PlatformLinux,
}

// PlatformLabel is the name a person reads for a platform.
var PlatformLabel = map[string]string{
	PlatformIOS: "iPhone and iPad", PlatformAndroid: "Android", PlatformMacOS: "Mac",
	PlatformWindows: "Windows", PlatformLinux: "Linux",
}

// ClientDef is one app of the catalog.
type ClientDef struct {
	ID        string   // the stored device client value and the key in enroll.json
	Label     string   // the app's name, for buttons and the landing page
	Format    string   // the profile format the app imports
	Platforms []string // where the app runs
	// Landing: the one-time link answers with a page and a button instead of a redirect, because
	// the app only opens from a tap.
	Landing bool
	link    func(subURL, name string) string
}

// Link is the app's import link for a subscription URL and a label.
func (c ClientDef) Link(subURL, name string) string { return c.link(subURL, name) }

// RunsOn says whether the app runs on the platform. An unknown platform ("") matches every app.
func (c ClientDef) RunsOn(p string) bool { return p == "" || slices.Contains(c.Platforms, p) }

// installConfigLink is the "install-config" import link that several apps share.
func installConfigLink(scheme string) func(string, string) string {
	return func(subURL, name string) string {
		return scheme + "://install-config?url=" + quote(subURL) + "&name=" + quote(name)
	}
}

// Catalog is every app node-control supports, in the default order.
var Catalog = []ClientDef{
	{ID: "sing-box", Label: "sing-box", Format: FormatSingBox,
		Platforms: []string{PlatformIOS, PlatformMacOS, PlatformAndroid},
		link: func(subURL, name string) string {
			return "sing-box://import-remote-profile?url=" + quote(subURL) + "#" + quote(name)
		}},
	{ID: "karing", Label: "Karing", Format: FormatSingBox, Landing: true,
		Platforms: []string{
			PlatformIOS, PlatformMacOS, PlatformAndroid, PlatformWindows, PlatformLinux,
		},
		link: installConfigLink("karing")},
	{ID: "clash", Label: "Clash Mi", Format: FormatClash, Landing: true,
		Platforms: []string{PlatformIOS, PlatformMacOS, PlatformAndroid, PlatformWindows},
		link:      installConfigLink("clash")},
	{ID: "shadowrocket", Label: "Shadowrocket", Format: FormatURIList, Landing: true,
		Platforms: []string{PlatformIOS, PlatformMacOS},
		// The URL is standard base64, as Shadowrocket's own sub:// QR codes carry it.
		link: func(subURL, name string) string {
			b := base64.StdEncoding.EncodeToString([]byte(subURL))
			return "shadowrocket://add/sub://" + b + "?remark=" + quote(name)
		}},
	{ID: "stash", Label: "Stash", Format: FormatStash, Landing: true,
		Platforms: []string{PlatformIOS, PlatformMacOS},
		link:      installConfigLink("stash")},
}

// Clients are the accepted device client values: the catalog IDs.
var Clients = func() []string {
	out := make([]string, len(Catalog))
	for i, c := range Catalog {
		out[i] = c.ID
	}
	return out
}()

// clientsRepr is the list in the form the 422 detail always used (a Python tuple).
var clientsRepr = "('" + strings.Join(Clients, "', '") + "')"

// ClientByID finds an app of the catalog.
func ClientByID(id string) (ClientDef, bool) {
	for _, c := range Catalog {
		if c.ID == id {
			return c, true
		}
	}
	return ClientDef{}, false
}

// formatOf is the profile format a device's stored client gets at the plain subscription URL. A
// value outside the catalog gets sing-box, the format the plain URL always served to anything but
// Clash.
func formatOf(client string) string {
	if c, ok := ClientByID(client); ok {
		return c.Format
	}
	return FormatSingBox
}

func validFormat(f string) bool {
	return f == FormatSingBox || f == FormatClash || f == FormatStash || f == FormatURIList
}

// ClientChoice is one app a site offers on the picker page, with where people get it per platform.
type ClientChoice struct {
	ID string `json:"id"`
	// Install maps a platform to the https URL of the app's store page.
	Install map[string]string `json:"install,omitempty"`
}

// Offered are the apps of the picker page: the site's choices, or every app of the catalog.
func (c IngressConfig) Offered() []ClientChoice {
	if len(c.Clients) > 0 {
		return c.Clients
	}
	out := make([]ClientChoice, len(Catalog))
	for i, d := range Catalog {
		out[i] = ClientChoice{ID: d.ID}
	}
	return out
}

// parseClients reads enroll.json "clients": a list of {"id": "<catalog id>", "install":
// {"<platform>": "https://..."}}.
func parseClients(v json.RawMessage) ([]ClientChoice, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(v, &items); err != nil || items == nil {
		return nil, errors.New("ENROLL_CONFIG: clients must be an array of objects")
	}
	if len(items) == 0 {
		return nil, errors.New(
			"ENROLL_CONFIG: clients must name at least one app (leave it out to offer every app)")
	}
	seen := map[string]bool{}
	out := make([]ClientChoice, 0, len(items))
	for i, raw := range items {
		ch, err := parseClientChoice(i, raw)
		if err != nil {
			return nil, err
		}
		if seen[ch.ID] {
			return nil, fmt.Errorf("ENROLL_CONFIG: clients[%d]: %s is listed twice", i, ch.ID)
		}
		seen[ch.ID] = true
		out = append(out, ch)
	}
	return out, nil
}

// parseClientChoice reads one entry of enroll.json "clients".
func parseClientChoice(i int, raw map[string]json.RawMessage) (ClientChoice, error) {
	if raw == nil {
		return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d] must be an object", i)
	}
	for k := range raw {
		if k != "id" && k != "install" {
			if !keyNameRe.MatchString(k) {
				k = "(a key with an unusual name)"
			}
			return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d]: unknown key %q", i, k)
		}
	}
	id, _ := rawString(raw["id"])
	if _, ok := ClientByID(id); !ok {
		return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d]: id must be one of %s",
			i, clientsRepr)
	}
	ch := ClientChoice{ID: id}
	if inst, present := raw["install"]; present {
		m, err := parseInstall(inst)
		if err != nil {
			return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d] %s: %w", i, id, err)
		}
		ch.Install = m
	}
	return ch, nil
}

// maxStoreURL bounds a store link of enroll.json.
const maxStoreURL = 500

// parseInstall reads the "install" object of a clients entry: platform to https URL.
func parseInstall(v json.RawMessage) (map[string]string, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil || m == nil {
		return nil, errors.New("install must be an object of platform to URL")
	}
	out := map[string]string{}
	for p, uv := range m {
		if !slices.Contains(platforms, p) {
			return nil, fmt.Errorf("install: the platform must be one of %s",
				strings.Join(platforms, ", "))
		}
		s, _ := rawString(uv)
		u, err := url.Parse(s)
		bad := err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil
		if bad || len(s) > maxStoreURL {
			return nil, fmt.Errorf("install %s must be an https URL", p)
		}
		out[p] = s
	}
	return out, nil
}

// PlatformOf guesses the platform from a User-Agent; "" when it cannot tell. An iPad that asks for
// desktop sites says it is a Mac: every app of the catalog that runs on iOS also runs on a Mac, so
// the guess offers the right apps.
func PlatformOf(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		return PlatformIOS
	case strings.Contains(ua, "android"):
		return PlatformAndroid
	case strings.Contains(ua, "macintosh"), strings.Contains(ua, "mac os x"):
		return PlatformMacOS
	case strings.Contains(ua, "windows"):
		return PlatformWindows
	case strings.Contains(ua, "linux"), strings.Contains(ua, "x11"), strings.Contains(ua, "cros"):
		return PlatformLinux
	}
	return ""
}
