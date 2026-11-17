package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// ADR 0006 (#395): no route that a tagged node can reach mints, returns or forwards a pre-auth key, an API key or
// an admin credential. The test runs the full module set of the "attention" scenario behind the production
// Authorizer and walks every admin route as a tagged caller, with each machine tag the modules know, and the public
// routes without a login. It checks:
//
//   - each admin route answers 403, except the KindOpen routes (health, static files) and the KindMachine routes of
//     the caller's own tag;
//   - each KindMachine route is on machineRoutes, so a new machine route needs a decision;
//   - no answer of any route has the text of a pre-auth key or an API key;
//   - the fake headscale mints no key during the walk.
//
// A new admin route already needs a Kind in authz.Routes (core.Mux.Seal); this test makes that Kind safe for keys.

// machineRoutes are the admin routes for tagged nodes, by the tag that may call them. None returns a key: a new
// one must be checked against ADR 0006 before it is added here.
var machineRoutes = map[string]string{
	"GET /ingress/v1/config": "tag:funnel-ingress", // the funnel route table: names, ports, targets
	"POST /ingress/v1/state": "tag:funnel-ingress", // the ingress report; the answer is empty
	"POST /serve/v1/cert":    "tag:serve",          // a TLS certificate for a CSR; the node keeps its own key
}

// machineTags are the tags the walk calls as: those of the machine routes, and other node tags of the demo.
var machineTags = []string{"tag:funnel-ingress", "tag:serve", "tag:ingress", "tag:exit", "tag:task-runner",
	"tag:node-control"}

// publicRoutes are the public enrolment routes, called without a login or a session.
var publicRoutes = []string{"GET /me", "POST /me/devices", "GET /me/devices/1", "GET /e/x", "GET /sub/x",
	"GET /i/x", "GET /oidc/login", "GET /oidc/callback"}

// keyText matches what a key looks like in an answer: the fake headscale's pre-auth keys, and real headscale and
// Tailscale keys.
var keyText = regexp.MustCompile(`demo-key-\d{4}|tskey-[a-z]+-[A-Za-z0-9]{8,}|hskey-[a-z]+-[A-Za-z0-9_-]{8,}`)

var pathParam = regexp.MustCompile(`\{[a-z]+\}`)

func TestNoKeyRouteForTaggedNodes(t *testing.T) {
	tn, admin := startGated(t)
	checkMachineList(t)
	keysBefore := tn.world.hsState.keyCount()
	for _, tag := range machineTags {
		walkAdmin(t, tn, admin, tag)
	}
	tn.tagged.Store("")
	walkPublic(t, tn.publicAddr())
	if n := tn.world.hsState.keyCount(); n != keysBefore {
		t.Errorf("tagged and anonymous callers minted %d pre-auth key(s)", n-keysBefore)
	}
}

// startGated runs the attention scenario with the Authorizer of node-control (the transition mode on, as in
// cmd/node-control's newAuthorizer) and returns its tailnet and admin address once seeded.
func startGated(t *testing.T) (*demoTailnet, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	tns := make(chan *demoTailnet, 1)
	hook := func(d *demoTailnet) {
		d.authorize = func(p *authz.Policy) core.Authorizer {
			return &authz.Authorizer{Policy: p, Tailnet: d, Open: true}
		}
		tns <- d
	}
	go func() {
		done <- serveWith(ctx, options{addr: "127.0.0.1:0", scenario: "attention"}, scenarios["attention"],
			hook, func(a string) { addrs <- a })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the demo stopped with: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("the demo did not stop")
		}
	})
	select {
	case a := <-addrs:
		return <-tns, a
	case err := <-done:
		t.Fatalf("the demo stopped: %v", err)
	case <-time.After(150 * time.Second):
		t.Fatal("the demo was not ready in time")
	}
	return nil, ""
}

// checkMachineList keeps machineRoutes and the KindMachine routes of authz.Routes the same set.
func checkMachineList(t *testing.T) {
	t.Helper()
	for p, r := range authz.Routes {
		if _, listed := machineRoutes[p]; r.Kind == authz.KindMachine && !listed {
			t.Errorf("machine route %q is not on the list of ADR 0006: check that it returns no key, then add it", p)
		}
	}
	for p := range machineRoutes {
		if r, ok := authz.Routes[p]; !ok || r.Kind != authz.KindMachine {
			t.Errorf("%q is on the machine list but is not a KindMachine route", p)
		}
	}
}

// walkAdmin calls every admin route as a node with tag.
func walkAdmin(t *testing.T, tn *demoTailnet, admin, tag string) {
	t.Helper()
	tn.tagged.Store(tag)
	for p, r := range authz.Routes {
		method, path, _ := strings.Cut(p, " ")
		path = pathParam.ReplaceAllString(strings.ReplaceAll(path, "{$}", ""), "x")
		code, body := probeRoute(t, admin, method, path)
		if keyText.Match(body) {
			t.Errorf("%s as %s: the answer has a key", p, tag)
		}
		switch {
		case r.Kind == authz.KindOpen:
			// health and static files: no data
		case r.Kind == authz.KindMachine && machineRoutes[p] == tag:
			if code == http.StatusForbidden {
				t.Errorf("%s as %s: its own machine route answers 403", p, tag)
			}
		case code != http.StatusForbidden:
			t.Errorf("%s as %s: want 403, got %d", p, tag, code)
		}
	}
}

// walkPublic calls the public routes with no login. They mint a key only for a signed-in person (self-enrolment,
// CSRF form); without a session they mint nothing and show none.
func walkPublic(t *testing.T, pub string) {
	t.Helper()
	if pub == "" {
		t.Fatal("the public listener is not up")
	}
	for _, p := range publicRoutes {
		method, path, _ := strings.Cut(p, " ")
		if _, body := probeRoute(t, pub, method, path); keyText.Match(body) {
			t.Errorf("public %s: the answer has a key", p)
		}
	}
}

func probeRoute(t *testing.T, addr, method, path string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		body = bytes.NewReader([]byte(`{}`))
	}
	req, err := http.NewRequest(method, "http://"+addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return resp.StatusCode, b
}
