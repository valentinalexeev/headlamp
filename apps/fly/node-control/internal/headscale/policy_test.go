package headscale

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type seen struct{ method, path, auth, body string }

// recorder serves a canned answer and records the last request.
func recorder(t *testing.T, status int, answer string) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)}
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "k1"}, got
}

func TestGetPolicyReturnsTextAndUpdatedAt(t *testing.T) {
	c, got := recorder(t, 200, `{"policy":"{\"a\":1}","updatedAt":"2026-01-02T03:04:05Z"}`)
	text, at, err := c.GetPolicy(context.Background())
	if err != nil || text != `{"a":1}` || at != "2026-01-02T03:04:05Z" {
		t.Fatalf("%q %q %v", text, at, err)
	}
	if got.method != "GET" || got.path != "/api/v1/policy" || got.auth != "Bearer k1" {
		t.Fatalf("%+v", got)
	}
}

func TestSetPolicySendsExactBody(t *testing.T) {
	c, got := recorder(t, 200, `{}`)
	if err := c.SetPolicy(context.Background(), "{\n \"x\": \"é\"}"); err != nil {
		t.Fatal(err)
	}
	if got.method != "PUT" || got.path != "/api/v1/policy" || got.auth != "Bearer k1" {
		t.Fatalf("%+v", got)
	}
	if got.body != `{"policy":"{\n \"x\": \"é\"}"}` {
		t.Fatalf("body %q", got.body)
	}
}

func TestSetPolicy400ReturnsAPIErrorWithHeadscaleMessage(t *testing.T) {
	c, _ := recorder(t, 400, `{"code":3,"message":"parsing policy: bad tag tag:example","details":[]}`)
	err := c.SetPolicy(context.Background(), "x")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "parsing policy: bad tag tag:example" {
		t.Fatalf("%#v", err)
	}
	if err.Error() != "headscale API 400: parsing policy: bad tag tag:example" {
		t.Fatal(err.Error())
	}
}

func TestCheckPolicy400(t *testing.T) {
	c, got := recorder(t, 400, `{"code":3,"message":"nope"}`)
	err := c.CheckPolicy(context.Background(), "{}")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Message != "nope" {
		t.Fatalf("%#v", err)
	}
	if got.method != "POST" || got.path != "/api/v1/policy/check" || got.body != `{"policy":"{}"}` {
		t.Fatalf("%+v", got)
	}
	c, _ = recorder(t, 200, `{}`)
	if err := c.CheckPolicy(context.Background(), "{}"); err != nil {
		t.Fatal(err)
	}
}

func TestAPIErrorFallbacks(t *testing.T) {
	c, _ := recorder(t, 404, "  plain text \n")
	var ae *APIError
	if err := c.SetPolicy(context.Background(), "x"); !errors.As(err, &ae) || ae.Message != "plain text" {
		t.Fatalf("%#v", err)
	}
	c, _ = recorder(t, 400, "")
	if err := c.SetPolicy(context.Background(), "x"); !errors.As(err, &ae) || ae.Message != "Bad Request" {
		t.Fatalf("%#v", err)
	}
	c, _ = recorder(t, 400, strings.Repeat("a", 5000))
	if err := c.SetPolicy(context.Background(), "x"); !errors.As(err, &ae) || len(ae.Message) > 2048 {
		t.Fatalf("%d", len(ae.Message))
	}
}

func TestStatus401And5xxAndNetworkErrorsAreDistinct(t *testing.T) {
	var ae *APIError
	for _, st := range []int{401, 403} {
		c, _ := recorder(t, st, `{"message":"unauthorized"}`)
		if err := c.SetPolicy(context.Background(), "x"); !errors.As(err, &ae) || ae.Status != st {
			t.Fatalf("%d: %#v", st, err)
		}
	}
	c, _ := recorder(t, 500, `{"message":"boom"}`)
	err := c.SetPolicy(context.Background(), "x")
	if errors.As(err, &ae) || err == nil || !strings.Contains(err.Error(), "setting the policy") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("%v", err)
	}
	c = &Client{BaseURL: "http://127.0.0.1:1", APIKey: "k1"}
	err = c.SetPolicy(context.Background(), "x")
	if errors.As(err, &ae) || err == nil || !strings.Contains(err.Error(), "setting the policy") {
		t.Fatalf("%v", err)
	}
}

func TestMissingAPIKeyFailsBeforeAnyRequest(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()
	_, _, e1 := c.GetPolicy(ctx)
	_, e3 := c.ListUsers(ctx)
	_, e4 := c.CreatePreAuthKey(ctx, 1, nil, 0, false, false)
	_, e5 := c.ListPreAuthKeys(ctx)
	for i, err := range []error{e1, c.SetPolicy(ctx, "x"), c.CheckPolicy(ctx, "x"), e3, e4, e5, c.ExpirePreAuthKey(ctx, 1)} {
		if !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if n.Load() != 0 {
		t.Fatalf("%d requests", n.Load())
	}
}

func TestNoAPIKeyInAnyErrorString(t *testing.T) {
	const key = "hskey-TESTSECRET"
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		body   string
	}{{400, `{"message":"bad"}`}, {401, "denied"}, {500, "internal " + key}, {200, "not json"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		c := &Client{BaseURL: srv.URL, APIKey: key}
		_, _, e1 := c.GetPolicy(ctx)
		_, e3 := c.ListUsers(ctx)
		_, e4 := c.CreatePreAuthKey(ctx, 1, []string{"tag:example"}, 3600e9, false, false)
		_, e5 := c.ListPreAuthKeys(ctx)
		for _, err := range []error{e1, c.SetPolicy(ctx, "x"), c.CheckPolicy(ctx, "x"), e3, e4, e5, c.ExpirePreAuthKey(ctx, 1)} {
			if err != nil && strings.Contains(err.Error(), key) {
				t.Fatalf("status %d leaks key: %v", tc.status, err)
			}
		}
		srv.Close()
	}
	dead := &Client{BaseURL: "http://127.0.0.1:1", APIKey: key}
	_, _, e1 := dead.GetPolicy(ctx)
	if e1 == nil || strings.Contains(e1.Error(), key) {
		t.Fatal(e1)
	}
}
