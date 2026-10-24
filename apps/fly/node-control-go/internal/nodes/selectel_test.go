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

	"example.com/node-control/internal/core"
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

func newSelectel(t *testing.T, f *fakeNova, env map[string]string) (*selectel, *clock) {
	t.Helper()
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	if f.token == "" {
		f.token = "tok-1"
	}
	if f.region == "" {
		f.region = "region-1"
	}
	p := NewSelectel(core.MapEnv(env), f.srv.Client()).(*selectel)
	p.authURL = f.srv.URL + "/identity/v3"
	c := &clock{t: time.Unix(1_000_000, 0)}
	p.now = c.now
	return p, c
}

var selTarget = Target{ID: "node-a", Name: "node-a", Provider: "selectel"}

func TestFindsInstanceByName(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, _ := newSelectel(t, f, selEnv)
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
	p, _ := newSelectel(t, f, map[string]string{
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
	p, _ := newSelectel(t, f, selEnv)
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
	p, _ := newSelectel(t, f, selEnv)
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
	p, c := newSelectel(t, f, selEnv)
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
		p, _ := newSelectel(t, f, env)
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
	p, _ := newSelectel(t, &fakeNova{}, env)
	if _, err := p.Status(context.Background(), selTarget); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestSelectelNoNameNotConfigured(t *testing.T) {
	p, _ := newSelectel(t, &fakeNova{}, selEnv)
	if _, err := p.Status(context.Background(), Target{ID: "x", Provider: "selectel"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestSelectelAmbiguousAndMissingInstance(t *testing.T) {
	f := &fakeNova{servers: []map[string]string{{"id": "a", "name": "node-a"}, {"id": "b", "name": "node-a"}}, status: map[string]string{}}
	p, _ := newSelectel(t, f, selEnv)
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
	p := NewSelectel(core.MapEnv(selEnv), ks.Client()).(*selectel)
	p.authURL = ks.URL
	_, err := p.Status(ctx, selTarget)
	if !errors.As(err, &ae) || ae.Status != 401 || strings.Contains(err.Error(), "pw-secret") || !strings.Contains(err.Error(), "bad login *** for svc") {
		t.Fatalf("keystone: %v", err)
	}

	// Compute failure whose body echoes the token.
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}, token: "tok-leak", serverErr: "oops tok-leak oops"}
	p2, _ := newSelectel(t, f, selEnv)
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
		p3 := NewSelectel(core.MapEnv(selEnv), big.Client()).(*selectel)
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
	p, _ := newSelectel(t, f, selEnv)
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
	p, _ := newSelectel(t, f, selEnv)
	_, err := p.Status(context.Background(), selTarget)
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "region-1") {
		t.Fatalf("%v", err)
	}
}
