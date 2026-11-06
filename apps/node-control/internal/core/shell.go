package core

import (
	"bytes"
	"encoding/json"
	"html"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

const (
	groupOperate   = "operate"
	groupConfigure = "configure"
)

// visibleItems keeps the entries the caller may see. An empty login (unknown or tagged caller)
// sees only entries without an Allow check.
func visibleItems(items []NavItem, login string) []NavItem {
	var out []NavItem
	for _, it := range items {
		if it.Allow == nil || (login != "" && it.Allow(login)) {
			out = append(out, it)
		}
	}
	return out
}

func esc(s string) string { return html.EscapeString(s) }

func themeButton(id, label, icon string) string {
	return `<button type="button" data-theme="` + id + `" aria-label="` + label + `" title="` + label +
		`" aria-pressed="false">` + ui.Icon(icon) + `</button>`
}

// themeControl is the System / Light / Dark control. shell.js marks the current choice.
func themeControl() string {
	return `<div class="seg" role="group" aria-label="Theme">` +
		themeButton("system", "System", "system") +
		themeButton("light", "Light", "sun") +
		themeButton("dark", "Dark", "moon") +
		`</div>`
}

// searchBox is the top-bar button that opens the command palette. The top-bar actions travel in a data attribute, so
// the palette offers them without a request; the pages come from /_nav.json when it opens.
func searchBox(actions []NavAction) string {
	type act struct {
		Label string `json:"label"`
		Path  string `json:"path"`
		Icon  string `json:"icon,omitempty"`
	}
	list := make([]act, 0, len(actions))
	for _, a := range actions {
		list = append(list, act{a.Label, a.Path, a.Icon})
	}
	data, _ := json.Marshal(list)
	var b strings.Builder
	if err := searchBoxTmpl.Execute(&b, struct {
		Actions string
		Icon    template.HTML
	}{string(data), template.HTML(ui.Icon("search"))}); err != nil { // ui.Icon is markup from this binary
		return ""
	}
	return b.String()
}

// searchBoxTmpl escapes for where each value goes, so the JSON cannot leave its attribute.
var searchBoxTmpl = template.Must(template.New("search").Parse(`<button type="button" class="search" data-palette aria-haspopup="dialog" data-actions="{{.Actions}}">` +
	`<span class="row nowrap">{{.Icon}}<span>Search or run a command</span></span><span class="kbd" data-palette-keys>⌘K</span></button>`))

func navLink(b *bytes.Buffer, it NavItem, active bool) {
	b.WriteString(`<a class="navitem`)
	if active {
		b.WriteString(` on`)
	}
	b.WriteString(`" href="` + esc(it.Path) + `"`)
	if active {
		b.WriteString(` aria-current="page"`)
	}
	if it.Source != "" {
		b.WriteString(` data-source="` + esc(it.Source) + `"`)
	}
	b.WriteString(`><span class="row nowrap">` + ui.Icon(it.Icon) + `<span>` + esc(it.Label) + `</span></span>`)
	if it.Source != "" {
		b.WriteString(`<span class="navcount" hidden></span>`)
	}
	b.WriteString(`</a>`)
}

func navGroup(b *bytes.Buffer, items []NavItem, activePath, label string) {
	if len(items) == 0 {
		return
	}
	b.WriteString(`<div class="navgroup collapsible">`)
	if label != "" {
		b.WriteString(`<div class="navlabel">` + label + `</div>`)
	}
	for _, it := range items {
		navLink(b, it, activePath != "" && it.Path == activePath)
	}
	b.WriteString(`</div>`)
}

// renderShell writes the console shell for one page: the theme script (render-blocking, so the
// page never flashes the wrong theme), the stylesheet, the sidebar (or, with fewer than two
// visible entries, a compact header with the theme control), the top bar, the empty status strip
// and the deferred scripts. Entries the login may not see are left out.
func renderShell(items []NavItem, actions []NavAction, login, reqPath string, now time.Time) []byte {
	visible := visibleItems(items, login)
	compact := len(visible) < 2
	var b bytes.Buffer
	b.WriteString(`<script src="/_ui/theme.js"></script>` + `<link rel="stylesheet" href="/_ui/hl.css">`)

	brand := `<div class="brand"><span class="lamp clear" aria-hidden="true"></span>headlamp`
	if !compact {
		activePath := ""
		if i := navActive(visible, reqPath); i >= 0 {
			activePath = visible[i].Path
		}
		b.WriteString(`<nav ` + navMarker + ` class="hl side" aria-label="Main">`)
		b.WriteString(brand + `<button type="button" class="iconbtn" data-nav-toggle aria-label="Menu" title="Menu" aria-expanded="false">` +
			ui.Icon("menu") + `</button></div>`)
		var ungrouped, operate, configure []NavItem
		for _, it := range visible {
			switch it.Group {
			case groupOperate:
				operate = append(operate, it)
			case groupConfigure:
				configure = append(configure, it)
			default:
				ungrouped = append(ungrouped, it)
			}
		}
		navGroup(&b, ungrouped, activePath, "")
		navGroup(&b, operate, activePath, "Operate")
		navGroup(&b, configure, activePath, "Configure")
		b.WriteString(`<div class="sidefoot"><div class="navlabel">Theme</div>` + themeControl() + `</div></nav>`)
		b.WriteString(`<div class="hl shellbar">`)
	} else {
		b.WriteString(`<div ` + navMarker + ` class="hl shellbar">`)
	}

	b.WriteString(`<div role="status" class="strip" hidden data-hl-loaded="` + strconv.FormatInt(now.Unix(), 10) + `"></div>`)
	b.WriteString(`<div class="top" data-top>`)
	if compact {
		b.WriteString(brand + `</div>`)
	} else {
		b.WriteString(searchBox(actions))
	}
	b.WriteString(`<span class="fresh mono" data-fresh></span>`)
	for _, a := range actions {
		b.WriteString(`<a class="btn primary" href="` + esc(a.Path) + `">` + ui.Icon(a.Icon) + `<span>` + esc(a.Label) + `</span></a>`)
	}
	if compact {
		b.WriteString(themeControl())
	}
	b.WriteString(`</div></div>`)
	b.WriteString(`<script src="/_ui/hl.js" defer></script><script src="/_ui/shell.js" defer></script><script src="/_ui/palette.js" defer></script>`)
	return b.Bytes()
}
