package core

import (
	"bufio"
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type navMod struct {
	stubModule
	items []NavItem
}

func (m *navMod) Nav() []NavItem { return m.items }

func twoItems() []NavItem {
	return []NavItem{{Label: "Nodes", Path: "/", Order: 10}, {Label: "Enrolment", Path: "/enroll", Order: 20}}
}

func htmlHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	})
}

// testNav wraps next with the shell middleware for an anonymous caller and no top-bar action.
func testNav(items []NavItem, next http.Handler) http.Handler {
	return NavMiddleware(items, nil, nil, next)
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestValidateNavItem(t *testing.T) {
	long := strings.Repeat("x", 25)
	cases := []struct {
		name string
		it   NavItem
		ok   bool
	}{
		{"root", NavItem{Label: "Nodes", Path: "/"}, true},
		{"enroll", NavItem{Label: "Enrolment", Path: "/enroll"}, true},
		{"24 runes", NavItem{Label: strings.Repeat("é", 24), Path: "/a"}, true},
		{"double slash", NavItem{Label: "A", Path: "//x"}, false},
		{"absolute url", NavItem{Label: "A", Path: "http://x"}, false},
		{"script url", NavItem{Label: "A", Path: "javascript:x"}, false},
		{"relative", NavItem{Label: "A", Path: "x"}, false},
		{"empty label", NavItem{Label: "", Path: "/a"}, false},
		{"25 runes", NavItem{Label: long, Path: "/a"}, false},
		{"newline label", NavItem{Label: "a\nb", Path: "/a"}, false},
		{"space path", NavItem{Label: "A", Path: "/a b"}, false},
		{"backslash path", NavItem{Label: "A", Path: `/a\b`}, false},
		{"control path", NavItem{Label: "A", Path: "/a\x01"}, false},
		{"query", NavItem{Label: "A", Path: "/a?x=1"}, false},
		{"fragment", NavItem{Label: "A", Path: "/a#x"}, false},
		{"angle bracket", NavItem{Label: "A", Path: "/a<b"}, false},
		{"closing angle", NavItem{Label: "A", Path: "/a>b"}, false},
		{"quote", NavItem{Label: "A", Path: `/a"b`}, false},
		{"blank label", NavItem{Label: " \t ", Path: "/a"}, false},
		{"invalid utf8 label", NavItem{Label: "a\xffb", Path: "/a"}, false},
		{"bidi override label", NavItem{Label: "a\u202eb", Path: "/a"}, false},
		{"zero width label", NavItem{Label: "a\u200bb", Path: "/a"}, false},
		{"markup label is data", NavItem{Label: `<script>"a"`, Path: "/a"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateNavItem(c.it); (err == nil) != c.ok {
				t.Fatalf("ValidateNavItem(%+v) = %v, want ok=%v", c.it, err, c.ok)
			}
		})
	}
}

func TestCollectNavSortsAndRejectsDuplicates(t *testing.T) {
	t.Run("sorted by order then label", func(t *testing.T) {
		mods := []Module{
			&navMod{stubModule{name: "b"}, []NavItem{{Label: "Zed", Path: "/z", Order: 5}, {Label: "Beta", Path: "/b", Order: 1}}},
			&navMod{stubModule{name: "a"}, []NavItem{{Label: "Alpha", Path: "/a", Order: 1}}},
		}
		got, err := CollectNav(mods)
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		for _, it := range got {
			labels = append(labels, it.Label)
		}
		if strings.Join(labels, ",") != "Alpha,Beta,Zed" {
			t.Fatalf("order %v", labels)
		}
	})
	t.Run("duplicate path names both modules", func(t *testing.T) {
		mods := []Module{
			&navMod{stubModule{name: "first"}, []NavItem{{Label: "A", Path: "/x"}}},
			&navMod{stubModule{name: "second"}, []NavItem{{Label: "B", Path: "/x"}}},
		}
		_, err := CollectNav(mods)
		if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
			t.Fatalf("want error naming both modules, got %v", err)
		}
	})
	t.Run("invalid entry names its module", func(t *testing.T) {
		_, err := CollectNav([]Module{&navMod{stubModule{name: "bad"}, []NavItem{{Label: "A", Path: "//evil"}}}})
		if err == nil || !strings.Contains(err.Error(), "bad") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCollectNavSkipsModulesWithoutNavigator(t *testing.T) {
	got, err := CollectNav([]Module{&stubModule{name: "plain"}, &navMod{stubModule{name: "n"}, nil}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestNavMiddlewareInjectsAfterBody(t *testing.T) {
	h := testNav(twoItems(), htmlHandler(`<html><head></head><BODY class="x" data-a='>'><p>hi</p></BODY></html>`))
	body := do(h, "GET", "/").Body.String()
	const open = `<html><head></head><BODY class="x" data-a='>'>`
	if !strings.HasPrefix(body, open+`<script src="/_ui/theme.js"></script><link rel="stylesheet" href="/_ui/hl.css"><nav data-nc-nav `) {
		t.Fatalf("shell not right after <body>: %s", body)
	}
	if !strings.HasSuffix(body, `<script src="/_ui/hl.js" defer></script><script src="/_ui/shell.js" defer></script><script src="/_ui/palette.js" defer></script><p>hi</p></BODY></html>`) {
		t.Fatalf("page content not right after the shell: %s", body)
	}
}

func TestNavMiddlewareLeavesPageAloneWithoutEntries(t *testing.T) {
	const page = `<body><p>hi</p></body>`
	for _, items := range [][]NavItem{nil, {}} {
		if got := do(testNav(items, htmlHandler(page)), "GET", "/").Body.String(); got != page {
			t.Fatalf("%d items: %q", len(items), got)
		}
	}
}

func TestNavMiddlewareLeavesNonHTMLAlone(t *testing.T) {
	mk := func(status int, ct, loc, body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct != "" {
				w.Header().Set("Content-Type", ct)
			}
			if loc != "" {
				w.Header().Set("Location", loc)
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
	}
	const page = `<body>x</body>`
	cases := []struct {
		name, method string
		h            http.Handler
	}{
		{"json", "GET", mk(200, "application/json", "", `{"a":"<body>"}`)},
		{"404 html", "GET", mk(404, "text/html", "", page)},
		{"302 html", "GET", mk(302, "text/html", "/x", page)},
		{"500 html", "GET", mk(500, "text/html", "", page)},
		{"POST html", "POST", mk(200, "text/html", "", page)},
		{"compressed html", "GET", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, page)
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(testNav(twoItems(), c.h), c.method, "/")
			if strings.Contains(rec.Body.String(), "data-nc-nav") {
				t.Fatalf("bar injected: %q", rec.Body.String())
			}
		})
	}
	t.Run("status and location kept", func(t *testing.T) {
		rec := do(testNav(twoItems(), mk(302, "text/html", "/x", page)), "GET", "/")
		if rec.Code != 302 || rec.Header().Get("Location") != "/x" || rec.Body.String() != page {
			t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
		}
	})
}

func TestNavMiddlewareNoBodyTag(t *testing.T) {
	const frag = `<p>fragment, a <bodyguard> mention</p>`
	rec := do(testNav(twoItems(), htmlHandler(frag)), "GET", "/")
	if rec.Body.String() != frag {
		t.Fatalf("got %q", rec.Body.String())
	}
}

func TestNavMiddlewareContentLengthMatches(t *testing.T) {
	const page = `<html><body><p>hi</p></body></html>`
	h := testNav(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(len(page)))
		_, _ = io.WriteString(w, page)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "data-nc-nav") {
		t.Fatal("not injected")
	}
	if resp.ContentLength != int64(len(b)) {
		t.Fatalf("Content-Length %d, body %d", resp.ContentLength, len(b))
	}
}

func TestNavMiddlewareHEAD(t *testing.T) {
	const page = `<body>x</body>`
	srv := httptest.NewServer(testNav(twoItems(), htmlHandler(page)))
	defer srv.Close()
	get, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if !strings.Contains(string(gb), "<nav data-nc-nav") || len(gb) <= len(page) {
		t.Fatalf("GET lacks the bar: %s", gb)
	}
	head, err := http.Head(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != 200 || head.ContentLength != int64(len(gb)) {
		t.Fatalf("HEAD %d length %d, GET body %d (bar must be counted)", head.StatusCode, head.ContentLength, len(gb))
	}
	t.Run("HEAD /_nav.json", func(t *testing.T) {
		rec := do(testNav(twoItems(), http.NotFoundHandler()), "HEAD", "/_nav.json")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
			t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
		}
	})
}

func TestNavMiddlewareNoDoubleInjection(t *testing.T) {
	h := testNav(twoItems(), htmlHandler(`<body>x</body>`))
	outer := testNav(twoItems(), h)
	if n := strings.Count(do(outer, "GET", "/").Body.String(), "<nav data-nc-nav"); n != 1 {
		t.Fatalf("%d bars", n)
	}
}

func TestNavActiveItem(t *testing.T) {
	h := testNav(twoItems(), htmlHandler(`<body></body>`))
	cases := []struct{ path, want string }{
		{"/", "Nodes"},
		{"/enroll", "Enrolment"},
		{"/enroll/x", "Enrolment"},
		{"/enrollment", ""},
		{"/other", ""},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			body := do(h, "GET", c.path).Body.String()
			if n := strings.Count(body, `aria-current="page"`); (c.want == "") != (n == 0) || n > 1 {
				t.Fatalf("%d highlighted in %s", n, body)
			}
			if c.want != "" && !strings.Contains(body, `aria-current="page"><span class="row nowrap"><span>`+c.want+`</span>`) {
				t.Fatalf("%s not active: %s", c.want, body)
			}
		})
	}
}

func TestNavActiveItemLongestWins(t *testing.T) {
	items := []NavItem{{Label: "A", Path: "/a", Order: 1}, {Label: "AB", Path: "/a/b", Order: 2}, {Label: "Root", Path: "/", Order: 0}}
	h := testNav(items, htmlHandler(`<body></body>`))
	for path, want := range map[string]string{"/a": "A", "/a/b": "AB", "/a/b/c": "AB", "/a/x": "A", "/": "Root", "/zzz": ""} {
		t.Run(path, func(t *testing.T) {
			body := do(h, "GET", path).Body.String()
			n := strings.Count(body, `aria-current="page"`)
			if want == "" && n != 0 || want != "" && (n != 1 || !strings.Contains(body, `aria-current="page"><span class="row nowrap"><span>`+want+`</span>`)) {
				t.Fatalf("%d current, want %q: %s", n, want, body)
			}
		})
	}
}

func TestNavJSON(t *testing.T) {
	for _, items := range [][]NavItem{twoItems(), nil} {
		rec := do(testNav(items, http.NotFoundHandler()), "GET", "/_nav.json")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%d %v", rec.Code, rec.Header())
		}
		var got []NavItem
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != len(items) {
			t.Fatalf("%v %q", err, rec.Body.String())
		}
	}
	t.Run("shape", func(t *testing.T) {
		rec := do(testNav(twoItems(), http.NotFoundHandler()), "GET", "/_nav.json")
		if !strings.HasPrefix(rec.Body.String(), `[{"label":"Nodes","path":"/","order":10},`) {
			t.Fatal(rec.Body.String())
		}
	})
	t.Run("not injected", func(t *testing.T) {
		rec := do(testNav(twoItems(), htmlHandler(`<body>`)), "GET", "/_nav.json")
		if strings.Contains(rec.Body.String(), "<nav") {
			t.Fatal("json wrapped")
		}
	})
}

func TestNavEscapesLabels(t *testing.T) {
	items := []NavItem{{Label: `<script>"a"&'b'`, Path: "/x", Order: 1}, {Label: "Two", Path: "/two", Order: 2}}
	for _, it := range items {
		if err := ValidateNavItem(it); err != nil {
			t.Fatal(err)
		}
	}
	rec := do(testNav(items, htmlHandler(`<body></body>`)), "GET", "/")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, `"a"`) {
		t.Fatalf("label not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;&#34;a&#34;&amp;&#39;b&#39;") {
		t.Fatalf("unexpected escaping: %s", body)
	}
	j := do(testNav(items, http.NotFoundHandler()), "GET", "/_nav.json").Body.String()
	if strings.Contains(j, "<script>") {
		t.Fatalf("json not escaped: %s", j)
	}
	t.Run("renderer escapes a path even if validation is bypassed", func(t *testing.T) {
		out := string(renderShell([]NavItem{{Label: "X", Path: "/x?a=1&b=<2>"}, {Label: "Y", Path: "/y"}}, nil, "", "/", time.Unix(0, 0)))
		if !strings.Contains(out, `href="/x?a=1&amp;b=&lt;2&gt;"`) {
			t.Fatal(out)
		}
	})
}

func TestNavMiddlewareFlushAndStreaming(t *testing.T) {
	const page = `<html><body><p>hi</p></body></html>`
	wire := func(t *testing.T, h http.Handler, method string) (*http.Response, []byte) {
		t.Helper()
		srv := httptest.NewServer(testNav(twoItems(), h))
		defer srv.Close()
		req, _ := http.NewRequest(method, srv.URL+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return resp, b
	}
	t.Run("Flush before the first write, own Content-Length", func(t *testing.T) {
		resp, b := wire(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Length", strconv.Itoa(len(page)))
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, page)
		}), "GET")
		if resp.StatusCode != 200 || int64(len(b)) != resp.ContentLength || !strings.Contains(string(b), "<nav data-nc-nav") {
			t.Fatalf("%d len %d/%d %s", resp.StatusCode, len(b), resp.ContentLength, b)
		}
	})
	t.Run("Flush before a non-200 status keeps the status", func(t *testing.T) {
		resp, _ := wire(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush()
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, page)
		}), "GET")
		if resp.StatusCode != 404 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("explicit WriteHeader(200) and chunked writes", func(t *testing.T) {
		resp, b := wire(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(200)
			for _, c := range []string{"<html><bo", "dy cl", `ass="x"><p>hi</p>`, "</body></html>"} {
				_, _ = io.WriteString(w, c)
				w.(http.Flusher).Flush()
			}
		}), "GET")
		if resp.StatusCode != 200 || strings.Count(string(b), "<nav data-nc-nav") != 1 || !strings.HasSuffix(string(b), "<p>hi</p></body></html>") {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	})
	t.Run("non-HTML streams while flushing", func(t *testing.T) {
		seen := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(testNav(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "one\n")
			w.(http.Flusher).Flush()
			close(seen)
			<-release
			_, _ = io.WriteString(w, "two\n")
			w.(http.Flusher).Flush()
		})))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		<-seen
		buf := make([]byte, 4)
		if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != "one\n" {
			t.Fatalf("first chunk not streamed before the handler finished: %q %v", buf, err)
		}
		close(release)
		rest, _ := io.ReadAll(resp.Body)
		if string(rest) != "two\n" {
			t.Fatalf("rest %q", rest)
		}
	})
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestNavWriterHijack(t *testing.T) {
	t.Run("delegates", func(t *testing.T) {
		rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
		h := testNav(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no Hijacker through the middleware")
				return
			}
			_, _, _ = hj.Hijack()
		}))
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if !rec.hijacked {
			t.Fatal("not delegated")
		}
	})
	t.Run("errors when unsupported", func(t *testing.T) {
		h := testNav(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, _, err := w.(http.Hijacker).Hijack(); err == nil {
				t.Error("want an error")
			}
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	})
}

func TestNavMiddlewareBufferCap(t *testing.T) {
	big := "<html><body>" + strings.Repeat("x", 2<<20) + "</body></html>"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		for i := 0; i < len(big); i += 64 << 10 {
			end := i + 64<<10
			if end > len(big) {
				end = len(big)
			}
			_, _ = io.WriteString(w, big[i:end])
		}
	})
	srv := httptest.NewServer(testNav(twoItems(), h))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("corrupted: %v", err)
	}
	if string(b) != big {
		t.Fatalf("body changed (%d vs %d bytes)", len(b), len(big))
	}
	if resp.ContentLength != int64(len(b)) {
		t.Fatalf("Content-Length %d, body %d", resp.ContentLength, len(b))
	}
}

func TestNavMiddlewareSkipsCommentsAndScripts(t *testing.T) {
	page := `<!doctype html><html><head><!-- <body> not this --><script>var s = "<body>";</SCRIPT><style>/* <body> */</style></head><body class="real"><p>x</p></body></html>`
	body := do(testNav(twoItems(), htmlHandler(page)), "GET", "/").Body.String()
	i := strings.Index(body, `<body class="real">`)
	if i < 0 || !strings.HasPrefix(body[i+len(`<body class="real">`):], `<script src="/_ui/theme.js">`) {
		t.Fatalf("bar not right after the real body tag: %s", body)
	}
	if strings.Count(body, "<nav data-nc-nav") != 1 {
		t.Fatal("injected more than once")
	}
	t.Run("only decoys: untouched", func(t *testing.T) {
		const decoy = `<html><head><!-- <body> --></head></html>`
		if got := do(testNav(twoItems(), htmlHandler(decoy)), "GET", "/").Body.String(); got != decoy {
			t.Fatal(got)
		}
	})
}

func TestRunNavWiring(t *testing.T) {
	page := func(r *Routes) {
		r.Admin.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<body>admin</body>")
		})
		if r.Public != nil {
			r.Public.HandleFunc("/pub", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<body>pub</body>")
			})
		}
	}
	mods := func() []Module {
		return []Module{
			&navMod{stubModule{name: "a", register: page}, []NavItem{{Label: "A", Path: "/", Order: 1}}},
			&navMod{stubModule{name: "b"}, []NavItem{{Label: "B", Path: "/b", Order: 2}}},
		}
	}
	env := MapEnv(map[string]string{"ENROLL_DB": t.TempDir() + "/e.db", "ENROLL_ALLOWED_HOSTS": "good.test"})
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, mods(), env, tn)
	defer stop()
	if code, body := get(t, tn.AdminAddr(), "good.test", "/"); code != 200 || !strings.Contains(body, "data-nc-nav") {
		t.Fatalf("admin: %d %s", code, body)
	}
	if code, body := get(t, tn.PublicAddr(), "", "/pub"); code != 200 || strings.Contains(body, "nav") {
		t.Fatalf("public: %d %s", code, body)
	}
	if code, body := get(t, tn.AdminAddr(), "good.test", "/_ui/hl.css"); code != 200 || !strings.Contains(body, ".hl") {
		t.Fatalf("admin asset: %d", code)
	}
	if code, _ := get(t, tn.PublicAddr(), "", "/_ui/hl.css"); code != 404 {
		t.Fatalf("public asset: %d, want 404", code)
	}
	if code, body := get(t, tn.AdminAddr(), "evil.test", "/_nav.json"); code != 400 || strings.Contains(body, "label") {
		t.Fatalf("bad host: %d %s", code, body)
	}
	t.Run("bad entry fails Run", func(t *testing.T) {
		bad := []Module{&navMod{stubModule{name: "bad"}, []NavItem{{Label: "A", Path: "//x"}}}}
		err := Run(context.Background(), bad, MapEnv(nil), NewLocal("127.0.0.1"))
		if err == nil || !strings.Contains(err.Error(), "bad") {
			t.Fatalf("got %v", err)
		}
	})
}

// ---- the shell ----

type actMod struct {
	navMod
	acts []NavAction
}

func (m *actMod) Actions() []NavAction { return m.acts }

func richItems() []NavItem {
	only := func(who string) func(string) bool { return func(l string) bool { return l == who } }
	return []NavItem{
		{Label: "Nodes", Path: "/", Order: 10, Group: "operate", Icon: "nodes", Source: "nodes"},
		{Label: "Tasks", Path: "/tasks", Order: 40, Group: "operate", Icon: "tasks", Source: "tasks", Allow: only("alice")},
		{Label: "Policy", Path: "/policy", Order: 50, Group: "configure", Icon: "policy", Source: "policy", Allow: only("alice")},
	}
}

type whoTailnet struct {
	Tailnet
	login string
	tags  []string
	err   error
}

func (w whoTailnet) WhoIs(context.Context, string) (string, []string, error) {
	return w.login, w.tags, w.err
}

func shellPage(t *testing.T, items []NavItem, actions []NavAction, tn Tailnet, path string) string {
	t.Helper()
	rec := do(NavMiddleware(items, actions, tn, htmlHandler(`<body><p>hi</p></body>`)), "GET", path)
	return rec.Body.String()
}

func TestShellInjectsSidebarTopBarAndAssets(t *testing.T) {
	body := shellPage(t, richItems(), []NavAction{{Label: "Enrol device", Path: "/enroll?enrol=1", Icon: "plus"}}, whoTailnet{login: "alice"}, "/tasks")
	theme := strings.Index(body, `<script src="/_ui/theme.js"></script>`)
	css := strings.Index(body, `<link rel="stylesheet" href="/_ui/hl.css">`)
	nav := strings.Index(body, `<nav data-nc-nav `)
	if !strings.HasPrefix(body, "<body>") || theme != len("<body>") || css <= theme || nav <= css {
		t.Fatalf("order: theme %d css %d nav %d: %s", theme, css, nav, body)
	}
	for _, want := range []string{
		`class="hl side"`, `role="status"`, `data-hl-loaded="`, `data-source="tasks"`, `class="navlabel">Operate<`, `class="navlabel">Configure<`,
		`<script src="/_ui/hl.js" defer></script><script src="/_ui/shell.js" defer></script><script src="/_ui/palette.js" defer></script><p>hi</p>`,
		`href="/enroll?enrol=1"`, `Enrol device`, `data-theme="system"`, `data-theme="light"`, `data-theme="dark"`,
		`href="/tasks" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{" style=", "<style", "onclick", "<script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("shell carries %q", bad)
		}
	}
}

func TestShellGroupsAndDropsEmptySections(t *testing.T) {
	items := []NavItem{
		{Label: "Nodes", Path: "/", Order: 10, Group: "operate", Icon: "nodes"},
		{Label: "Devices", Path: "/enroll", Order: 20, Group: "operate", Icon: "devices"},
	}
	body := shellPage(t, items, nil, whoTailnet{login: "alice"}, "/")
	if !strings.Contains(body, ">Operate<") || strings.Contains(body, ">Configure<") {
		t.Fatalf("headings: %s", body)
	}
}

func TestShellFiltersByLogin(t *testing.T) {
	cases := []struct {
		name string
		tn   Tailnet
		want []string
	}{
		{"allowed", whoTailnet{login: "alice"}, []string{"Nodes", "Tasks", "Policy"}},
		{"other login", whoTailnet{login: "bob"}, []string{"Nodes"}},
		{"whois error", whoTailnet{err: io.EOF}, []string{"Nodes"}},
		{"tagged", whoTailnet{login: "alice", tags: []string{"tag:x"}}, []string{"Nodes"}},
		{"no tailnet", nil, []string{"Nodes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NavMiddleware(richItems(), nil, c.tn, htmlHandler(`<body></body>`))
			page := do(h, "GET", "/").Body.String()
			var got []NavItem
			if err := json.Unmarshal(do(h, "GET", "/_nav.json").Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("json: %+v", got)
			}
			for i, l := range c.want {
				// A single visible entry gets the compact header, which lists no entries.
				if got[i].Label != l || (len(c.want) > 1 && !strings.Contains(page, "<span>"+l+"</span>")) {
					t.Errorf("want %s visible (json %+v)", l, got)
				}
			}
			for _, l := range []string{"Nodes", "Tasks", "Policy"} {
				hidden := !strings.Contains(strings.Join(c.want, ","), l)
				if hidden && strings.Contains(page, "<span>"+l+"</span>") {
					t.Errorf("%s leaked into the page", l)
				}
			}
		})
	}
	t.Run("json is an empty array, not null", func(t *testing.T) {
		only := []NavItem{{Label: "T", Path: "/t", Allow: func(string) bool { return true }}}
		if got := do(NavMiddleware(only, nil, nil, http.NotFoundHandler()), "GET", "/_nav.json").Body.String(); got != "[]\n" {
			t.Fatalf("%q", got)
		}
	})
}

func TestShellCompactWithOneItem(t *testing.T) {
	one := []NavItem{{Label: "Nodes", Path: "/", Order: 10, Group: "operate", Icon: "nodes"}}
	body := shellPage(t, one, nil, whoTailnet{login: "alice"}, "/")
	for _, want := range []string{`/_ui/theme.js`, `/_ui/hl.css`, `/_ui/shell.js`, `data-nc-nav`, `class="brand"`, `data-theme="dark"`, `role="status"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{"<nav", ">Operate<", ">Configure<", "navlabel", `aria-current`} {
		if strings.Contains(body, bad) {
			t.Errorf("compact shell has %q", bad)
		}
	}
	// A caller who may see only one of several entries also gets the compact form.
	if got := shellPage(t, richItems(), nil, whoTailnet{login: "bob"}, "/"); strings.Contains(got, "<nav") {
		t.Errorf("sidebar for a single visible entry")
	}
}

func TestShellOmitsActionWithoutProvider(t *testing.T) {
	if body := shellPage(t, richItems(), nil, whoTailnet{login: "alice"}, "/"); strings.Contains(body, "Enrol device") {
		t.Fatal("action without a provider")
	}
	mods := []Module{
		&actMod{navMod{stubModule{name: "a"}, richItems()}, []NavAction{{Label: "Enrol device", Path: "/enroll?enrol=1", Icon: "plus"}}},
		&navMod{stubModule{name: "b"}, nil},
	}
	acts := CollectActions(mods)
	if len(acts) != 1 || acts[0].Label != "Enrol device" {
		t.Fatalf("actions %+v", acts)
	}
	if got := CollectActions([]Module{&navMod{stubModule{name: "b"}, nil}}); len(got) != 0 {
		t.Fatalf("actions %+v", got)
	}
}

func TestShellCarriesServerLoadTime(t *testing.T) {
	now := time.Unix(1_800_000_123, 0)
	out := string(renderShell(richItems(), nil, "alice", "/", now))
	if !strings.Contains(out, `role="status" class="strip" hidden data-hl-loaded="1800000123"`) {
		t.Fatalf("load time missing: %s", out)
	}
}

func TestUIAssetsServedOnAdminOnly(t *testing.T) {
	h := NavMiddleware(richItems(), nil, nil, http.NotFoundHandler())
	for _, p := range []string{"/_ui/hl.css", "/_ui/hl.js", "/_ui/theme.js", "/_ui/shell.js", "/_ui/palette.js"} {
		if rec := do(h, "GET", p); rec.Code != 200 || rec.Body.Len() == 0 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	if rec := do(h, "GET", "/_ui/nothing.js"); rec.Code != 404 {
		t.Errorf("unknown asset = %d", rec.Code)
	}
	if rec := do(h, "POST", "/_ui/hl.css"); rec.Code != http.StatusNotFound {
		t.Errorf("POST asset = %d (passes to the app)", rec.Code)
	}
}

func TestShellEscapesLabels(t *testing.T) {
	items := []NavItem{{Label: `<script>"a"&'b'`, Path: "/x", Order: 1, Source: `s"><b>`}, {Label: "Two", Path: "/two", Order: 2}}
	body := shellPage(t, items, []NavAction{{Label: `<i>"x"`, Path: "/a?b=1&c=<2>", Icon: "plus"}}, nil, "/")
	if strings.Contains(body, "<script>\"a\"") || strings.Contains(body, "<i>") || strings.Contains(body, "<b>") {
		t.Fatalf("not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;&#34;a&#34;&amp;&#39;b&#39;") || !strings.Contains(body, `href="/a?b=1&amp;c=&lt;2&gt;"`) {
		t.Fatalf("unexpected escaping: %s", body)
	}
}

func TestShellOnlyOnSuccessfulGetHTML(t *testing.T) {
	h := NavMiddleware(richItems(), nil, nil, htmlHandler(`<body>x</body>`))
	if got := do(h, "POST", "/").Body.String(); got != "<body>x</body>" {
		t.Errorf("POST: %s", got)
	}
	nf := NavMiddleware(richItems(), nil, nil, http.NotFoundHandler())
	if got := do(nf, "GET", "/x").Body.String(); strings.Contains(got, "data-nc-nav") {
		t.Errorf("404: %s", got)
	}
}

func TestValidateNavGroupAndIcon(t *testing.T) {
	ok := NavItem{Label: "A", Path: "/a", Group: "operate", Icon: "nodes"}
	if err := ValidateNavItem(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []NavItem{{Label: "A", Path: "/a", Group: "x"}, {Label: "A", Path: "/a", Icon: "nope"}} {
		if ValidateNavItem(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

type countingTailnet struct {
	Tailnet
	n atomic.Int32
}

func (c *countingTailnet) WhoIs(context.Context, string) (string, []string, error) {
	c.n.Add(1)
	return "alice", nil, nil
}

func TestShellAsksWhoIsOnlyWhenItInjects(t *testing.T) {
	mk := func(ct string, status int, body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		})
	}
	cases := []struct {
		name, method string
		h            http.Handler
		want         int32
	}{
		{"POST", "POST", mk("text/html", 200, "<body></body>"), 0},
		{"404", "GET", mk("text/html", 404, "<body></body>"), 0},
		{"app JSON", "GET", mk("application/json", 200, "{}"), 0},
		{"non-HTML", "GET", mk("text/plain", 200, "x"), 0},
		{"asset", "GET", mk("text/html", 200, "<body></body>"), 0},
		{"GET page", "GET", mk("text/html", 200, "<body></body>"), 1},
		{"HEAD page", "HEAD", mk("text/html", 200, "<body></body>"), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tn := &countingTailnet{}
			path := "/"
			if c.name == "asset" {
				path = "/_ui/hl.css"
			}
			do(NavMiddleware(richItems(), nil, tn, c.h), c.method, path)
			if got := tn.n.Load(); got != c.want {
				t.Fatalf("WhoIs ran %d times, want %d", got, c.want)
			}
		})
	}
	// /_nav.json filters by caller, so it asks exactly once.
	tn := &countingTailnet{}
	do(NavMiddleware(richItems(), nil, tn, http.NotFoundHandler()), "GET", "/_nav.json")
	if tn.n.Load() != 1 {
		t.Fatalf("/_nav.json asked %d times", tn.n.Load())
	}
}

func TestShellHasTheSearchBoxExceptWhenCompact(t *testing.T) {
	acts := []NavAction{{Label: "Enrol device", Path: "/enroll?enrol=1", Icon: "plus"}, {Label: `A "quoted" <one>`, Path: "/x"}}
	out := string(renderShell(richItems(), acts, "alice", "/", time.Unix(1_800_000_123, 0)))
	box := `<button type="button" class="search" data-palette aria-haspopup="dialog" data-actions="`
	i := strings.Index(out, box)
	if i < 0 {
		t.Fatalf("no search box: %s", out)
	}
	for _, want := range []string{"Search or run a command", `<span class="kbd" data-palette-keys>⌘K</span>`, `<path d="M3 11a8 8 0 1 0 16 0`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if fresh := strings.Index(out, `<span class="fresh`); i > fresh || strings.Index(out, `data-top>`) > i {
		t.Errorf("the search box goes in the top bar, before the freshness text")
	}
	// the actions travel as JSON in an escaped attribute, so a hostile label cannot leave it
	start := i + len(box)
	attr := out[start : start+strings.Index(out[start:], `"`+`>`)]
	var got []struct{ Label, Path, Icon string }
	if err := json.Unmarshal([]byte(html.UnescapeString(attr)), &got); err != nil || len(got) != 2 || got[0].Label != "Enrol device" || got[0].Icon != "plus" || got[1].Label != `A "quoted" <one>` {
		t.Errorf("actions: %v %+v (%q)", err, got, attr)
	}
	if strings.Contains(attr, `<one>`) || strings.Contains(attr, `"quoted"`) {
		t.Errorf("the attribute is not escaped: %q", attr)
	}
	// with no action the top bar is still there, for the search box
	if none := string(renderShell(richItems(), nil, "alice", "/", time.Now())); strings.Contains(none, `data-top hidden`) || !strings.Contains(none, `data-actions="[]"`) {
		t.Error("the top bar must stay visible without actions")
	}
}

func TestCompactShellHasNoSearchBox(t *testing.T) {
	one := []NavItem{{Label: "Nodes", Path: "/", Order: 10, Group: "operate", Icon: "nodes"}}
	body := shellPage(t, one, []NavAction{{Label: "Enrol device", Path: "/enroll", Icon: "plus"}}, whoTailnet{login: "alice"}, "/")
	for _, bad := range []string{"data-palette", "Search or run a command", "kbd"} {
		if strings.Contains(body, bad) {
			t.Errorf("compact shell has %q", bad)
		}
	}
	if !strings.Contains(body, `<script src="/_ui/palette.js" defer></script>`) {
		t.Error("the palette script is part of every shell")
	}
}
