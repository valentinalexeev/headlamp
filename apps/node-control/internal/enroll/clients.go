package enroll

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
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
	// AppStoreID is the numeric id of the app in the Apple App Store, for the Smart App Banner of
	// the app page (#271) and the default iOS store link. "" when the app has no iOS listing.
	AppStoreID string
	// AndroidPackage is the application id of the Android app, for the intent:// links of the
	// picker on Android (#271): Chrome opens that app when it is installed, else a fallback URL.
	AndroidPackage string
	// OnPlay says the Android app is on Google Play, so its Play page is a good fallback.
	OnPlay bool
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
	// The iOS listing that the sing-box documentation links ("sing-box MT").
	{ID: "sing-box", Label: "sing-box", Format: FormatSingBox, AppStoreID: "6785326793",
		AndroidPackage: "io.nekohasekai.sfa", OnPlay: true,
		Platforms: []string{PlatformIOS, PlatformMacOS, PlatformAndroid},
		link: func(subURL, name string) string {
			return "sing-box://import-remote-profile?url=" + quote(subURL) + "#" + quote(name)
		}},
	{ID: "karing", Label: "Karing", Format: FormatSingBox, Landing: true, AppStoreID: "6472431552",
		AndroidPackage: "com.nebula.karing",
		Platforms: []string{
			PlatformIOS, PlatformMacOS, PlatformAndroid, PlatformWindows, PlatformLinux,
		},
		link: installConfigLink("karing")},
	{ID: "clash", Label: "Clash Mi", Format: FormatClash, Landing: true, AppStoreID: "6744321968",
		AndroidPackage: "com.nebula.clashmi",
		Platforms:      []string{PlatformIOS, PlatformMacOS, PlatformAndroid, PlatformWindows},
		link:           installConfigLink("clash")},
	{ID: "shadowrocket", Label: "Shadowrocket", Format: FormatURIList, Landing: true,
		AppStoreID: "932747118",
		Platforms:  []string{PlatformIOS, PlatformMacOS},
		// The URL is standard base64, as Shadowrocket's own sub:// QR codes carry it.
		link: func(subURL, name string) string {
			b := base64.StdEncoding.EncodeToString([]byte(subURL))
			return "shadowrocket://add/sub://" + b + "?remark=" + quote(name)
		}},
	{ID: "stash", Label: "Stash", Format: FormatStash, Landing: true, AppStoreID: "1596063349",
		Platforms: []string{PlatformIOS, PlatformMacOS},
		link:      installConfigLink("stash")},
	// Happ (#278), an xray-core app. Its import link is happ://add/ followed by the plain
	// subscription URL: Happ takes everything after the prefix as the URL, so the URL is not
	// encoded. The link has no name: Happ names the subscription from the profile-title
	// header, else from the host.
	{ID: "happ", Label: "Happ", Format: FormatURIList, Landing: true, AppStoreID: "6504287215",
		AndroidPackage: "com.happproxy", OnPlay: true,
		Platforms: []string{
			PlatformIOS, PlatformMacOS, PlatformAndroid, PlatformWindows, PlatformLinux,
		},
		link: happLink},
	// Throne (#295), a sing-box client for desktop and Android; there is no iOS app. It is not on
	// Google Play, so it has no default Android store link.
	{ID: "throne", Label: "Throne", Format: FormatURIList, Landing: true,
		AndroidPackage: "com.nb4a.throne",
		Platforms:      []string{PlatformAndroid, PlatformWindows, PlatformMacOS, PlatformLinux},
		link:           throneLink},
}

// maxThroneName bounds the subscription name in a Throne link.
const maxThroneName = 64

// throneLink is "throne://addsub/" and the standard Base64 (with "+", "/" and padding; Throne
// refuses URL-safe Base64) of "<subscription URL>#<name>". Throne reads the URL as it is and the
// name after the first "#" (https://throneproj.github.io/advanced/deeplinks/). The subscription
// URL has no "#", so the split is exact. Line breaks leave the name, which is also shortened.
func throneLink(subURL, name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > maxThroneName {
		name = string(r[:maxThroneName])
	}
	payload := subURL
	if name != "" {
		payload += "#" + name
	}
	return "throne://addsub/" + base64.StdEncoding.EncodeToString([]byte(payload))
}

// happLink is "happ://add/" and the plain subscription URL. Happ reads the URL without decoding,
// so it is not encoded: it must be an https URL of only URL-safe characters. node-control makes
// it from link_host and a hex secret, so it always is; anything else gives no link ("") rather
// than a link that could break out of an attribute.
func happLink(subURL, _ string) string {
	if !strings.HasPrefix(subURL, "https://") {
		return ""
	}
	for _, c := range subURL {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.ContainsRune(":/.-_~", c)
		if !ok {
			return ""
		}
	}
	return "happ://add/" + subURL
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
	// AppStoreID replaces the App Store id of the catalog (#271), because listings change.
	AppStoreID string `json:"app_store_id,omitempty"`
	// AndroidPackage replaces the Android application id of the catalog (#271).
	AndroidPackage string `json:"android_package,omitempty"`
}

// androidPackageRe is the form of an Android application id.
var androidPackageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)

// maxAndroidPackage bounds an application id of enroll.json.
const maxAndroidPackage = 255

// Package is the Android application id of the app: the site's, else the catalog's.
func (ch ClientChoice) Package() string {
	if ch.AndroidPackage != "" {
		return ch.AndroidPackage
	}
	def, _ := ClientByID(ch.ID)
	return def.AndroidPackage
}

// PlayURL is the Google Play page of an Android application id.
func PlayURL(pkg string) string { return "https://play.google.com/store/apps/details?id=" + pkg }

// intentLink makes an import link an Android intent:// link for one package. Chrome opens the
// package with the import link when it is installed, else it goes to fallback. The data part
// keeps the import link whole: Android splits the link at its last "#", the one of "#Intent;".
func intentLink(importLink, pkg, fallback string) string {
	scheme, rest, ok := strings.Cut(importLink, "://")
	if !ok {
		return importLink
	}
	return "intent://" + rest + "#Intent;scheme=" + scheme + ";package=" + pkg +
		";S.browser_fallback_url=" + quote(fallback) + ";end"
}

// appStoreIDRe is the form of an App Store id.
var appStoreIDRe = regexp.MustCompile(`^[0-9]{1,12}$`)

// StoreID is the App Store id of the app: the site's, else the catalog's. "" when it has none.
func (ch ClientChoice) StoreID() string {
	if ch.AppStoreID != "" {
		return ch.AppStoreID
	}
	def, _ := ClientByID(ch.ID)
	return def.AppStoreID
}

// AppStoreURL is the App Store page of an app id. Apple sends the visitor to their own storefront.
func AppStoreURL(id string) string { return "https://apps.apple.com/app/id" + id }

// installLinks are the store links of an app per platform: the site's, plus the App Store page of
// the app id for iOS when the site gives no iOS link.
func (ch ClientChoice) installLinks() map[string]string {
	out := map[string]string{}
	for p, u := range ch.Install {
		out[p] = u
	}
	if id := ch.StoreID(); id != "" && out[PlatformIOS] == "" {
		out[PlatformIOS] = AppStoreURL(id)
	}
	def, _ := ClientByID(ch.ID)
	// The Play page belongs to the catalog's package: a site's own package may not be on Play.
	onPlay := def.OnPlay && (ch.AndroidPackage == "" || ch.AndroidPackage == def.AndroidPackage)
	if out[PlatformAndroid] == "" && onPlay && ch.Package() != "" {
		out[PlatformAndroid] = PlayURL(ch.Package())
	}
	return out
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
		if k != "id" && k != "install" && k != "app_store_id" && k != "android_package" {
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
	if v, present := raw["app_store_id"]; present {
		s, _ := rawString(v)
		if !appStoreIDRe.MatchString(s) {
			return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d] %s: "+
				"app_store_id must be a string of 1 to 12 digits", i, id)
		}
		ch.AppStoreID = s
	}
	if v, present := raw["android_package"]; present {
		s, _ := rawString(v)
		if !androidPackageRe.MatchString(s) || len(s) > maxAndroidPackage {
			return ClientChoice{}, fmt.Errorf("ENROLL_CONFIG: clients[%d] %s: android_package "+
				"must be an Android application id, for example com.example.app", i, id)
		}
		ch.AndroidPackage = s
	}
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
