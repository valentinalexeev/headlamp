package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const navMarker = "data-nc-nav"

// ValidateNavItem checks a bar entry: a short printable label and an absolute local path.
func ValidateNavItem(it NavItem) error {
	if !utf8.ValidString(it.Label) {
		return fmt.Errorf("nav label %q is not valid UTF-8", it.Label)
	}
	if strings.TrimSpace(it.Label) == "" {
		return fmt.Errorf("nav label is empty or blank")
	}
	if utf8.RuneCountInString(it.Label) > 24 {
		return fmt.Errorf("nav label %q is longer than 24 characters", it.Label)
	}
	// Control and format characters (bidi overrides, zero-width marks) could disguise a label.
	if strings.IndexFunc(it.Label, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
		return fmt.Errorf("nav label %q contains a control or format character", it.Label)
	}
	p := it.Path
	switch {
	case !strings.HasPrefix(p, "/"), strings.HasPrefix(p, "//"):
		return fmt.Errorf("nav path %q must start with a single /", p)
	case strings.ContainsAny(p, "\\: ?#<>\""):
		return fmt.Errorf("nav path %q must not contain a colon, backslash, space, ?, #, <, > or a quote", p)
	case strings.IndexFunc(p, unicode.IsControl) >= 0:
		return fmt.Errorf("nav path %q contains a control character", p)
	}
	return nil
}

// CollectNav gathers the entries of every module that implements Navigator, validates them and
// sorts by Order, then Label. Pass only modules whose Register succeeded.
func CollectNav(mods []Module) ([]NavItem, error) {
	var items []NavItem
	owner := map[string]string{}
	for _, m := range mods {
		n, ok := m.(Navigator)
		if !ok {
			continue
		}
		for _, it := range n.Nav() {
			if err := ValidateNavItem(it); err != nil {
				return nil, fmt.Errorf("module %s: %w", m.Name(), err)
			}
			if prev, dup := owner[it.Path]; dup {
				return nil, fmt.Errorf("nav path %q claimed by both module %s and module %s", it.Path, prev, m.Name())
			}
			owner[it.Path] = m.Name()
			items = append(items, it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Order != items[j].Order {
			return items[i].Order < items[j].Order
		}
		return items[i].Label < items[j].Label
	})
	return items, nil
}

// The pages are dark-only and define --panel, --border, --text, --muted, --accent and --bg; the
// bar uses them and falls back to the same dark palette for pages that define none.
const navCSS = `[data-nc-nav]{position:fixed;top:0;left:0;right:0;z-index:1000;box-sizing:border-box;height:36px;margin:0;padding:0 12px;display:flex;align-items:center;background:var(--panel,#131824);border-bottom:1px solid var(--border,#232a3a);font:14px/1 -apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif}` +
	`[data-nc-nav] ul{display:flex;gap:4px;margin:0;padding:0;list-style:none}` +
	`[data-nc-nav] a{display:block;padding:7px 10px;border-radius:6px;color:var(--muted,#8b93a7);text-decoration:none}` +
	`[data-nc-nav] a:hover{background:var(--bg,#0b0e14)}` +
	`[data-nc-nav] a[aria-current]{background:var(--bg,#0b0e14);color:var(--text,#e6e9ef);font-weight:600;box-shadow:inset 0 -2px var(--accent,#4f8cff)}`

const navSpacer = `<div data-nc-nav-spacer aria-hidden="true" style="height:36px"></div>`

// navActive returns the index of the one current item: the longest path that equals the request
// path or is a parent of it ("/" only matches exactly "/"). -1 when none.
func navActive(items []NavItem, reqPath string) int {
	best, bestLen := -1, -1
	for i, it := range items {
		ok := false
		if it.Path == "/" {
			ok = reqPath == "/"
		} else {
			ok = reqPath == it.Path || strings.HasPrefix(reqPath, strings.TrimSuffix(it.Path, "/")+"/")
		}
		if ok && len(it.Path) > bestLen {
			best, bestLen = i, len(it.Path)
		}
	}
	return best
}

func renderNav(items []NavItem, reqPath string) []byte {
	var b bytes.Buffer
	b.WriteString("<style>" + navCSS + "</style>")
	b.WriteString(`<nav ` + navMarker + ` aria-label="Modules"><ul>`)
	active := navActive(items, reqPath)
	for i, it := range items {
		b.WriteString(`<li><a href="` + html.EscapeString(it.Path) + `"`)
		if i == active {
			b.WriteString(` aria-current="page"`)
		}
		b.WriteString(`>` + html.EscapeString(it.Label) + `</a></li>`)
	}
	b.WriteString(`</ul></nav>` + navSpacer)
	return b.Bytes()
}

var bodyTag = regexp.MustCompile(`^(?i)<body(?:\s(?:"[^"]*"|'[^']*'|[^>"'])*)?>`)

func asciiPrefixFold(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	return strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// findBodyTag returns the end offset of the first real opening <body> tag, skipping comments and
// script/style blocks, or -1.
func findBodyTag(b []byte) int {
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '<')
		if j < 0 {
			return -1
		}
		i += j
		rest := b[i:]
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			k := bytes.Index(rest[4:], []byte("-->"))
			if k < 0 {
				return -1
			}
			i += 4 + k + 3
		case asciiPrefixFold(rest, "<script") || asciiPrefixFold(rest, "<style"):
			name := "</script"
			if asciiPrefixFold(rest, "<style") {
				name = "</style"
			}
			k := -1
			for p := 1; p < len(rest); {
				q := bytes.IndexByte(rest[p:], '<')
				if q < 0 {
					break
				}
				p += q
				if asciiPrefixFold(rest[p:], name) {
					k = p
					break
				}
				p++
			}
			if k < 0 {
				return -1
			}
			i += k + len(name)
		default:
			if loc := bodyTag.FindIndex(rest); loc != nil {
				return i + loc[1]
			}
			i++
		}
	}
	return -1
}

// NavMiddleware serves GET /_nav.json and inserts the bar into successful HTML responses to
// GET and HEAD requests when there are at least two entries. Everything else passes untouched.
func NavMiddleware(items []NavItem, next http.Handler) http.Handler {
	list := append([]NavItem{}, items...)
	jsonBody, _ := json.Marshal(list)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/_nav.json" {
			h := w.Header()
			h.Set("Content-Type", "application/json")
			h.Set("Cache-Control", "no-store")
			out := append(append([]byte{}, jsonBody...), '\n')
			h.Set("Content-Length", strconv.Itoa(len(out)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(out)
			}
			return
		}
		if len(list) < 2 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		nw := &navWriter{ResponseWriter: w, bar: func() []byte { return renderNav(list, r.URL.Path) }}
		next.ServeHTTP(nw, r)
		nw.finish()
	})
}

// maxNavBuffer bounds how much of an HTML response is held back; a larger page is passed through
// unchanged. ETag and Last-Modified are left alone: the admin pages set neither (they are static
// embedded bytes), and a validator on a page that is not byte-identical would be wrong anyway.
const maxNavBuffer = 1 << 20

// navWriter buffers a response only when it looks like an uncompressed 200 HTML page; every other
// response streams through. The decision is made at WriteHeader or the first Write. io.ReaderFrom
// is deliberately not implemented: io.Copy then falls back to Write, which this type must see.
type navWriter struct {
	http.ResponseWriter
	bar       func() []byte
	decided   bool
	buffering bool
	status    int
	buf       bytes.Buffer
}

func (n *navWriter) Unwrap() http.ResponseWriter { return n.ResponseWriter }

func (n *navWriter) decide(status int, first []byte) {
	n.decided = true
	n.status = status
	h := n.Header()
	ct := h.Get("Content-Type")
	if ct == "" && len(first) > 0 {
		ct = http.DetectContentType(first)
	}
	n.buffering = status == http.StatusOK && h.Get("Content-Encoding") == "" &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/html")
}

func (n *navWriter) WriteHeader(code int) {
	if n.decided {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		n.ResponseWriter.WriteHeader(code)
		return
	}
	n.decide(code, nil)
	if !n.buffering {
		n.ResponseWriter.WriteHeader(code)
	}
}

func (n *navWriter) Write(p []byte) (int, error) {
	if !n.decided {
		n.decide(http.StatusOK, p)
		if !n.buffering {
			n.ResponseWriter.WriteHeader(http.StatusOK)
		}
	}
	if !n.buffering {
		return n.ResponseWriter.Write(p)
	}
	if n.buf.Len()+len(p) > maxNavBuffer {
		// Too large to hold: emit what was buffered unchanged and stream the rest.
		n.buffering = false
		n.ResponseWriter.WriteHeader(n.status)
		if _, err := n.ResponseWriter.Write(n.buf.Bytes()); err != nil {
			return 0, err
		}
		n.buf = bytes.Buffer{}
		return n.ResponseWriter.Write(p)
	}
	return n.buf.Write(p)
}

// Flush does nothing until the response is decided, so an early Flush cannot commit an implicit
// 200 before the status and headers are known; it is also held back while a page is buffered.
func (n *navWriter) Flush() {
	if n.decided && !n.buffering {
		if f, ok := n.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// Hijack hands the connection over when the underlying writer allows it; the response is then
// never buffered.
func (n *navWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := n.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	n.decided, n.buffering = true, false
	return hj.Hijack()
}

// finish emits a buffered page, with the bar inserted when the page has a body tag.
func (n *navWriter) finish() {
	if !n.buffering {
		return
	}
	body := n.buf.Bytes()
	if end := findBodyTag(body); end >= 0 && !bytes.Contains(body, []byte(navMarker)) {
		bar := n.bar()
		out := make([]byte, 0, len(body)+len(bar))
		out = append(out, body[:end]...)
		out = append(out, bar...)
		out = append(out, body[end:]...)
		body = out
		n.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	n.ResponseWriter.WriteHeader(n.status)
	_, _ = n.ResponseWriter.Write(body)
}
