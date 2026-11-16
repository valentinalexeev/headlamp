// Package exitnode tests the login of the exit-node image (#394) against a real headscale: the node logs in once
// with a one-time key, comes back after a restart with its state and no key, and stops with a clear message (no
// login loop) when the key is used and the state is gone. It needs Docker; without it the test is skipped. The
// image is built for the host architecture. Run: cd tests/e2e && go test ./exitnode -v
package exitnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const hsImage = "ghcr.io/juanfont/headscale:v0.29.7"

type env struct {
	t   *testing.T
	id  string
	net string
	hs  string
	dir string
}

func (e *env) docker(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("docker %s: %v: %s", args[0], err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func (e *env) must(args ...string) string {
	e.t.Helper()
	out, err := e.docker(args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) hsx(args ...string) string {
	e.t.Helper()
	return e.must(append([]string{"exec", e.hs, "headscale", "-c", "/etc/headscale/config.yaml"}, args...)...)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func waitFor(d time.Duration, f func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		if f() {
			return true
		}
	}
	return false
}

// key mints a pre-auth key: one-time unless reusable is set.
func (e *env) key(user string, reusable bool) string {
	e.t.Helper()
	args := []string{"preauthkeys", "create", "-u", user, "-e", "1h", "-o", "json"}
	if reusable {
		args = append(args, "--reusable")
	}
	m := regexp.MustCompile(`"key":\s*"([^"]+)"`).FindStringSubmatch(e.hsx(args...))
	if m == nil {
		e.t.Fatal("no key in headscale output")
	}
	return m[1]
}

// start runs the exit node with the state volume and an optional key; it returns the container name.
func (e *env) start(name, key string) string {
	e.t.Helper()
	c := e.id + "-" + name
	args := []string{"run", "-d", "--name", c, "--network", e.net, "--cap-add", "NET_ADMIN", "--device", "/dev/net/tun",
		"--sysctl", "net.ipv4.ip_forward=1", "-v", e.id + "-state:/var/lib/tailscale",
		"-e", "HEADSCALE_LOGIN_SERVER=http://" + e.hs + ":8080", "-e", "EXIT_NODE_HOSTNAME=exit-e2e", "-e", "HEALTH_PORT=off"}
	if key != "" {
		args = append(args, "-e", "TAILSCALE_AUTH_KEY="+key)
	}
	e.must(append(args, e.id+"-exit")...)
	return c
}

func (e *env) logs(c string) string {
	out, _ := exec.Command("docker", "logs", c).CombinedOutput()
	return string(out)
}

func (e *env) nodes() []string {
	e.t.Helper()
	var list []struct {
		ID   any    `json:"id"`
		Name string `json:"given_name"`
	}
	if err := json.Unmarshal([]byte(e.hsx("nodes", "list", "-o", "json")), &list); err != nil {
		e.t.Fatalf("nodes list: %v", err)
	}
	ids := make([]string, 0, len(list))
	for _, n := range list {
		ids = append(ids, fmt.Sprint(n.ID))
	}
	return ids
}

func (e *env) running(c string) bool {
	out, _ := e.docker("exec", c, "/app/tailscale", "status", "--json")
	return strings.Contains(out, `"BackendState": "Running"`)
}

func TestExitNodeOneTimeKey(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	id := fmt.Sprintf("exitkey-%d", time.Now().UnixNano()%1e6)
	e := &env{t: t, id: id, net: id + "-net", hs: id + "-hs", dir: t.TempDir()}
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+id).Output()
		for _, c := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", c).Run()
		}
		_ = exec.Command("docker", "volume", "rm", "-f", id+"-state").Run()
		_ = exec.Command("docker", "network", "rm", e.net).Run()
		_ = exec.Command("docker", "image", "rm", "-f", id+"-exit").Run()
	})

	// The image, for the host architecture (the Dockerfile defaults to amd64).
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	ts, err := os.ReadFile(filepath.Join(repoRoot(), "TAILSCALE_VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	v := strings.TrimSpace(string(ts))
	e.must("build", "-q", "-t", id+"-exit", "--build-arg", "TSVERSION="+v,
		"--build-arg", "TSFILE=tailscale_"+v+"_"+arch+".tgz", filepath.Join(repoRoot(), "apps", "exit-node"))

	cfg := filepath.Join(e.dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(`server_url: http://`+e.hs+`:8080
listen_addr: 0.0.0.0:8080
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
unix_socket: /data/headscale.sock
log:
  level: info
`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.must("network", "create", e.net)
	e.must("run", "-d", "--name", e.hs, "--network", e.net, "-v", e.dir+":/etc/headscale:ro", hsImage,
		"serve", "-c", "/etc/headscale/config.yaml")
	if !waitFor(40*time.Second, func() bool {
		_, err := e.docker("exec", e.hs, "headscale", "-c", "/etc/headscale/config.yaml", "users", "list")
		return err == nil
	}) {
		t.Fatal("headscale did not start")
	}
	e.hsx("users", "create", "e2e")
	uid := regexp.MustCompile(`"id":\s*(\d+)`).FindStringSubmatch(e.hsx("users", "list", "-o", "json"))[1]

	// 1. The first login with a one-time key.
	key := e.key(uid, false)
	c1 := e.start("first", key)
	if !waitFor(60*time.Second, func() bool { return e.running(c1) }) {
		t.Fatalf("first login failed:\n%s", e.logs(c1))
	}
	if n := e.nodes(); len(n) != 1 {
		t.Fatalf("one node after the first login, got %v", n)
	}
	// The key reaches tailscale through a 0600 file that is removed after the login: not in the environment of
	// tailscaled, not in the log, and no key file stays.
	if out, _ := e.docker("exec", c1, "sh", "-c",
		"for p in $(pidof tailscaled); do tr '\\0' '\\n' < /proc/$p/environ; done | grep -c '^TAILSCALE_AUTH_KEY=' || true"); out != "0" {
		t.Fatalf("tailscaled has the key in its environment (%s)", out)
	}
	if strings.Contains(e.logs(c1), key) {
		t.Fatal("the key shows in the log")
	}
	if out, _ := e.docker("exec", c1, "sh", "-c", "ls /run/ts-authkey.* 2>/dev/null | wc -l"); out != "0" {
		t.Fatalf("the key file stays after the login: %q", out)
	}
	e.must("stop", "-t", "10", c1)

	// 2. A restart with the state and the same (now used) key: no new node, no new login.
	c2 := e.start("restart", key)
	if !waitFor(60*time.Second, func() bool { return e.running(c2) }) {
		t.Fatalf("restart with a used key and a valid state failed:\n%s", e.logs(c2))
	}
	e.must("stop", "-t", "10", c2)

	// 3. A restart with the state and no key at all.
	c3 := e.start("nokey", "")
	if !waitFor(60*time.Second, func() bool { return e.running(c3) }) {
		t.Fatalf("restart without a key failed:\n%s", e.logs(c3))
	}
	if n := e.nodes(); len(n) != 1 {
		t.Fatalf("still one node after two restarts, got %v", n)
	}
	e.must("stop", "-t", "10", c3)

	// 4. The state is lost and the key is used: a clear message, no login loop, the container stays up.
	e.must("rm", "-f", c1, c2, c3)
	e.must("volume", "rm", id+"-state")
	c4 := e.start("lost", key)
	if !waitFor(90*time.Second, func() bool { return strings.Contains(e.logs(c4), "Ask an admin for a new one-time key") }) {
		t.Fatalf("no clear message for a used key:\n%s", e.logs(c4))
	}
	time.Sleep(15 * time.Second)
	if st, _ := e.docker("inspect", "-f", "{{.State.Running}}", c4); st != "true" {
		t.Fatalf("the container must wait, not exit:\n%s", e.logs(c4))
	}
	if n := strings.Count(e.logs(c4), "Ask an admin"); n != 1 {
		t.Fatalf("the message must come once (no loop), got %d", n)
	}
	if n := e.nodes(); len(n) != 1 {
		t.Fatalf("a used key must not register a second node, got %v", n)
	}

	// 5. A new one-time key from an admin brings it back.
	e.must("rm", "-f", c4)
	c5 := e.start("newkey", e.key(uid, false))
	if !waitFor(60*time.Second, func() bool { return e.running(c5) }) {
		t.Fatalf("new key failed:\n%s", e.logs(c5))
	}
}
