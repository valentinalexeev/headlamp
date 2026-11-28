package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The Map tab (#429) and its assets are behind the same policy.view gate as the console.
func TestMapPageNeedsPolicyView(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, p := range []string{"/policy/map", "/policy/api/map"} {
		if rec := h.do("GET", p, adminAddr, "", false); rec.Code != 200 {
			t.Errorf("%s as admin: %d", p, rec.Code)
		}
		for _, addr := range []string{otherAddr, taggedAddr, errAddr} {
			if rec := h.do("GET", p, addr, "", false); rec.Code == 200 {
				t.Errorf("%s from %s: %d, want refused", p, addr, rec.Code)
			}
		}
	}
}

func TestMapAssetsHaveTheirTypes(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for p, want := range map[string]string{
		"/policy/map":                            "text/html",
		"/policy/static/map.js":                  "text/javascript",
		"/policy/static/map.css":                 "text/css",
		"/policy/static/vendor/cytoscape.min.js": "text/javascript",
	} {
		rec := h.do("GET", p, adminAddr, "", false)
		if ct := rec.Header().Get("Content-Type"); rec.Code != 200 || !strings.HasPrefix(ct, want) {
			t.Errorf("%s: %d %q, want %s", p, rec.Code, ct, want)
		}
	}
	if rec := h.do("GET", "/policy/static/vendor/LICENSE", adminAddr, "", false); rec.Code == 200 {
		t.Error("the vendor directory is served as a whole")
	}
}

// The editor and the map link to each other.
func TestPolicyTabsLinkBothViews(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, p := range []string{"/policy", "/policy/map"} {
		body := h.do("GET", p, adminAddr, "", false).Body.String()
		if !strings.Contains(body, `href="/policy/map"`) || !strings.Contains(body, `href="/policy"`) {
			t.Errorf("%s does not link both views", p)
		}
	}
}

// The binary serves the pinned file: the embedded bytes have the SHA-256 that static/vendor/VENDORED (and
// make check-vendored) pins.
func TestVendoredCytoscapeIsThePinnedFile(t *testing.T) {
	manifest, err := os.ReadFile("static/vendor/VENDORED")
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, line := range strings.Split(string(manifest), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "cytoscape.min.js" {
			want = f[2]
		}
	}
	sum := sha256.Sum256(cytoscapeJS)
	if got := hex.EncodeToString(sum[:]); want == "" || got != want {
		t.Errorf("embedded cytoscape.min.js sha256 %s, VENDORED pins %q", got, want)
	}
	h := newHarness(t, baseEnv(t))
	if !bytes.Equal(h.do("GET", "/policy/static/vendor/cytoscape.min.js", adminAddr, "", false).Body.Bytes(), cytoscapeJS) {
		t.Error("the served file is not the embedded one")
	}
}

// The map script runs under node against a small DOM stand-in and a fake Cytoscape.js (skipped when node is missing).
func TestMapPageSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "testdata/map_selftest.js", "../ui/static/hl.js", "static/map.js").CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/map.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}
