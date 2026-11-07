package enroll

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hy2Cfg is multiCfg with Hysteria 2 on the first bridge (#110): a mixed set, xray VLESS on both, Hysteria 2 on one.
func hy2Cfg() IngressConfig {
	c := multiCfg
	c.Ingresses = append([]Ingress(nil), multiCfg.Ingresses...)
	c.Ingresses[0].Hy2Port, c.Ingresses[0].Hy2Obfs = 8443, "obfs-fake"
	for i := range c.Ingresses { // a key in the real format, so sing-box check reads it (not a real key)
		c.Ingresses[i].PublicKey, c.Ingresses[i].ShortID = zeroKey, "0a1b"
	}
	return c
}

func TestHy2SingBoxProfile(t *testing.T) {
	cfg, d := hy2Cfg(), multiDevice()
	out, err := RenderProfile(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	by := map[string]map[string]any{}
	for _, o := range p.Outbounds {
		by[o["tag"].(string)] = o
	}
	for _, m := range Modes {
		g := by[m]
		want := []any{"" + m + "-ingress-1", m + "-ingress-1-hy2", m + "-ingress-2"}
		if g["type"] != "urltest" || !equalAny(g["outbounds"].([]any), want) {
			t.Fatalf("%s group: %v", m, g)
		}
		h := by[m+"-ingress-1-hy2"]
		uuid, _ := cfg.UUIDFor(cfg.Ingresses[0], m, d.UUIDs[m])
		if h["type"] != "hysteria2" || h["server"] != "ingress-1.example.com" || h["server_port"] != 8443.0 || h["password"] != uuid {
			t.Fatalf("%s hy2: %v", m, h)
		}
		obfs := h["obfs"].(map[string]any)
		tls := h["tls"].(map[string]any)
		if obfs["type"] != "salamander" || obfs["password"] != "obfs-fake" || tls["server_name"] != "ingress-1.example.com" || tls["insecure"] != nil {
			t.Fatalf("%s hy2 obfs/tls: %v", m, h)
		}
		if _, ok := by[m+"-ingress-2-hy2"]; ok {
			t.Fatal("a bridge without hy2_port got a Hysteria 2 outbound")
		}
	}
	checkSingBox(t, out)
}

// One bridge with Hysteria 2: the modes become groups of two, so a client falls back when UDP is blocked.
func TestHy2SingleBridgeIsGrouped(t *testing.T) {
	cfg := IngressConfig{ProfileName: "tunnel", LinkHost: "b.example.com", Ingresses: []Ingress{{Name: "b", Host: "b.example.com",
		PublicKey: zeroKey, ShortID: "0a", SNI: "s.example.com", Port: 443, UUIDs: "base", Hy2Port: 443}}}
	out, err := RenderProfile(cfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"tag":"auto","outbounds":["auto-b","auto-b-hy2"]`, `"type":"hysteria2"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, `"obfs"`) {
		t.Fatal("obfs without a password")
	}
	checkSingBox(t, out)
	cl, _ := RenderClashProfile(cfg, multiDevice())
	if !strings.Contains(string(cl), `"auto-b-hy2"`) || strings.Contains(string(cl), "obfs") {
		t.Fatalf("clash: %s", cl)
	}
}

func TestHy2ClashProfile(t *testing.T) {
	out, err := RenderClashProfile(hy2Cfg(), multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"name": "exit-ingress-1-hy2"`, `"type": "hysteria2"`, `"port": 8443`, `"obfs": "salamander"`, `"obfs-password": "obfs-fake"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestHy2Config(t *testing.T) {
	base := `{"profile_name":"t","link_host":"l.example.com","ingresses":[{"name":"a","host":"a.example.com","public_key":"K","short_id":"0a","uuids":"base"%s}]}`
	cfg, err := ParseIngressConfig([]byte(strings.Replace(base, "%s", `,"hy2_port":8443`, 1)))
	if err != nil || cfg.Ingresses[0].Hy2Port != 8443 {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, bad := range []string{`,"hy2_port":0`, `,"hy2_port":70000`, `,"hy2_port":"443"`, `,"hy2_obfs":"x"`} {
		if _, err := ParseIngressConfig([]byte(strings.Replace(base, "%s", bad, 1))); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// The password comes from HY2_OBFS_<NAME> only, and is never serialised.
	r, err := NewRegistry(RegistryConfig{Cfg: cfg, Getenv: func(k string) string {
		return map[string]string{"AGENT_TOKEN": "t", "HY2_OBFS_A": "pw"}[k]
	}, AgentURL: "http://agent.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	ing := r.First().Ingress
	if ing.Hy2Obfs != "pw" {
		t.Fatalf("obfs %q", ing.Hy2Obfs)
	}
	if b, _ := json.Marshal(ing); strings.Contains(string(b), "pw") {
		t.Fatalf("the password is serialised: %s", b)
	}
}

// The published hy2_port is read from /v1/info and changes the fingerprint; without it the fingerprint is the old one.
func TestHy2InfoAndFingerprint(t *testing.T) {
	body := `{"name":"b","host":"b.example.com","port":443,"sni":"b.example.com","public_key":"K","short_id":"0a"}`
	old, err := parseInfo("b", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	withHy2, err := parseInfo("b", []byte(strings.TrimSuffix(body, "}")+`,"hy2_port":443}`))
	if err != nil || withHy2.ing.Hy2Port != 443 {
		t.Fatalf("%+v %v", withHy2, err)
	}
	if old.fp == withHy2.fp {
		t.Fatal("adding Hysteria 2 must need a new approval")
	}
	const before = "{\"name\":\"b\",\"host\":\"b.example.com\",\"port\":443,\"sni\":\"b.example.com\",\"public_key\":\"K\",\"short_id\":\"0a\"}"
	if old.fp != sha256Hex(before) {
		t.Fatal("the fingerprint of a bridge without Hysteria 2 changed")
	}
}

// zeroKey is 32 zero bytes in base64url: the format of a Reality public key, and no real key.
const zeroKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func equalAny(a []any, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkSingBox(t *testing.T, profile []byte) {
	t.Helper()
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		return
	}
	f := filepath.Join(t.TempDir(), "p.json")
	_ = os.WriteFile(f, profile, 0o600)
	if out, err := exec.Command(bin, "check", "-c", f).CombinedOutput(); err != nil {
		t.Fatalf("sing-box check: %v\n%s", err, out)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
