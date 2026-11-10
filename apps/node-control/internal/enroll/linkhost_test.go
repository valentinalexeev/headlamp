package enroll

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #270: a bridge front whose host is not link_host proxies /me, /oidc/login and /i/<token> with its own Host. The
// OIDC redirect_uri and the host-only cookies are on link_host, so these pages send the browser there first.
func TestSelfPagesOnAnotherFrontGoToLinkHost(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	link := f.link.LinkHost
	hex := strings.Repeat("ab", 32)
	h := f.srv.Config.Handler
	for _, p := range []string{"/me", "/oidc/login", "/i/" + hex} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Host = "bridge1.example.test"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "https://"+link+p {
			t.Errorf("%s on another front: %d %q, want a redirect to https://%s%s", p, w.Code, w.Header().Get("Location"), link, p)
		}
		if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("%s on another front set cookies on the wrong host: %v", p, c)
		}
	}
	// The same front with link_host's name (also when it differs in case) serves the page.
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Host = strings.ToUpper(link)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign in") {
		t.Fatalf("/me on link_host: %d", w.Code)
	}
	// The whole sign-in, started on another front: the browser follows the redirect to link_host and signs in
	// (the 400 of #270 was here: the flow cookie was on the front's host).
	c := f.browser()
	c.Transport = frontAs(c.Transport, "bridge1.example.test", strings.TrimPrefix(f.srv.URL, "https://"))
	resp, err := c.Get("https://bridge1.example.test/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	if b := readAll(resp); resp.StatusCode != 200 || !strings.Contains(b, "Signed in as") {
		t.Fatalf("sign-in from another front: %d %s", resp.StatusCode, b)
	}
}

func TestSameHost(t *testing.T) {
	for _, c := range []struct {
		req, link string
		want      bool
	}{
		{"enroll.example.com", "enroll.example.com", true},
		{"Enroll.Example.com", "enroll.example.com", true},
		{"enroll.example.com:443", "enroll.example.com", true},
		{"127.0.0.1:5000", "127.0.0.1:5000", true},
		{"127.0.0.1:5001", "127.0.0.1:5000", false},
		{"bridge1.example.com", "enroll.example.com", false},
	} {
		if got := sameHost(c.req, c.link); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.req, c.link, got, c.want)
		}
	}
}

// frontAs is a transport that sends requests for the front's name to the test server, keeping the Host header:
// what a bridge front does for node-control.
type frontRT struct {
	base        http.RoundTripper
	front, addr string
}

func frontAs(base http.RoundTripper, front, addr string) http.RoundTripper {
	return &frontRT{base: base, front: front, addr: addr}
}

func (f *frontRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == f.front {
		r = r.Clone(r.Context())
		r.URL.Host = f.addr
		r.Host = f.front
	}
	return f.base.RoundTrip(r)
}
