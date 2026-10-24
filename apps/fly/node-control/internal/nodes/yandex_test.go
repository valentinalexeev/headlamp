package nodes

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"example.com/node-control/internal/core"
)

const (
	yxToken = "fake-iam-token-0123456789abcdef"
	yxEpoch = 1_000_000
)

var (
	yxKeyOnce sync.Once
	yxKey     *rsa.PrivateKey
)

// yxTestKey generates a throwaway RSA key at runtime; no key material is
// stored in the repository.
func yxTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	yxKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		yxKey = k
	})
	return yxKey
}

func yxPEM(t *testing.T) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, yxTestKey(t))}))
}

func mustPKCS8(t *testing.T, k *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func yxKeyJSON(t *testing.T, mutate func(m map[string]string)) string {
	t.Helper()
	m := map[string]string{"id": "fake-key-id", "service_account_id": "fake-sa-id", "private_key": yxPEM(t)}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func yxEnv(t *testing.T) map[string]string {
	return map[string]string{"YANDEX_SA_KEY_JSON": yxKeyJSON(t, nil), "YANDEX_FOLDER_ID": "fake-folder"}
}

type yxInstance struct{ ID, Name, Status string }

type fakeYandex struct {
	mu        sync.Mutex
	srv       *httptest.Server
	clock     *clock
	instances []yxInstance
	iamCode   int
	iamBody   string // error body for a failing IAM call; {JWT} is replaced
	getErr    string // body of a 500 on GET /instances/{id}; {JWT} is replaced
	postCode  int
	noStatus  bool
	jwts      []string
	iamCalls  int
	lists     []string // raw query of list calls
	posts     []string
	auths     []string
}

func (f *fakeYandex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lastJWT := ""
	if len(f.jwts) > 0 {
		lastJWT = f.jwts[len(f.jwts)-1]
	}
	if r.URL.Path == "/iam/v1/tokens" {
		var body struct {
			JWT string `json:"jwt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.iamCalls++
		f.jwts = append(f.jwts, body.JWT)
		if f.iamCode != 0 {
			http.Error(w, strings.ReplaceAll(f.iamBody, "{JWT}", body.JWT), f.iamCode)
			return
		}
		exp := f.clock.now().Add(time.Hour).UTC().Format(time.RFC3339)
		_ = json.NewEncoder(w).Encode(map[string]string{"iamToken": yxToken, "expiresAt": exp})
		return
	}
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	if r.Header.Get("Authorization") != "Bearer "+yxToken {
		http.Error(w, "unauthenticated", 401)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/instances")
	switch {
	case r.Method == http.MethodGet && rest == "":
		f.lists = append(f.lists, r.URL.RawQuery)
		filter := r.URL.Query().Get("filter")
		var out []map[string]string
		for _, i := range f.instances {
			if filter == `name="`+i.Name+`"` {
				out = append(out, map[string]string{"id": i.ID, "name": i.Name, "status": i.Status})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": out})
	case r.Method == http.MethodGet:
		if f.getErr != "" {
			http.Error(w, strings.ReplaceAll(f.getErr, "{JWT}", lastJWT), 500)
			return
		}
		id := strings.TrimPrefix(rest, "/")
		for _, i := range f.instances {
			if i.ID == id {
				doc := map[string]string{"id": i.ID, "name": i.Name}
				if !f.noStatus {
					doc["status"] = i.Status
				}
				_ = json.NewEncoder(w).Encode(doc)
				return
			}
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost:
		f.posts = append(f.posts, r.URL.Path)
		if f.postCode != 0 {
			http.Error(w, "conflict", f.postCode)
			return
		}
		_, _ = w.Write([]byte(`{"id":"op-1","done":false}`))
	default:
		http.NotFound(w, r)
	}
}

func newYandex(t *testing.T, f *fakeYandex, env map[string]string) (*yandex, *clock) {
	t.Helper()
	f.clock = &clock{t: time.Unix(yxEpoch, 0)}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	p := NewYandex(core.MapEnv(env), f.srv.Client()).(*yandex)
	p.iamURL = f.srv.URL + "/iam/v1/tokens"
	p.computeURL = f.srv.URL + "/instances"
	p.now = f.clock.now
	return p, f.clock
}

var yxTarget = Target{ID: "node-a", Name: "web", Provider: "yandex"}

func yxBlock(b map[string]any) Target {
	t := yxTarget
	t.Block = b
	return t
}

func TestJWTVerifiesWithPublicKey(t *testing.T) {
	f := &fakeYandex{instances: []yxInstance{{"i1", "web", "RUNNING"}}}
	p, c := newYandex(t, f, yxEnv(t))
	if _, err := p.Status(context.Background(), yxTarget); err != nil {
		t.Fatal(err)
	}
	if len(f.jwts) != 1 {
		t.Fatalf("iam calls: %d", len(f.jwts))
	}
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(f.jwts[0], claims,
		func(*jwt.Token) (any, error) { return &yxTestKey(t).PublicKey, nil },
		jwt.WithValidMethods([]string{"PS256"}), jwt.WithTimeFunc(c.now),
		jwt.WithIssuedAt(), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid {
		t.Fatalf("jwt did not verify: %v", err)
	}
	if tok.Header["kid"] != "fake-key-id" || tok.Header["alg"] != "PS256" {
		t.Fatalf("header: %v", tok.Header)
	}
	if claims["iss"] != "fake-sa-id" || claims["aud"] != "https://iam.api.cloud.yandex.net/iam/v1/tokens" {
		t.Fatalf("claims: %v", claims)
	}
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	if int64(iat) != yxEpoch || int64(exp) != yxEpoch+3600 {
		t.Fatalf("iat/exp: %v %v", iat, exp)
	}
	// A different key must not verify it.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := jwt.Parse(f.jwts[0], func(*jwt.Token) (any, error) { return &other.PublicKey, nil },
		jwt.WithValidMethods([]string{"PS256"}), jwt.WithTimeFunc(c.now)); err == nil {
		t.Fatal("verified with the wrong key")
	}
}

func TestYandexTokenCached(t *testing.T) {
	f := &fakeYandex{instances: []yxInstance{{"i1", "web", "RUNNING"}}}
	p, c := newYandex(t, f, yxEnv(t))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := p.Status(ctx, yxTarget); err != nil {
			t.Fatal(err)
		}
	}
	if f.iamCalls != 1 {
		t.Fatalf("iam calls = %d, want 1", f.iamCalls)
	}
	c.add(54 * time.Minute) // 6 minutes left: still cached
	if _, err := p.Status(ctx, yxTarget); err != nil || f.iamCalls != 1 {
		t.Fatalf("%v calls=%d", err, f.iamCalls)
	}
	c.add(2 * time.Minute) // 4 minutes left: refresh
	if _, err := p.Status(ctx, yxTarget); err != nil || f.iamCalls != 2 {
		t.Fatalf("%v calls=%d", err, f.iamCalls)
	}
}

func TestFindByNameInFolder(t *testing.T) {
	f := &fakeYandex{instances: []yxInstance{{"i1", "web", "STOPPED"}, {"i2", "other", "RUNNING"}}}
	p, _ := newYandex(t, f, yxEnv(t))
	st, err := p.Status(context.Background(), yxTarget)
	if err != nil || st["status"] != "STOPPED" {
		t.Fatalf("%v %v", st, err)
	}
	if len(f.lists) != 1 || !strings.Contains(f.lists[0], "folderId=fake-folder") ||
		!strings.Contains(f.lists[0], "filter=name%3D%22web%22") {
		t.Fatalf("list query: %v", f.lists)
	}
	// instance_name overrides Target.Name.
	st, err = p.Status(context.Background(), yxBlock(map[string]any{"instance_name": "other"}))
	if err != nil || st["status"] != "RUNNING" {
		t.Fatalf("%v %v", st, err)
	}
}

func TestYandexNoneAmbiguousAndHostileNames(t *testing.T) {
	ctx := context.Background()
	var ae *APIError
	f := &fakeYandex{instances: []yxInstance{{"i1", "dup", "RUNNING"}, {"i2", "dup", "STOPPED"}}}
	p, _ := newYandex(t, f, yxEnv(t))
	_, err := p.Status(ctx, Target{Name: "nope"})
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "no instance named 'nope' in the folder") {
		t.Fatalf("%v", err)
	}
	_, err = p.Status(ctx, Target{Name: "dup"})
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "2 instances named 'dup'; refusing to guess") {
		t.Fatalf("%v", err)
	}
	for _, name := range []string{`a"b`, `a\b`} {
		before := len(f.lists)
		_, err = p.Status(ctx, Target{Name: name})
		if !errors.Is(err, ErrNotConfigured) && !errors.As(err, &ae) {
			t.Fatalf("%q: %v", name, err)
		}
		if len(f.lists) != before {
			t.Fatalf("%q reached the API", name)
		}
	}
}

func TestYandexPinnedIDSkipsLookup(t *testing.T) {
	f := &fakeYandex{instances: []yxInstance{{"i9", "x", "RUNNING"}}}
	env := yxEnv(t)
	delete(env, "YANDEX_FOLDER_ID") // not needed when pinned
	p, _ := newYandex(t, f, env)
	st, err := p.Status(context.Background(), yxBlock(map[string]any{"instance_id": "i9"}))
	if err != nil || st["status"] != "RUNNING" || len(f.lists) != 0 {
		t.Fatalf("%v %v lists=%v", st, err, f.lists)
	}
}

func TestYandexStatusVocabulary(t *testing.T) {
	ctx := context.Background()
	for _, s := range []string{"RUNNING", "STOPPED", "STARTING", "STOPPING", "PROVISIONING", "ERROR"} {
		f := &fakeYandex{instances: []yxInstance{{"i1", "web", s}}}
		p, _ := newYandex(t, f, yxEnv(t))
		st, err := p.Status(ctx, yxTarget)
		if err != nil || st["status"] != s {
			t.Fatalf("%s: %v %v", s, st, err)
		}
	}
	f := &fakeYandex{instances: []yxInstance{{"i1", "web", "RUNNING"}}, noStatus: true}
	p, _ := newYandex(t, f, yxEnv(t))
	pin := yxBlock(map[string]any{"instance_id": "i1"})
	if st, err := p.Status(ctx, pin); err != nil || st["status"] != "UNKNOWN" {
		t.Fatalf("missing status: %v %v", st, err)
	}
}

func TestYandexStartStopIdempotent(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, state       string
		start             bool
		wantPrev, wantCur string
		wantPost          string
	}{
		{"start stopped", "STOPPED", true, "STOPPED", "STARTING", "/instances/i1:start"},
		{"start running", "RUNNING", true, "RUNNING", "RUNNING", ""},
		{"start starting", "STARTING", true, "STARTING", "STARTING", ""},
		{"stop running", "RUNNING", false, "RUNNING", "STOPPING", "/instances/i1:stop"},
		{"stop stopped", "STOPPED", false, "STOPPED", "STOPPED", ""},
		{"stop stopping", "STOPPING", false, "STOPPING", "STOPPING", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeYandex{instances: []yxInstance{{"i1", "web", tc.state}}}
			p, _ := newYandex(t, f, yxEnv(t))
			var prev, cur string
			var err error
			if tc.start {
				prev, cur, err = p.Start(ctx, yxTarget)
			} else {
				prev, cur, err = p.Stop(ctx, yxTarget)
			}
			if err != nil || prev != tc.wantPrev || cur != tc.wantCur {
				t.Fatalf("%q %q %v", prev, cur, err)
			}
			if tc.wantPost == "" && len(f.posts) != 0 || tc.wantPost != "" && (len(f.posts) != 1 || f.posts[0] != tc.wantPost) {
				t.Fatalf("posts: %v", f.posts)
			}
		})
	}
	t.Run("action failure", func(t *testing.T) {
		f := &fakeYandex{instances: []yxInstance{{"i1", "web", "STOPPED"}}, postCode: 409}
		p, _ := newYandex(t, f, yxEnv(t))
		var ae *APIError
		_, _, err := p.Start(ctx, yxTarget)
		if !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatalf("%v", err)
		}
	})
}

func TestBadKeyJSONNotConfigured(t *testing.T) {
	ctx := context.Background()
	const marker = "MARKER-must-not-echo"
	cases := map[string]string{
		"unset":          "",
		"not json":       marker + " {{{",
		"missing id":     yxKeyJSON(t, func(m map[string]string) { delete(m, "id"); m["x"] = marker }),
		"missing sa":     yxKeyJSON(t, func(m map[string]string) { delete(m, "service_account_id"); m["x"] = marker }),
		"missing key":    yxKeyJSON(t, func(m map[string]string) { delete(m, "private_key"); m["x"] = marker }),
		"unparsable key": yxKeyJSON(t, func(m map[string]string) { m["private_key"] = marker }),
	}
	for name, kj := range cases {
		t.Run(name, func(t *testing.T) {
			env := yxEnv(t)
			env["YANDEX_SA_KEY_JSON"] = kj
			f := &fakeYandex{instances: []yxInstance{{"i1", "web", "RUNNING"}}}
			p, _ := newYandex(t, f, env)
			_, err := p.Status(ctx, yxTarget)
			if !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("%v", err)
			}
			if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "fake-sa-id") {
				t.Fatalf("key content echoed: %v", err)
			}
		})
	}
	t.Run("missing folder", func(t *testing.T) {
		env := yxEnv(t)
		delete(env, "YANDEX_FOLDER_ID")
		p, _ := newYandex(t, &fakeYandex{}, env)
		if _, err := p.Status(ctx, yxTarget); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no name", func(t *testing.T) {
		p, _ := newYandex(t, &fakeYandex{}, yxEnv(t))
		if _, err := p.Status(ctx, Target{}); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%v", err)
		}
	})
}

func TestYandexErrorsAreBoundedAndScrubbed(t *testing.T) {
	ctx := context.Background()
	var ae *APIError
	inst := []yxInstance{{"i1", "web", "RUNNING"}}
	pin := yxBlock(map[string]any{"instance_id": "i1"})

	t.Run("compute body echoing the iam token early", func(t *testing.T) {
		f := &fakeYandex{instances: inst, getErr: "oops " + yxToken + " oops"}
		p, _ := newYandex(t, f, yxEnv(t))
		_, err := p.Status(ctx, pin)
		if !errors.As(err, &ae) || ae.Status != 500 || strings.Contains(err.Error(), yxToken) || !strings.Contains(err.Error(), "oops ***") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("compute body echoing the jwt early", func(t *testing.T) {
		f := &fakeYandex{instances: inst, getErr: "oops {JWT} oops"}
		p, _ := newYandex(t, f, yxEnv(t))
		_, err := p.Status(ctx, pin)
		if err == nil || strings.Contains(err.Error(), f.jwts[0][:40]) || !strings.Contains(err.Error(), "oops ***") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("iam failure echoing the jwt", func(t *testing.T) {
		f := &fakeYandex{instances: inst, iamCode: 400, iamBody: "bad {JWT} bad"}
		p, _ := newYandex(t, f, yxEnv(t))
		_, err := p.Status(ctx, pin)
		if !errors.As(err, &ae) || ae.Status != 400 || strings.Contains(err.Error(), f.jwts[0][:40]) || !strings.Contains(err.Error(), "bad ***") {
			t.Fatalf("%v", err)
		}
	})

	t.Run("body echoing the key json and private key", func(t *testing.T) {
		env := yxEnv(t)
		pemText := yxPEM(t)
		for name, secret := range map[string]string{"pem": pemText, "json": env["YANDEX_SA_KEY_JSON"]} {
			f := &fakeYandex{instances: inst, getErr: "key=" + secret + "=end"}
			p, _ := newYandex(t, f, env)
			_, err := p.Status(ctx, pin)
			if err == nil || strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), strings.Split(pemText, "\n")[2]) {
				t.Fatalf("%s leaked: %v", name, err)
			}
		}
	})

	t.Run("token straddling the cut", func(t *testing.T) {
		for _, pad := range []int{500, 505, 511, 512} {
			f := &fakeYandex{instances: inst, getErr: strings.Repeat("x", pad) + yxToken + strings.Repeat("y", 5000)}
			p, _ := newYandex(t, f, yxEnv(t))
			_, err := p.Status(ctx, pin)
			if err == nil || len(err.Error()) > 700 || strings.Contains(err.Error(), yxToken[:8]) {
				t.Fatalf("pad %d: %v", pad, err)
			}
		}
	})

	t.Run("UTF-8 cut lands mid-rune", func(t *testing.T) {
		// 511 ASCII bytes then 2-byte runes: byte 512 is the second half of a rune.
		f := &fakeYandex{instances: inst, getErr: strings.Repeat("x", 511) + strings.Repeat("é", 100)}
		p, _ := newYandex(t, f, yxEnv(t))
		_, err := p.Status(ctx, pin)
		if err == nil || !utf8.ValidString(err.Error()) || strings.Contains(err.Error(), "é") {
			t.Fatalf("%q", err)
		}
	})
}
