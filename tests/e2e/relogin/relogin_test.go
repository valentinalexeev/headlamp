// Package relogin tests what a second login of a tagged node changes on the pinned headscale (HEADSCALE_VERSION) and
// tailscale (TAILSCALE_VERSION), for the re-key playbook (#398, #399). It covers four cases:
//
//  1. tailscale up with a new one-time key and no --force-reauth on a node that is logged in;
//  2. tailscale up --force-reauth with a new key from a file;
//  3. the re-login fails (expired key);
//  4. an admin expires the node (headscale nodes expire).
//
// It needs Docker; without it the test is skipped. Run: cd tests/e2e && go test ./relogin -v
package relogin

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	hsConfig = "/etc/headscale/config.yaml"
	tag      = "tag:exit"
	host     = "relogin-e2e"
)

type env struct {
	t     *testing.T
	id    string
	hs    string
	node  string
	login string
	user  string
}

func run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String() + errb.String()), fmt.Errorf("docker %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func (e *env) must(args ...string) string {
	e.t.Helper()
	out, err := run(args...)
	if err != nil {
		e.t.Fatalf("%v: %s", err, out)
	}
	return out
}

func (e *env) hsx(args ...string) string {
	e.t.Helper()
	return e.must(append([]string{"exec", e.hs, "headscale", "-c", hsConfig}, args...)...)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func pin(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(), name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func waitFor(d time.Duration, f func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		if f() {
			return true
		}
	}
	return f()
}

// key mints a tagged one-time key that expires after exp.
func (e *env) key(exp string) string {
	e.t.Helper()
	out := e.hsx("preauthkeys", "create", "-u", e.user, "--tags", tag, "-e", exp, "-o", "json")
	m := regexp.MustCompile(`"key":\s*"([^"]+)"`).FindStringSubmatch(out)
	if m == nil {
		e.t.Fatal("no key in the headscale output")
	}
	return m[1]
}

// keyFile puts key into a 0600 file in the node container and returns its path, so the key is not on a command line.
func (e *env) keyFile(key string) string {
	e.t.Helper()
	cmd := exec.Command("docker", "exec", "-i", e.node, "sh", "-c", "umask 077; cat > /run/k")
	cmd.Stdin = strings.NewReader(key)
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("key file: %v: %s", err, out)
	}
	return "/run/k"
}

// up runs tailscale up on the node with extra flags. It returns the output and the error, with a time limit. The
// tag comes from the key, as with the keys that node-control mints, so the node does not advertise it.
func (e *env) up(extra ...string) (string, error) {
	args := append([]string{"exec", e.node, "tailscale", "up", "--timeout=30s", "--login-server=" + e.login,
		"--hostname=" + host}, extra...)
	return run(args...)
}

type record struct {
	ID      string
	Name    string
	IPs     []string
	Tags    []string
	NodeKey string
	Online  bool
}

// nodes returns the node records of headscale.
func (e *env) nodes() []record {
	e.t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(e.hsx("nodes", "list", "-o", "json")), &list); err != nil {
		e.t.Fatalf("nodes list: %v", err)
	}
	out := make([]record, 0, len(list))
	for _, n := range list {
		r := record{ID: fmt.Sprint(n["id"]), Name: str(n["given_name"]), NodeKey: str(n["node_key"]),
			Online: n["online"] == true, IPs: list2(n["ip_addresses"])}
		r.Tags = append(list2(n["tags"]), list2(n["forced_tags"])...)
		slices.Sort(r.Tags)
		r.Tags = slices.Compact(r.Tags)
		out = append(out, r)
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list2(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// one returns the only node record and fails when there is not exactly one.
func (e *env) one() record {
	e.t.Helper()
	n := e.nodes()
	if len(n) != 1 {
		e.t.Fatalf("want one node, got %d: %+v", len(n), n)
	}
	return n[0]
}

// state returns the backend state of tailscaled on the node (Running, NeedsLogin, ...).
func (e *env) state() string {
	out, _ := run("exec", e.node, "tailscale", "status", "--json")
	var s struct{ BackendState string }
	_ = json.Unmarshal([]byte(out), &s)
	return s.BackendState
}

// writeCert writes a self-signed RSA certificate for name to dir/tls.crt and its key to dir/tls.key (readable by the
// headscale container). The node trusts the certificate through SSL_CERT_FILE.
func writeCert(t *testing.T, dir, name string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pk := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), crt, 0o644); err != nil {
		t.Fatal(err)
	}
	// 0644: the test key of a throwaway container, read by the headscale user in the container.
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pk, 0o644); err != nil { //nolint:gosec // test key
		t.Fatal(err)
	}
}

// same reports whether b keeps the identity of a: id, name, addresses and tags.
func same(a, b record) bool {
	return a.ID == b.ID && a.Name == b.Name && slices.Equal(a.IPs, b.IPs) && slices.Equal(a.Tags, b.Tags)
}

func TestTaggedNodeSecondLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	hsv, tsv := pin(t, "HEADSCALE_VERSION"), pin(t, "TAILSCALE_VERSION")
	id := fmt.Sprintf("relogin-%d", time.Now().UnixNano()%1e6)
	e := &env{t: t, id: id, hs: id + "-hs", node: id + "-node"}
	// Headscale serves TLS on 443, as in production: a re-registration (--force-reauth) dials port 443 with TLS
	// even when the login server is plain HTTP, so a plain-HTTP harness cannot test it.
	e.login = "https://" + e.hs
	net := id + "-net"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", e.hs, e.node).Run()
		_ = exec.Command("docker", "network", "rm", net).Run()
		_ = exec.Command("docker", "image", "rm", "-f", id+"-ts").Run()
	})

	// The node: the pinned tailscale on Alpine, userspace networking, for the host architecture.
	dir := t.TempDir()
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(`FROM alpine:3.24
ARG V
ARG A
RUN wget -q https://pkgs.tailscale.com/stable/tailscale_${V}_${A}.tgz && tar xzf tailscale_${V}_${A}.tgz && \
  cp tailscale_${V}_${A}/tailscale tailscale_${V}_${A}/tailscaled /usr/local/bin/ && rm -rf tailscale_*
CMD ["tailscaled", "--tun=userspace-networking", "--state=/var/lib/tailscale/tailscaled.state"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must("build", "-q", "-t", id+"-ts", "--build-arg", "V="+tsv, "--build-arg", "A="+arch, dir)

	writeCert(t, dir, e.hs)

	// Headscale with a policy that lets the user own the tag.
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"tagOwners":{"`+tag+`":["e2e@"]},
"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(`server_url: `+e.login+`
listen_addr: 0.0.0.0:443
tls_cert_path: /etc/headscale/tls.crt
tls_key_path: /etc/headscale/tls.key
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
noise:
  private_key_path: /data/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: true
    region_id: 999
    region_code: e2e
    region_name: e2e
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /data/derp_server_private.key
    automatically_add_embedded_derp_region: true
  urls: []
  auto_update_enabled: false
database:
  type: sqlite
  sqlite:
    path: /data/db.sqlite
dns:
  magic_dns: false
  override_local_dns: false
  nameservers:
    global: []
policy:
  mode: file
  path: /etc/headscale/policy.json
unix_socket: /data/headscale.sock
log:
  level: info
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must("network", "create", net)
	e.must("run", "-d", "--name", e.hs, "--network", net, "-v", dir+":/etc/headscale:ro",
		"ghcr.io/juanfont/headscale:"+hsv, "serve", "-c", hsConfig)
	if !waitFor(40*time.Second, func() bool {
		_, err := run("exec", e.hs, "headscale", "-c", hsConfig, "users", "list")
		return err == nil
	}) {
		out, _ := run("logs", e.hs)
		t.Fatalf("headscale did not start:\n%s", out)
	}
	e.hsx("users", "create", "e2e")
	e.user = regexp.MustCompile(`"id":\s*(\d+)`).FindStringSubmatch(e.hsx("users", "list", "-o", "json"))[1]
	e.must("run", "-d", "--name", e.node, "--network", net, "-e", "SSL_CERT_FILE=/etc/headscale/tls.crt",
		"-v", filepath.Join(dir, "tls.crt")+":/etc/headscale/tls.crt:ro", id+"-ts")

	// The first login, with a tagged one-time key.
	if !waitFor(20*time.Second, func() bool { return e.state() != "" }) {
		t.Fatal("tailscaled did not start")
	}
	if out, err := e.up("--auth-key=file:" + e.keyFile(e.key("1h"))); err != nil {
		t.Fatalf("first login: %v: %s", err, out)
	}
	first := e.one()
	if !slices.Equal(first.Tags, []string{tag}) {
		t.Fatalf("the node must carry %s, got %v", tag, first.Tags)
	}
	t.Logf("first login: %+v", first)

	t.Run("1 new key without force-reauth", func(t *testing.T) {
		e.t = t
		out, err := e.up("--auth-key=file:" + e.keyFile(e.key("1h")))
		got := e.one()
		t.Logf("result: err=%v out=%q record=%+v", err, out, got)
		if err != nil {
			t.Fatalf("tailscale up on a logged in node failed: %v: %s", err, out)
		}
		if got.NodeKey != first.NodeKey {
			t.Fatalf("the node key changed without --force-reauth: %s -> %s", first.NodeKey, got.NodeKey)
		}
		if !same(first, got) {
			t.Fatalf("the record changed: %+v -> %+v", first, got)
		}
		if e.state() != "Running" {
			t.Fatalf("state %q", e.state())
		}
	})

	var second record
	t.Run("2 force-reauth with a new key", func(t *testing.T) {
		e.t = t
		out, err := e.up("--force-reauth", "--auth-key=file:"+e.keyFile(e.key("1h")))
		second = e.one()
		t.Logf("result: err=%v out=%q record=%+v", err, out, second)
		if err != nil {
			t.Fatalf("force-reauth failed: %v: %s", err, out)
		}
		if !same(first, second) {
			t.Fatalf("headscale did not keep the node record: %+v -> %+v", first, second)
		}
		if second.NodeKey == first.NodeKey {
			t.Fatalf("the node key did not change with --force-reauth (%s)", second.NodeKey)
		}
		if !waitFor(20*time.Second, func() bool { return e.state() == "Running" }) {
			t.Fatalf("state %q", e.state())
		}
	})

	// A failed re-login (here an expired key) leaves the node as it was: the old node key and login stay in use, the
	// node keeps running, also after a restart. The playbook can retry with a new key.
	t.Run("3 re-login with an expired key", func(t *testing.T) {
		e.t = t
		k := e.key("2s")
		time.Sleep(4 * time.Second)
		out, err := e.up("--force-reauth", "--auth-key=file:"+e.keyFile(k))
		got := e.one()
		t.Logf("result: err=%v out=%q state=%s record=%+v", err, out, e.state(), got)
		if err == nil || !strings.Contains(out, "expired") {
			t.Fatalf("tailscale up with an expired key must fail and say so: %v: %s", err, out)
		}
		if !same(second, got) || got.NodeKey != second.NodeKey {
			t.Fatalf("the failed login changed the record: %+v -> %+v", second, got)
		}
		if st := e.state(); st != "Running" {
			t.Fatalf("after a failed re-login the old login must stay in use, state %s", st)
		}
		e.must("restart", e.node)
		if !waitFor(30*time.Second, func() bool { return e.state() == "Running" }) {
			t.Fatalf("after a restart the old state must still work, state %s", e.state())
		}
		if !waitFor(30*time.Second, func() bool { return e.one().Online }) {
			t.Fatal("headscale does not see the node online with its old state")
		}
		if got := e.one(); got.NodeKey != second.NodeKey {
			t.Fatalf("the node key changed after a restart: %s -> %s", second.NodeKey, got.NodeKey)
		}
	})

	// An admin expiry logs the node out at once. Neither tailscale up without a key nor a restart brings it back;
	// a new one-time key does, as the same record (id, name, addresses, tags).
	t.Run("4 admin expires the node", func(t *testing.T) {
		e.t = t
		e.hsx("nodes", "expire", "-i", second.ID, "--force")
		if !waitFor(30*time.Second, func() bool { return e.state() == "NeedsLogin" }) {
			t.Fatalf("an expired node must need a login, state %q", e.state())
		}
		out, err := run("exec", e.node, "tailscale", "up", "--timeout=10s", "--login-server="+e.login,
			"--hostname="+host)
		t.Logf("up without a key: err=%v out=%q", err, out)
		if err == nil || e.state() == "Running" {
			t.Fatalf("an expired node came back without a key (state %s)", e.state())
		}
		e.must("restart", e.node)
		if !waitFor(30*time.Second, func() bool { return e.state() != "" }) {
			t.Fatal("tailscaled did not restart")
		}
		time.Sleep(5 * time.Second)
		if st := e.state(); st != "NeedsLogin" {
			t.Fatalf("after a restart an expired node must still need a login, state %s", st)
		}
		out, err = e.up("--auth-key=file:" + e.keyFile(e.key("1h")))
		got := e.one()
		t.Logf("new key: err=%v out=%q record=%+v", err, out, got)
		if err != nil || !waitFor(20*time.Second, func() bool { return e.state() == "Running" }) {
			t.Fatalf("a new key did not bring the expired node back: %v: %s", err, out)
		}
		if !same(second, got) {
			t.Fatalf("the expired node came back as a new record: %+v -> %+v", second, got)
		}
		// Observed on the pins: the node key stays the same. Only logged, because the playbook does not depend on it.
		if got.NodeKey != second.NodeKey {
			t.Logf("note: the node key changed on the login after an expiry: %s -> %s", second.NodeKey, got.NodeKey)
		}
	})
}
