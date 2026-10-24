package nodes

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"example.com/node-control/internal/core"
)

const ociFP = "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"

var (
	ociKeyOnce sync.Once
	ociKey     *rsa.PrivateKey
)

// ociTestPEM is a throwaway PKCS#1 key generated at run time and encoded in
// memory; no key material is stored in the repository.
func ociTestPEM(t *testing.T) string {
	t.Helper()
	ociKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		ociKey = k
	})
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ociKey)}))
}

type fakeOCI struct {
	mu        sync.Mutex
	srv       *httptest.Server
	instances []map[string]string // id, name, state
	shape     string
	getErr    string // body of a 500 on GET /instances/{id}
	postCode  int
	posts     []string // "path?query"
	lists     []string // raw query of list calls
	auths     []string
	bodies    []string // body of POSTs
}

func (f *fakeOCI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	f.auths = append(f.auths, auth)
	if !strings.HasPrefix(auth, "Signature ") || r.Header.Get("Date") == "" {
		http.Error(w, "unsigned", 401)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/20160918/instances":
		f.lists = append(f.lists, r.URL.RawQuery)
		var out []map[string]string
		for _, i := range f.instances {
			if i["name"] == r.URL.Query().Get("displayName") {
				out = append(out, map[string]string{"id": i["id"], "displayName": i["name"], "lifecycleState": i["state"]})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/20160918/instances/"):
		if f.getErr != "" {
			http.Error(w, strings.ReplaceAll(f.getErr, "{AUTH}", auth), 500)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/20160918/instances/")
		for _, i := range f.instances {
			if i["id"] == id {
				fmt.Fprintf(w, `{"id":%q,"lifecycleState":%q,"shape":%q}`, id, i["state"], f.shape)
				return
			}
		}
		http.Error(w, "missing", 404)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/20160918/instances/"):
		f.posts = append(f.posts, r.URL.Path+"?"+r.URL.RawQuery)
		b := make([]byte, 16)
		n, _ := r.Body.Read(b)
		f.bodies = append(f.bodies, string(b[:n]))
		if f.postCode != 0 {
			http.Error(w, "conflict", f.postCode)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func ociEnv(t *testing.T) map[string]string {
	return map[string]string{
		"OCI_USER_OCID": "ocid1.user.oc1..exampleuser", "OCI_TENANCY_OCID": "ocid1.tenancy.oc1..exampletenancy",
		"OCI_FINGERPRINT": ociFP, "OCI_PRIVATE_KEY": ociTestPEM(t),
		"OCI_REGION": "region-1", "OCI_COMPARTMENT_ID": "ocid1.compartment.oc1..examplecomp",
	}
}

func newOCI(t *testing.T, f *fakeOCI, env map[string]string) *oci {
	t.Helper()
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	if f.shape == "" {
		f.shape = "shape-1"
	}
	p := NewOCI(core.MapEnv(env), f.srv.Client()).(*oci)
	p.baseURL = f.srv.URL
	p.now = func() time.Time { return time.Unix(1_000_000, 0) }
	return p
}

func ociInst(id, name, state string) map[string]string {
	return map[string]string{"id": id, "name": name, "state": state}
}

var ociTarget = Target{ID: "node-a", Name: "web", Provider: "oci"}

func ociBlock(b map[string]any) Target {
	t := ociTarget
	t.Block = b
	return t
}

func TestFindByNameIgnoresTerminated(t *testing.T) {
	f := &fakeOCI{instances: []map[string]string{
		ociInst("old", "web", "TERMINATED"), ociInst("dying", "web", "TERMINATING"), ociInst("new", "web", "RUNNING"),
		ociInst("other", "web2", "RUNNING"),
	}}
	p := newOCI(t, f, ociEnv(t))
	st, err := p.Status(context.Background(), ociTarget)
	if err != nil || st["status"] != "RUNNING" || st["shape"] != "shape-1" {
		t.Fatalf("%v %v", st, err)
	}
	q := f.lists[0]
	if !strings.Contains(q, "compartmentId=ocid1.compartment.oc1..examplecomp") || !strings.Contains(q, "displayName=web") {
		t.Fatalf("query %q", q)
	}
	for _, a := range f.auths {
		if !strings.Contains(a, `keyId="ocid1.tenancy.oc1..exampletenancy/ocid1.user.oc1..exampleuser/`+ociFP+`"`) {
			t.Fatalf("keyId missing in %q", a)
		}
	}
}

func TestOCINoneAndAmbiguous(t *testing.T) {
	ctx := context.Background()
	var ae *APIError
	f := &fakeOCI{instances: []map[string]string{ociInst("old", "web", "TERMINATED"), ociInst("dying", "web", "TERMINATING")}}
	p := newOCI(t, f, ociEnv(t))
	_, err := p.Status(ctx, ociTarget)
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "no instance named 'web' in the compartment") {
		t.Fatalf("%v", err)
	}
	f2 := &fakeOCI{instances: []map[string]string{ociInst("a", "web", "RUNNING"), ociInst("b", "web", "STOPPED")}}
	p2 := newOCI(t, f2, ociEnv(t))
	_, err = p2.Status(ctx, ociTarget)
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "2 live instances named 'web'; refusing to guess") {
		t.Fatalf("%v", err)
	}
}

func TestOCIPinnedIDSkipsLookupAndBlockOverrides(t *testing.T) {
	f := &fakeOCI{instances: []map[string]string{ociInst("pinned", "x", "STOPPED")}}
	env := ociEnv(t)
	delete(env, "OCI_COMPARTMENT_ID")
	p := newOCI(t, f, env)
	st, err := p.Status(context.Background(), ociBlock(map[string]any{"instance_id": "pinned", "region": "region-2"}))
	if err != nil || st["status"] != "STOPPED" || len(f.lists) != 0 {
		t.Fatalf("%v %v lists=%v", st, err, f.lists)
	}
	// instance_name overrides Target.Name; block compartment overrides env.
	f.instances = append(f.instances, ociInst("byname", "custom", "RUNNING"))
	st, err = p.Status(context.Background(), ociBlock(map[string]any{"instance_name": "custom", "compartment_id": "ocid1.compartment.oc1..blockcomp"}))
	if err != nil || st["status"] != "RUNNING" || !strings.Contains(f.lists[0], "compartmentId=ocid1.compartment.oc1..blockcomp") {
		t.Fatalf("%v %v %v", st, err, f.lists)
	}
}

func TestOCIStartStopIdempotent(t *testing.T) {
	ctx := context.Background()
	f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}}
	p := newOCI(t, f, ociEnv(t))
	prev, cur, err := p.Start(ctx, ociTarget)
	if err != nil || prev != "RUNNING" || cur != "RUNNING" || len(f.posts) != 0 {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}
	prev, cur, err = p.Stop(ctx, ociTarget)
	if err != nil || prev != "RUNNING" || cur != "STOPPING" || len(f.posts) != 1 || f.posts[0] != "/20160918/instances/i1?action=STOP" {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}
	if f.bodies[0] != "" {
		t.Fatalf("action body %q", f.bodies[0])
	}

	f.instances[0]["state"] = "STOPPED"
	prev, cur, err = p.Stop(ctx, ociTarget)
	if err != nil || prev != "STOPPED" || cur != "STOPPED" || len(f.posts) != 1 {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}
	prev, cur, err = p.Start(ctx, ociTarget)
	if err != nil || prev != "STOPPED" || cur != "STARTING" || f.posts[1] != "/20160918/instances/i1?action=START" {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}

	// Transient states are left alone rather than re-triggered.
	f.instances[0]["state"] = "STARTING"
	if prev, cur, err = p.Start(ctx, ociTarget); err != nil || prev != "STARTING" || cur != "STARTING" || len(f.posts) != 2 {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}
	f.instances[0]["state"] = "STOPPING"
	if prev, cur, err = p.Stop(ctx, ociTarget); err != nil || prev != "STOPPING" || cur != "STOPPING" || len(f.posts) != 2 {
		t.Fatalf("%q %q %v %v", prev, cur, err, f.posts)
	}
}

func TestOCIActionFailure(t *testing.T) {
	f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "STOPPED")}, postCode: 409}
	p := newOCI(t, f, ociEnv(t))
	_, _, err := p.Start(context.Background(), ociTarget)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("%v", err)
	}
}

func TestNotConfigured(t *testing.T) {
	ctx := context.Background()
	for _, miss := range []string{"OCI_USER_OCID", "OCI_TENANCY_OCID", "OCI_FINGERPRINT", "OCI_PRIVATE_KEY", "OCI_REGION", "OCI_COMPARTMENT_ID"} {
		t.Run(miss, func(t *testing.T) {
			env := ociEnv(t)
			delete(env, miss)
			p := newOCI(t, &fakeOCI{}, env)
			_, err := p.Status(ctx, ociTarget)
			if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), miss) {
				t.Fatalf("%v", err)
			}
		})
	}
	t.Run("pinned id needs no compartment", func(t *testing.T) {
		env := ociEnv(t)
		delete(env, "OCI_COMPARTMENT_ID")
		p := newOCI(t, &fakeOCI{instances: []map[string]string{ociInst("p", "x", "RUNNING")}}, env)
		if _, err := p.Status(ctx, ociBlock(map[string]any{"instance_id": "p"})); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unparsable key", func(t *testing.T) {
		env := ociEnv(t)
		env["OCI_PRIVATE_KEY"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not-a-key-zzz-not-a-key")}))
		p := newOCI(t, &fakeOCI{}, env)
		_, err := p.Status(ctx, ociTarget)
		if !errors.Is(err, ErrNotConfigured) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("not-a-key-zzz-not-a-key"))[:12]) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("hostile region", func(t *testing.T) {
		env := ociEnv(t)
		env["OCI_REGION"] = "x.example.com/"
		p := NewOCI(core.MapEnv(env), nil)
		if _, err := p.Status(ctx, ociTarget); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%v", err)
		}
	})
}

func TestOCIAcceptsPKCS8AndEscapedNewlines(t *testing.T) {
	pemText := ociTestPEM(t)
	blk, _ := pem.Decode([]byte(pemText))
	k1, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k1)
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, key := range map[string]string{
		"pkcs8":           pkcs8,
		"escaped newline": strings.ReplaceAll(strings.TrimSpace(pemText), "\n", `\n`),
	} {
		t.Run(name, func(t *testing.T) {
			env := ociEnv(t)
			env["OCI_PRIVATE_KEY"] = key
			f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}}
			p := newOCI(t, f, env)
			if _, err := p.Status(context.Background(), ociTarget); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOCIErrorsAreBoundedAndScrubbed(t *testing.T) {
	ctx := context.Background()
	var ae *APIError

	t.Run("body echoing the authorization header", func(t *testing.T) {
		f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}, getErr: "oops {AUTH} oops"}
		p := newOCI(t, f, ociEnv(t))
		_, err := p.Status(ctx, ociTarget)
		if !errors.As(err, &ae) || ae.Status != 500 {
			t.Fatalf("%v", err)
		}
		msg := err.Error()
		last := f.auths[len(f.auths)-1]
		sig := last[strings.Index(last, `signature="`)+len(`signature="`):]
		sig = sig[:strings.Index(sig, `"`)]
		if sig == "" || strings.Contains(msg, sig[:40]) {
			t.Fatalf("signature leaked: %q", msg)
		}
		if strings.Contains(msg, ociFP) {
			t.Fatalf("fingerprint leaked: %q", msg)
		}
		if !strings.Contains(msg, "oops ") {
			t.Fatalf("body missing: %q", msg)
		}
	})

	t.Run("body echoing the private key", func(t *testing.T) {
		env := ociEnv(t)
		pemText := env["OCI_PRIVATE_KEY"]
		f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}, getErr: "key=" + pemText + "=end"}
		p := newOCI(t, f, env)
		_, err := p.Status(ctx, ociTarget)
		if err == nil || strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), strings.Split(pemText, "\n")[2]) {
			t.Fatalf("key leaked: %v", err)
		}
		if !strings.Contains(err.Error(), "key=***=end") {
			t.Fatalf("not scrubbed in place: %v", err)
		}
	})

	t.Run("fingerprint straddling the cut", func(t *testing.T) {
		for _, pad := range []int{500, 505, 511, 512} {
			f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")},
				getErr: strings.Repeat("x", pad) + ociFP + strings.Repeat("y", 5000)}
			p := newOCI(t, f, ociEnv(t))
			_, err := p.Status(ctx, ociTarget)
			if err == nil || len(err.Error()) > 700 || strings.Contains(err.Error(), "aa:bb") || strings.Contains(err.Error(), "88:99") {
				t.Fatalf("pad %d: %v", pad, err)
			}
		}
	})

	t.Run("UTF-8 cut lands mid-rune", func(t *testing.T) {
		// 511 ASCII bytes then 2-byte runes: byte 512 is the second half of a rune.
		body := strings.Repeat("x", 511) + strings.Repeat("é", 100)
		f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}, getErr: body}
		p := newOCI(t, f, ociEnv(t))
		_, err := p.Status(ctx, ociTarget)
		if err == nil || !utf8.ValidString(err.Error()) {
			t.Fatalf("invalid utf-8: %q", err)
		}
		if strings.Contains(err.Error(), "é") {
			t.Fatalf("expected the straddling rune to be dropped: %q", err)
		}
	})
}
