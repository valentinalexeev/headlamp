package enroll

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// #260: the public paths of this listener are proxied by the edge of a site through one shared Caddy file. Every
// path that the public mux serves must be matched by it, and every matched path must be served here. When this
// test fails, change ansible/roles/reality-ingress/files/enroll-paths.caddy, and copy it to
// examples/compose/control/caddy/enroll-paths.caddy (scripts/check-enroll-paths.sh compares the copies).

const sharedPaths = "../../../../ansible/roles/reality-ingress/files/enroll-paths.caddy"

// edgeMatchers reads the path_regexp matchers of the shared file.
func edgeMatchers(t *testing.T) []*regexp.Regexp {
	t.Helper()
	b, err := os.ReadFile(sharedPaths)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?m)^@[a-z]+ path_regexp [a-z]+ `([^`]+)`$")
	var out []*regexp.Regexp
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out = append(out, regexp.MustCompile(m[1]))
	}
	if len(out) != 2 {
		t.Fatalf("want the @enroll and @self matchers in %s, found %d", sharedPaths, len(out))
	}
	return out
}

func edgeProxies(ms []*regexp.Regexp, p string) bool {
	for _, m := range ms {
		if m.MatchString(p) {
			return true
		}
	}
	return false
}

func TestEdgePathsMatchThePublicRoutes(t *testing.T) {
	ms := edgeMatchers(t)
	hex := strings.Repeat("ab", 32)
	// Every route of the public listener, with a valid example path. The edge must proxy each of them.
	served := []string{"/e/tok_1", "/sub/abc", "/me", "/oidc/login", "/oidc/callback", "/me/devices", "/me/devices/12",
		"/me/devices/12/revoke", "/me/devices/12/extend", "/me/devices/12/app/sing-box", "/me/logout", "/me/icons/sing-box",
		"/i/" + hex,
		"/s/style.css", "/s/fonts/ibm-plex-sans-latin-400-normal.woff2"}
	for _, format := range []string{FormatSingBox, FormatClash, FormatStash, FormatURIList} {
		served = append(served, "/sub/abc/"+format)
	}
	for _, p := range served {
		if !edgeProxies(ms, p) {
			t.Errorf("node-control serves %s, but the edge does not proxy it", p)
		}
	}
	// Every route in selfweb.go must have an example above: a new route fails here until the shared file and this
	// list cover it.
	src, err := os.ReadFile("selfweb.go")
	if err != nil {
		t.Fatal(err)
	}
	example := strings.NewReplacer("{id}", "12", "{app}", "sing-box", "{token}", hex)
	routes := regexp.MustCompile(`mux\.HandleFunc\("(GET|POST) (/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 9 {
		t.Fatalf("found %d routes in selfweb.go; the pattern of this test is out of date", len(routes))
	}
	for _, r := range routes {
		p := example.Replace(r[2])
		found := false
		for _, s := range served {
			found = found || s == p
		}
		if !found {
			t.Errorf("selfweb.go serves %s %s, which this test does not list: add it, and check the shared file", r[1], r[2])
		}
	}
	// Paths that are not served must not be proxied.
	for _, p := range []string{"/", "/me/", "/admin", "/enroll", "/e/..", "/sub/abc/bad", "/sub/a/b/c", "/i/short",
		"/me/devices/x", "/me/devices/1/app/", "/me/devices/1/app/a/b", "/oidc/other", "/metrics", "/s/", "/s/x.css", "/s/fonts/a.ttf", "/s/fonts/../x.woff2", "/s/fonts/a/b.woff2"} {
		if edgeProxies(ms, p) {
			t.Errorf("the edge proxies %s, which node-control does not serve", p)
		}
	}
}
