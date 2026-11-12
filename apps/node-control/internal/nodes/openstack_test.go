package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

type fakeNova struct {
	mu         sync.Mutex
	srv        *httptest.Server
	authN      int
	authBody   map[string]any
	servers    []map[string]string // id, name
	status     map[string]string   // id -> raw status
	posts      []string            // "path body"
	queries    []string
	token      string
	seenTok    string
	region     string
	authFail   int
	serverErr  string // body of a 500 on GET /servers/{id}
	postStatus int    // status for action POSTs (default 202)
}

func (f *fakeNova) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/identity/v3/auth/tokens":
		f.authN++
		_ = json.NewDecoder(r.Body).Decode(&f.authBody)
		if f.authFail != 0 {
			http.Error(w, "denied "+f.token, f.authFail)
			return
		}
		w.Header().Set("X-Subject-Token", f.token)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":{"catalog":[{"type":"compute","endpoints":[
			{"interface":"admin","region":%q,"url":"http://wrong.invalid/a"},
			{"interface":"public","region":"other","url":"http://wrong.invalid/b"},
			{"interface":"public","region":%q,"url":"%s/compute/v2.1/"}]}]}}`, f.region, f.region, f.srv.URL)
	case r.Header.Get("X-Auth-Token") == "":
		http.Error(w, "no token", 401)
	case r.Method == "GET" && r.URL.Path == "/compute/v2.1/servers":
		f.seenTok = r.Header.Get("X-Auth-Token")
		f.queries = append(f.queries, r.URL.Query().Get("name"))
		var out []map[string]string
		for _, s := range f.servers {
			out = append(out, map[string]string{"id": s["id"], "name": s["name"]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"servers": out})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/compute/v2.1/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/compute/v2.1/servers/")
		if f.serverErr != "" {
			http.Error(w, f.serverErr, 500)
			return
		}
		st, ok := f.status[id]
		if !ok {
			http.Error(w, "missing", 404)
			return
		}
		fmt.Fprintf(w, `{"server":{"id":%q,"status":%q}}`, id, st)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/action"):
		b := new(strings.Builder)
		_, _ = fmt.Fprint(b, r.URL.Path, " ")
		var m any
		_ = json.NewDecoder(r.Body).Decode(&m)
		j, _ := json.Marshal(m)
		b.Write(j)
		f.posts = append(f.posts, b.String())
		if f.postStatus != 0 {
			http.Error(w, "conflict", f.postStatus)
			return
		}
		w.WriteHeader(202)
	default:
		http.Error(w, "not found", 404)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var selEnv = map[string]string{
	"SELECTEL_ACCOUNT_ID": "acct-1", "SELECTEL_PROJECT_ID": "proj-1",
	"SELECTEL_USER": "svc", "SELECTEL_PASSWORD": "pw-secret", "SELECTEL_REGION": "region-1",
}

func newOpenStack(t *testing.T, f *fakeNova, env map[string]string) (*openstack, *clock) {
	t.Helper()
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	if f.token == "" {
		f.token = "tok-1"
	}
	if f.region == "" {
		f.region = "region-1"
	}
	p := NewOpenStack(core.MapEnv(env), f.srv.Client()).(*openstack)
	p.authURL = f.srv.URL + "/identity/v3"
	c := &clock{t: time.Unix(1_000_000, 0)}
	p.now = c.now
	return p, c
}

var selTarget = Target{ID: "node-a", Name: "node-a", Provider: "selectel"}

func TestFindsInstanceByName(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, _ := newOpenStack(t, f, selEnv)
	st, err := p.Status(context.Background(), selTarget)
	if err != nil || st["status"] != "RUNNING" {
		t.Fatal(st, err)
	}
	if len(f.queries) != 1 || f.queries[0] != `^node-a$` {
		t.Fatalf("%v", f.queries)
	}
	if f.seenTok != "tok-1" {
		t.Fatal(f.seenTok)
	}
	// auth payload
	b, _ := json.Marshal(f.authBody)
	for _, want := range []string{`"acct-1"`, `"proj-1"`, `"svc"`, `"pw-secret"`, `"password"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("auth body %s lacks %s", b, want)
		}
	}
	// name is regexp-quoted and instance_name beats Target.Name
	tg := Target{ID: "x", Name: "ignored", Provider: "selectel", Block: map[string]any{"instance_name": "a.b+c"}}
	_, _ = p.Status(context.Background(), tg)
	if f.queries[1] != `^a\.b\+c$` {
		t.Fatalf("%v", f.queries)
	}
}

func TestInstanceIDPinsAndRegionFromBlock(t *testing.T) {
	f := &fakeNova{region: "region-2", status: map[string]string{"pinned": "SHUTOFF"}}
	p, _ := newOpenStack(t, f, map[string]string{
		"SELECTEL_ACCOUNT_ID": "a", "SELECTEL_PROJECT_ID": "p", "SELECTEL_USER": "u", "SELECTEL_PASSWORD": "pw",
	})
	tg := Target{ID: "x", Name: "x", Provider: "selectel", Block: map[string]any{"instance_id": "pinned", "region": "region-2"}}
	st, err := p.Status(context.Background(), tg)
	if err != nil || st["status"] != "STOPPED" {
		t.Fatal(st, err)
	}
	if len(f.queries) != 0 {
		t.Fatalf("listed servers: %v", f.queries)
	}
}

func TestStatusVocabulary(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "BUILD"}}
	p, _ := newOpenStack(t, f, selEnv)
	for raw, want := range map[string]string{"ACTIVE": "RUNNING", "SHUTOFF": "STOPPED", "BUILD": "STARTING", "HARD_REBOOT": "STARTING", "ERROR": "ERROR"} {
		f.status["id1"] = raw
		st, err := p.Status(context.Background(), selTarget)
		if err != nil || st["status"] != want {
			t.Fatal(raw, st, err)
		}
	}
}

func TestStartStopIdempotent(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, _ := newOpenStack(t, f, selEnv)
	ctx := context.Background()

	prev, cur, err := p.Start(ctx, selTarget)
	if err != nil || prev != "RUNNING" || cur != "RUNNING" || len(f.posts) != 0 {
		t.Fatal(prev, cur, err, f.posts)
	}
	prev, cur, err = p.Stop(ctx, selTarget)
	if err != nil || prev != "RUNNING" || cur != "STOPPING" {
		t.Fatal(prev, cur, err)
	}
	if len(f.posts) != 1 || f.posts[0] != `/compute/v2.1/servers/id1/action {"os-stop":null}` {
		t.Fatalf("%v", f.posts)
	}
	f.status["id1"] = "SHUTOFF"
	prev, cur, _ = p.Stop(ctx, selTarget)
	if prev != "STOPPED" || cur != "STOPPED" || len(f.posts) != 1 {
		t.Fatal(prev, cur, f.posts)
	}
	prev, cur, _ = p.Start(ctx, selTarget)
	if prev != "STOPPED" || cur != "STARTING" || f.posts[1] != `/compute/v2.1/servers/id1/action {"os-start":null}` {
		t.Fatal(prev, cur, f.posts)
	}
}

func TestTokenCachedUntilNearExpiry(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, c := newOpenStack(t, f, selEnv)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := p.Status(ctx, selTarget); err != nil {
			t.Fatal(err)
		}
	}
	if f.authN != 1 {
		t.Fatalf("auth calls %d", f.authN)
	}
	c.add(2600 * time.Second) // 400s left: still cached
	_, _ = p.Status(ctx, selTarget)
	if f.authN != 1 {
		t.Fatalf("auth calls %d", f.authN)
	}
	c.add(200 * time.Second) // 200s left: refresh
	f.token = "tok-2"
	_, _ = p.Status(ctx, selTarget)
	if f.authN != 2 || f.seenTok != "tok-2" {
		t.Fatalf("auth calls %d tok %q", f.authN, f.seenTok)
	}
}

func TestMissingCredentialsNotConfigured(t *testing.T) {
	for _, drop := range []string{"SELECTEL_ACCOUNT_ID", "SELECTEL_PROJECT_ID", "SELECTEL_USER", "SELECTEL_PASSWORD"} {
		env := map[string]string{}
		for k, v := range selEnv {
			if k != drop {
				env[k] = v
			}
		}
		f := &fakeNova{}
		p, _ := newOpenStack(t, f, env)
		_, err := p.Status(context.Background(), selTarget)
		if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), drop) {
			t.Fatalf("%s: %v", drop, err)
		}
		if f.authN != 0 {
			t.Fatal("contacted server without credentials")
		}
	}
}

func TestSelectelRegionNotConfigured(t *testing.T) {
	env := map[string]string{}
	for k, v := range selEnv {
		if k != "SELECTEL_REGION" {
			env[k] = v
		}
	}
	p, _ := newOpenStack(t, &fakeNova{}, env)
	if _, err := p.Status(context.Background(), selTarget); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestSelectelNoNameNotConfigured(t *testing.T) {
	p, _ := newOpenStack(t, &fakeNova{}, selEnv)
	if _, err := p.Status(context.Background(), Target{ID: "x", Provider: "selectel"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestSelectelAmbiguousAndMissingInstance(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "a", "name": "node-a"}, {"id": "b", "name": "node-a"}}, status: map[string]string{}}
	p, _ := newOpenStack(t, f, selEnv)
	var ae *APIError
	_, err := p.Status(context.Background(), selTarget)
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "2 instances named 'node-a'; refusing to guess") {
		t.Fatalf("%v", err)
	}
	f.servers = nil
	_, err = p.Status(context.Background(), selTarget)
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "no instance named 'node-a'") {
		t.Fatalf("%v", err)
	}
}

func TestSelectelErrorsAreBoundedAndScrubbed(t *testing.T) {
	ctx := context.Background()
	var ae *APIError

	// Keystone failure whose body echoes the password early on.
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad login pw-secret for svc", 401)
	}))
	defer ks.Close()
	p := NewOpenStack(core.MapEnv(selEnv), ks.Client()).(*openstack)
	p.authURL = ks.URL
	_, err := p.Status(ctx, selTarget)
	if !errors.As(err, &ae) || ae.Status != 401 || strings.Contains(err.Error(), "pw-secret") || !strings.Contains(err.Error(), "bad login *** for svc") {
		t.Fatalf("keystone: %v", err)
	}
	// A 401 says where to look: the value in the container (#302).
	if !strings.Contains(err.Error(), "credential fingerprint in the start log") {
		t.Fatalf("keystone 401 has no hint: %v", err)
	}

	// Compute failure whose body echoes the token.
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}, token: "tok-leak", serverErr: "oops tok-leak oops"}
	p2, _ := newOpenStack(t, f, selEnv)
	_, err = p2.Status(ctx, selTarget)
	if !errors.As(err, &ae) || ae.Status != 500 || strings.Contains(err.Error(), "tok-leak") || !strings.Contains(err.Error(), "oops *** oops") {
		t.Fatalf("compute: %v", err)
	}

	// A secret straddling the 512-byte cut is masked, and the text is bounded.
	for _, pad := range []int{500, 509, 511, 512} {
		body := strings.Repeat("x", pad) + "pw-secret" + strings.Repeat("y", 5000)
		big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, body, 500)
		}))
		p3 := NewOpenStack(core.MapEnv(selEnv), big.Client()).(*openstack)
		p3.authURL = big.URL
		_, err = p3.Status(ctx, selTarget)
		big.Close()
		if err == nil || len(err.Error()) > 700 || strings.Contains(err.Error(), "pw-") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("pad %d: %v", pad, err)
		}
	}
}

func TestScrubKeepsValidUTF8(t *testing.T) {
	got := scrub(strings.Repeat("\u00e9", 400), nil) // 2-byte runes, cut lands mid-rune
	if !utf8.ValidString(got) || len(got) > maxErrBody+3 {
		t.Fatalf("%d %v", len(got), utf8.ValidString(got))
	}
}

func TestSelectelActionFailureAndPinned404(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "SHUTOFF"}, postStatus: 409}
	p, _ := newOpenStack(t, f, selEnv)
	var ae *APIError
	_, _, err := p.Start(context.Background(), selTarget)
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("%v", err)
	}
	tg := Target{ID: "x", Name: "x", Provider: "selectel", Block: map[string]any{"instance_id": "gone"}}
	_, err = p.Status(context.Background(), tg)
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
}

func TestSelectelNoComputeEndpointForRegion(t *testing.T) {
	f := &fakeNova{region: "region-9"}
	p, _ := newOpenStack(t, f, selEnv)
	_, err := p.Status(context.Background(), selTarget)
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "region-1") {
		t.Fatalf("%v", err)
	}
}

// Another OpenStack-compatible cloud with its own Keystone (#101): the operator declares it in OPENSTACK_CLOUDS and
// the node block names it. That cloud's own service user signs in there.
func otherCloud(t *testing.T) (*openstack, *fakeNova, *fakeNova, Target) {
	t.Helper()
	other := &fakeNova{servers: []map[string]string{{"id": "id9", "name": "node-a"}}, status: map[string]string{"id9": "SHUTOFF"},
		token: "tok-2", region: "zone-2"}
	tls := httptest.NewTLSServer(other)
	t.Cleanup(tls.Close)
	other.srv = tls
	env := map[string]string{"OPENSTACK_CLOUDS": " OTHERCLOUD = " + tls.URL + "/identity/v3/ ",
		"OTHERCLOUD_ACCOUNT_ID": "acct-2", "OTHERCLOUD_PROJECT_ID": "proj-2",
		"OTHERCLOUD_USER": "svc2", "OTHERCLOUD_PASSWORD": "pw2-secret", "OTHERCLOUD_REGION": "zone-2"}
	for k, v := range selEnv {
		env[k] = v
	}
	def := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, _ := newOpenStack(t, def, env)
	p.hc = tls.Client()
	return p, def, other, Target{ID: "node-a", Name: "node-a", Provider: "selectel", Block: map[string]any{"cloud": "OTHERCLOUD"}}
}

func TestOtherOpenStackCloud(t *testing.T) {
	p, def, other, far := otherCloud(t)
	st, err := p.Status(context.Background(), far)
	if err != nil || st["status"] != "STOPPED" {
		t.Fatalf("other cloud: %v %v", st, err)
	}
	id := other.authBody["auth"].(map[string]any)["identity"].(map[string]any)["password"].(map[string]any)["user"].(map[string]any)
	if id["name"] != "svc2" || id["password"] != "pw2-secret" || id["domain"].(map[string]any)["name"] != "acct-2" {
		t.Fatalf("auth body: %v", other.authBody)
	}
	if proj := other.authBody["auth"].(map[string]any)["scope"].(map[string]any)["project"].(map[string]any)["id"]; proj != "proj-2" {
		t.Fatalf("project: %v", proj)
	}
	if st, err := p.Status(context.Background(), selTarget); err != nil || st["status"] != "RUNNING" {
		t.Fatalf("default cloud: %v %v", st, err)
	}
	if def.authN != 1 || other.authN != 1 {
		t.Fatalf("one token per cloud: default=%d other=%d", def.authN, other.authN)
	}
	if _, cur, err := p.Start(context.Background(), far); err != nil || cur != "STARTING" || len(other.posts) != 1 || len(def.posts) != 0 {
		t.Fatalf("start went to the wrong cloud: %v %v other=%v default=%v", cur, err, other.posts, def.posts)
	}
}

func TestOtherOpenStackCloudPasswordScrubbed(t *testing.T) {
	p, _, other, far := otherCloud(t)
	other.authFail = 401
	other.token = "pw2-secret" // the fake echoes the token in its error body: make it the password
	_, err := p.Status(context.Background(), far)
	if err == nil || strings.Contains(err.Error(), "pw2-secret") {
		t.Fatalf("password not scrubbed: %v", err)
	}
}

func TestOpenStackCloudsRules(t *testing.T) {
	ctx := context.Background()
	base := func(clouds string) *openstack {
		env := map[string]string{"OPENSTACK_CLOUDS": clouds, "OTHERCLOUD_REGION": "r"}
		for k, v := range selEnv {
			env[k] = v
		}
		p, _ := newOpenStack(t, &fakeNova{}, env)
		return p
	}
	far := Target{Name: "x", Block: map[string]any{"cloud": "OTHERCLOUD"}}
	// A block can only name a declared cloud; it can never supply a URL.
	if _, err := base("").Status(ctx, far); !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "OPENSTACK_CLOUDS") {
		t.Errorf("undeclared cloud: %v", err)
	}
	for _, bad := range []string{
		"OTHERCLOUD=http://keystone.example.com/v3",      // not https
		"SELECTEL=https://keystone.example.com/v3",       // would send the Selectel password elsewhere
		"lower=https://keystone.example.com/v3",          // not an env name
		"OTHERCLOUD",                                     // no URL
		"secret-value-here=https://keystone.example.com", // the error names the entry, never its text
	} {
		_, err := base(bad).Status(ctx, far)
		if !errors.Is(err, ErrNotConfigured) || strings.Contains(err.Error(), "secret-value-here") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Declared, but its credentials are missing: the error names the variable, never a value.
	if _, err := base("OTHERCLOUD=https://keystone.example.com/v3").Status(ctx, far); !errors.Is(err, ErrNotConfigured) ||
		!strings.Contains(err.Error(), "OTHERCLOUD_USER") {
		t.Errorf("missing creds: %v", err)
	}
}

// "openstack" is the generic provider name (#101): the same provider, chosen by tag:cloud-openstack or
// "provider": "openstack" with an "openstack" block.
func TestOpenStackProviderName(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"nodes":{"n1":{"provider":"openstack","openstack":{"instance_id":"id1","region":"r"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ts := Discover(cfg, nil)
	if len(ts) != 1 || ts[0].Provider != "openstack" || ts[0].Block["instance_id"] != "id1" {
		t.Fatalf("targets: %+v", ts)
	}
	if got := cloudProvider([]string{"tag:exit", "tag:cloud-openstack"}); got != "openstack" {
		t.Fatalf("tag: %q", got)
	}
	if ProviderDefaults["openstack"]["startStatus"] != "RUNNING" {
		t.Fatal("no UI defaults for openstack")
	}
}
