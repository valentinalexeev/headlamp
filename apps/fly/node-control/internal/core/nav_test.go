package core

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
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

const navStyleGolden = `<style>` + navCSS + `</style>`

func TestNavMiddlewareInjectsAfterBody(t *testing.T) {
	h := NavMiddleware(twoItems(), htmlHandler(`<html><head></head><BODY class="x" data-a='>'><p>hi</p></BODY></html>`))
	rec := do(h, "GET", "/")
	want := `<html><head></head><BODY class="x" data-a='>'>` + navStyleGolden +
		`<nav data-nc-nav aria-label="Modules"><ul>` +
		`<li><a href="/" aria-current="page">Nodes</a></li>` +
		`<li><a href="/enroll">Enrolment</a></li>` +
		`</ul></nav><div data-nc-nav-spacer aria-hidden="true" style="height:36px"></div><p>hi</p></BODY></html>`
	if rec.Body.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", rec.Body.String(), want)
	}
	for _, line := range strings.Split(navCSS, "}") {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "[data-nc-nav]") {
			t.Errorf("unscoped CSS rule %q", line)
		}
	}
}

func TestNavMiddlewareHidesBarWithOneItem(t *testing.T) {
	const page = `<body><p>hi</p></body>`
	for _, items := range [][]NavItem{nil, {{Label: "Nodes", Path: "/"}}} {
		rec := do(NavMiddleware(items, htmlHandler(page)), "GET", "/")
		if rec.Body.String() != page {
			t.Fatalf("%d items: %q", len(items), rec.Body.String())
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
			rec := do(NavMiddleware(twoItems(), c.h), c.method, "/")
			if strings.Contains(rec.Body.String(), "data-nc-nav") {
				t.Fatalf("bar injected: %q", rec.Body.String())
			}
		})
	}
	t.Run("status and location kept", func(t *testing.T) {
		rec := do(NavMiddleware(twoItems(), mk(302, "text/html", "/x", page)), "GET", "/")
		if rec.Code != 302 || rec.Header().Get("Location") != "/x" || rec.Body.String() != page {
			t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
		}
	})
}

func TestNavMiddlewareNoBodyTag(t *testing.T) {
	const frag = `<p>fragment, a <bodyguard> mention</p>`
	rec := do(NavMiddleware(twoItems(), htmlHandler(frag)), "GET", "/")
	if rec.Body.String() != frag {
		t.Fatalf("got %q", rec.Body.String())
	}
}

func TestNavMiddlewareContentLengthMatches(t *testing.T) {
	const page = `<html><body><p>hi</p></body></html>`
	h := NavMiddleware(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(NavMiddleware(twoItems(), htmlHandler(page)))
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
		rec := do(NavMiddleware(twoItems(), http.NotFoundHandler()), "HEAD", "/_nav.json")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
			t.Fatalf("%d %v %q", rec.Code, rec.Header(), rec.Body.String())
		}
	})
}

func TestNavMiddlewareNoDoubleInjection(t *testing.T) {
	h := NavMiddleware(twoItems(), htmlHandler(`<body>x</body>`))
	outer := NavMiddleware(twoItems(), h)
	if n := strings.Count(do(outer, "GET", "/").Body.String(), "<nav data-nc-nav"); n != 1 {
		t.Fatalf("%d bars", n)
	}
}

func TestNavActiveItem(t *testing.T) {
	h := NavMiddleware(twoItems(), htmlHandler(`<body></body>`))
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
			if c.want != "" && !strings.Contains(body, `aria-current="page">`+c.want+`</a>`) {
				t.Fatalf("%s not active: %s", c.want, body)
			}
		})
	}
}

func TestNavActiveItemLongestWins(t *testing.T) {
	items := []NavItem{{Label: "A", Path: "/a", Order: 1}, {Label: "AB", Path: "/a/b", Order: 2}, {Label: "Root", Path: "/", Order: 0}}
	h := NavMiddleware(items, htmlHandler(`<body></body>`))
	for path, want := range map[string]string{"/a": "A", "/a/b": "AB", "/a/b/c": "AB", "/a/x": "A", "/": "Root", "/zzz": ""} {
		t.Run(path, func(t *testing.T) {
			body := do(h, "GET", path).Body.String()
			n := strings.Count(body, `aria-current="page"`)
			if want == "" && n != 0 || want != "" && (n != 1 || !strings.Contains(body, `aria-current="page">`+want+`</a>`)) {
				t.Fatalf("%d current, want %q: %s", n, want, body[strings.Index(body, "<ul>"):])
			}
		})
	}
}

func TestNavJSON(t *testing.T) {
	for _, items := range [][]NavItem{twoItems(), nil} {
		rec := do(NavMiddleware(items, http.NotFoundHandler()), "GET", "/_nav.json")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%d %v", rec.Code, rec.Header())
		}
		var got []NavItem
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != len(items) {
			t.Fatalf("%v %q", err, rec.Body.String())
		}
	}
	t.Run("shape", func(t *testing.T) {
		rec := do(NavMiddleware(twoItems(), http.NotFoundHandler()), "GET", "/_nav.json")
		if !strings.HasPrefix(rec.Body.String(), `[{"label":"Nodes","path":"/","order":10},`) {
			t.Fatal(rec.Body.String())
		}
	})
	t.Run("not injected", func(t *testing.T) {
		rec := do(NavMiddleware(twoItems(), htmlHandler(`<body>`)), "GET", "/_nav.json")
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
	rec := do(NavMiddleware(items, htmlHandler(`<body></body>`)), "GET", "/")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, `"a"`) {
		t.Fatalf("label not escaped: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;&#34;a&#34;&amp;&#39;b&#39;") {
		t.Fatalf("unexpected escaping: %s", body)
	}
	j := do(NavMiddleware(items, http.NotFoundHandler()), "GET", "/_nav.json").Body.String()
	if strings.Contains(j, "<script>") {
		t.Fatalf("json not escaped: %s", j)
	}
	t.Run("renderer escapes a path even if validation is bypassed", func(t *testing.T) {
		out := string(renderNav([]NavItem{{Label: "X", Path: "/x?a=1&b=<2>"}}, "/"))
		if !strings.Contains(out, `href="/x?a=1&amp;b=&lt;2&gt;"`) {
			t.Fatal(out)
		}
	})
}

func TestNavMiddlewareFlushAndStreaming(t *testing.T) {
	const page = `<html><body><p>hi</p></body></html>`
	wire := func(t *testing.T, h http.Handler, method string) (*http.Response, []byte) {
		t.Helper()
		srv := httptest.NewServer(NavMiddleware(twoItems(), h))
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
		srv := httptest.NewServer(NavMiddleware(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		h := NavMiddleware(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		h := NavMiddleware(twoItems(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(NavMiddleware(twoItems(), h))
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
	body := do(NavMiddleware(twoItems(), htmlHandler(page)), "GET", "/").Body.String()
	i := strings.Index(body, `<body class="real">`)
	if i < 0 || !strings.HasPrefix(body[i+len(`<body class="real">`):], "<style>[data-nc-nav]") {
		t.Fatalf("bar not right after the real body tag: %s", body)
	}
	if strings.Count(body, "<nav data-nc-nav") != 1 {
		t.Fatal("injected more than once")
	}
	t.Run("only decoys: untouched", func(t *testing.T) {
		const decoy = `<html><head><!-- <body> --></head></html>`
		if got := do(NavMiddleware(twoItems(), htmlHandler(decoy)), "GET", "/").Body.String(); got != decoy {
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
