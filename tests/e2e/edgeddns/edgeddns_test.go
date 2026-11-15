// Package edgeddns tests the funnel record kept by the compose edge (#384): Caddy with the
// dynamic_dns plugin and the rfc2136 provider, run with the real caddy/ddns/rfc2136.caddy snippet,
// against BIND. A stub serves the edge's "public address". It needs Docker; without it the test is
// skipped. Run: cd tests/e2e && go test ./edgeddns -run TestEdgeKeepsFunnelRecord -v
package edgeddns

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// caddyDynamicDNS is the plugin, pinned by commit: it has no release tags.
	caddyDynamicDNS = "github.com/mholt/caddy-dynamicdns@67d107a42c02"
	caddyRFC2136    = "github.com/caddy-dns/rfc2136@v1.0.0"
	zone            = "f.example.test"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestEdgeKeepsFunnelRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	root := repoRoot(t)
	snippet := filepath.Join(root, "examples/compose/control/caddy/ddns/rfc2136.caddy")
	if _, err := os.Stat(snippet); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	id := fmt.Sprintf("ddns-e2e-%d", time.Now().UnixNano()%1e6)
	sub := "10.231.9"
	net := id + "-net"

	// Images: Caddy with the two plugins, BIND from Alpine (every architecture).
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.caddy"), []byte(
		"FROM caddy:2.10-builder AS b\nRUN xcaddy build --output /out/caddy --with "+caddyDynamicDNS+" --with "+caddyRFC2136+
			"\nFROM caddy:2.10\nCOPY --from=b /out/caddy /usr/bin/caddy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	docker(t, "build", "-q", "-t", id+"-caddy", "-f", filepath.Join(dir, "Dockerfile.caddy"), dir)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", id+"-caddy", id+"-bind").Run() })
	buildBind(t, id)

	docker(t, "network", "create", "--subnet", sub+".0/24", net)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", id+"-dns", id+"-ip", id+"-edge").Run()
		_ = exec.Command("docker", "network", "rm", net).Run()
	})

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	tsig := base64.StdEncoding.EncodeToString(key)
	dnsDir := filepath.Join(dir, "dns")
	_ = os.MkdirAll(dnsDir, 0o755)
	write(t, filepath.Join(dnsDir, "named.conf"), fmt.Sprintf(`options { directory "/var/cache/bind"; listen-on { any; }; listen-on-v6 { none; }; recursion no; allow-query { any; }; dnssec-validation no; };
key "e2e-key." { algorithm hmac-sha256; secret "%s"; };
zone "%s" { type primary; file "/var/lib/bind/zone.db"; update-policy { grant e2e-key. zonesub ANY; }; allow-transfer { key e2e-key.; }; };
`, tsig, zone))
	write(t, filepath.Join(dnsDir, "zone.db"), fmt.Sprintf("$TTL 60\n@ IN SOA ns.%[1]s. hostmaster.%[1]s. 1 60 60 600 60\n@ IN NS ns.%[1]s.\nns IN A %[2]s.2\nwww IN A 192.0.2.99\n", zone, sub))
	docker(t, "run", "-d", "--name", id+"-dns", "--network", net, "--ip", sub+".2", "-v", dnsDir+":/zone:ro", id+"-bind",
		"sh", "-c", "mkdir -p /var/lib/bind /var/cache/bind /etc/bind && cp /zone/named.conf /etc/bind/named.conf && "+
			"cp /zone/zone.db /var/lib/bind/zone.db && chown -R named /var/lib/bind /var/cache/bind && "+
			"exec named -u named -g -c /etc/bind/named.conf")

	// The "public address": 198.51.100.7, then 198.51.100.8 once /tmp/next exists.
	docker(t, "run", "-d", "--name", id+"-ip", "--network", net, "--ip", sub+".3", "python:3.13-alpine", "python", "-c",
		`import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ip = "198.51.100.8" if os.path.exists("/tmp/next") else "198.51.100.7"
        self.send_response(200); self.end_headers(); self.wfile.write(ip.encode())
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 80), H).serve_forever()`)

	// The edge: the real snippet, imported from a global block, as in the compose Caddyfile.
	write(t, filepath.Join(dir, "Caddyfile"), "{\n\tadmin off\n\timport ddns/rfc2136.caddy\n}\n:8080 {\n\trespond ok\n}\n")
	docker(t, "run", "-d", "--name", id+"-edge", "--network", net,
		"-v", filepath.Join(dir, "Caddyfile")+":/etc/caddy/Caddyfile:ro",
		"-v", filepath.Dir(snippet)+":/etc/caddy/ddns:ro",
		"-e", "FUNNEL_DOMAIN="+zone, "-e", "FUNNEL_DDNS_IP_URL=http://"+sub+".3/", "-e", "FUNNEL_DDNS_INTERVAL=2s",
		"-e", "RFC2136_KEY_NAME=e2e-key.", "-e", "RFC2136_KEY_ALG=hmac-sha256", "-e", "RFC2136_KEY="+tsig,
		"-e", "RFC2136_SERVER="+sub+".2:53", id+"-caddy")

	q := func(name, typ string) string {
		out, _ := exec.Command("docker", "exec", id+"-dns", "dig", "+short", "@127.0.0.1", name, typ).Output()
		return strings.TrimSpace(string(out))
	}
	wait := func(name, want string) bool {
		for range 60 {
			if q(name, "A") == want {
				return true
			}
			time.Sleep(time.Second)
		}
		return false
	}
	logs := func() string {
		out, _ := exec.Command("docker", "logs", id+"-edge").CombinedOutput()
		return string(out)
	}

	if !wait("*."+zone, "198.51.100.7") {
		t.Fatalf("the wildcard was not created: %q\n%s", q("*."+zone, "A"), logs())
	}
	if got := q("svc."+zone, "A"); got != "198.51.100.7" {
		t.Fatalf("a name under the wildcard: %q", got)
	}
	serial := func() string {
		f := strings.Fields(q(zone, "SOA"))
		if len(f) > 2 {
			return f[2]
		}
		return ""
	}
	s1 := serial()
	time.Sleep(8 * time.Second)
	if s2 := serial(); s1 != s2 {
		t.Fatalf("no needless writes: the zone serial changed %s -> %s with no address change", s1, s2)
	}
	docker(t, "exec", id+"-ip", "touch", "/tmp/next")
	if !wait("*."+zone, "198.51.100.8") {
		t.Fatalf("the record did not follow the address: %q\n%s", q("*."+zone, "A"), logs())
	}
	if got := q("www."+zone, "A"); got != "192.0.2.99" {
		t.Fatalf("another record of the zone changed: %q", got)
	}
	if strings.Contains(logs(), "zone transfer") {
		t.Fatalf("with allow-transfer for the key, the edge must read the zone without errors:\n%s", logs())
	}
}

func buildBind(t *testing.T, id string) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-q", "-t", id+"-bind", "-")
	cmd.Stdin = strings.NewReader("FROM alpine:3.22\nRUN apk add --no-cache bind bind-tools\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bind image: %v\n%s", err, out)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
