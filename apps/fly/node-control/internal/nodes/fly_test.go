package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"example.com/node-control/internal/core"
)

type fakeFly struct {
	mu       sync.Mutex
	machines string
	posts    []string
	auth     string
	failPost bool
	listCode int // non-zero: GET machines answers this code
}

func (f *fakeFly) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/apps/x/machines":
		if f.listCode != 0 {
			w.WriteHeader(f.listCode)
		}
		fmt.Fprint(w, f.machines)
	case r.Method == "POST":
		f.posts = append(f.posts, r.URL.Path)
		if f.failPost {
			http.Error(w, "nope", 500)
			return
		}
		fmt.Fprint(w, `{}`)
	default:
		http.Error(w, "not found", 404)
	}
}

func newFly(t *testing.T, f *fakeFly, token string) (Provider, func()) {
	srv := httptest.NewServer(f)
	p := NewFly(core.MapEnv(map[string]string{"FLY_API_TOKEN": token}), srv.Client()).(*fly)
	p.base = srv.URL + "/v1"
	return p, srv.Close
}

var flyTarget = Target{ID: "down", Name: "down", Provider: "fly", Block: map[string]any{"app": "x"}}

func TestFlyStatusAndStartStop(t *testing.T) {
	f := &fakeFly{machines: `[{"id":"m0","state":"destroyed"},{"id":"m1","state":"stopped"}]`}
	p, closeFn := newFly(t, f, "tok")
	defer closeFn()
	ctx := context.Background()

	st, err := p.Status(ctx, flyTarget)
	if err != nil || st["status"] != "stopped" || st["machine_id"] != "m1" {
		t.Fatalf("%v %v", st, err)
	}
	if f.auth != "Bearer tok" {
		t.Fatalf("auth %q", f.auth)
	}
	prev, cur, err := p.Start(ctx, flyTarget)
	if err != nil || prev != "stopped" || cur != "starting" {
		t.Fatal(prev, cur, err)
	}
	if len(f.posts) != 1 || f.posts[0] != "/v1/apps/x/machines/m1/start" {
		t.Fatalf("%v", f.posts)
	}
	// idempotent stop of a stopped machine
	prev, cur, err = p.Stop(ctx, flyTarget)
	if err != nil || prev != "stopped" || cur != "stopped" || len(f.posts) != 1 {
		t.Fatal(prev, cur, err, f.posts)
	}

	f.machines = `[{"id":"m1","state":"started"}]`
	prev, cur, _ = p.Start(ctx, flyTarget)
	if prev != "started" || cur != "started" || len(f.posts) != 1 {
		t.Fatal(prev, cur, f.posts)
	}
	prev, cur, _ = p.Stop(ctx, flyTarget)
	if prev != "started" || cur != "stopping" || f.posts[1] != "/v1/apps/x/machines/m1/stop" {
		t.Fatal(prev, cur, f.posts)
	}
}

func TestFlyErrors(t *testing.T) {
	ctx := context.Background()
	f := &fakeFly{machines: `[{"id":"m0","state":"destroyed"}]`}
	p, closeFn := newFly(t, f, "tok")
	defer closeFn()
	var ae *APIError
	if _, err := p.Status(ctx, flyTarget); !errors.As(err, &ae) {
		t.Fatalf("no machines: %v", err)
	}
	f.machines = `[{"id":"m1","state":"stopped"}]`
	f.failPost = true
	if _, _, err := p.Start(ctx, flyTarget); !errors.As(err, &ae) {
		t.Fatalf("post failure: %v", err)
	}
	if _, err := p.Status(ctx, Target{ID: "d", Name: "missing-app", Provider: "fly"}); !errors.As(err, &ae) {
		t.Fatalf("404: %v", err)
	}

	p2, c2 := newFly(t, f, "")
	defer c2()
	_, err := p2.Status(ctx, flyTarget)
	if !errors.Is(err, ErrNotConfigured) || err.Error() != "FLY_API_TOKEN is not set" {
		t.Fatalf("%v", err)
	}
}

func TestFlyAppDefaultsToTargetName(t *testing.T) {
	f := &fakeFly{machines: `[{"id":"m1","state":"started"}]`}
	p, closeFn := newFly(t, f, "tok")
	defer closeFn()
	st, err := p.Status(context.Background(), Target{ID: "x", Name: "x", Provider: "fly"})
	if err != nil || st["status"] != "started" {
		t.Fatal(st, err)
	}
}

func TestFlyErrorBodyIsScrubbedAndBounded(t *testing.T) {
	f := &fakeFly{listCode: 500, machines: "tok " + strings.Repeat("\u00e9", 3000)}
	p, closeFn := newFly(t, f, "tok")
	defer closeFn()
	_, err := p.Status(context.Background(), flyTarget)
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("%v", err)
	}
	if len(ae.Message) > 600 || !utf8.ValidString(ae.Message) || strings.Contains(ae.Message, "tok ") || !strings.Contains(ae.Message, "***") {
		t.Fatalf("%d bytes: %.80q", len(ae.Message), ae.Message)
	}
}
