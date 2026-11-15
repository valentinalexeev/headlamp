package dnsrec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeCloudDNS is the Cloud DNS v1 rrsets API for one managed zone (project p1, zone z1).
type fakeCloudDNS struct {
	mu     sync.Mutex
	sets   map[string]gcdRRSet // name|type
	writes int
}

func (f *fakeCloudDNS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`))
		return
	}
	const zone = "/projects/p1/managedZones/z1/rrsets"
	path, _ := url.PathUnescape(r.URL.EscapedPath())
	if !strings.HasPrefix(path, zone) {
		w.WriteHeader(404)
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(path, zone), "/")
	var in gcdRRSet
	if r.Method != http.MethodGet {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			w.WriteHeader(400)
			return
		}
	}
	switch {
	case r.Method == http.MethodPost && rest == "":
		if _, dup := f.sets[in.Name+"|"+in.Type]; dup {
			w.WriteHeader(409)
			return
		}
		f.sets[in.Name+"|"+in.Type] = in
		f.writes++
	case r.Method == http.MethodGet || r.Method == http.MethodPatch:
		name, typ, ok := strings.Cut(rest, "/")
		cur, found := f.sets[name+"|"+typ]
		if !ok || !found {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"The 'parameters.name' resource named 'x' does not exist."}}`))
			return
		}
		if r.Method == http.MethodPatch {
			if in.Name != name || in.Type != typ {
				w.WriteHeader(400)
				return
			}
			cur = in
			f.sets[name+"|"+typ] = in
			f.writes++
		}
		_ = json.NewEncoder(w).Encode(cur)
		return
	default:
		w.WriteHeader(405)
		return
	}
	_ = json.NewEncoder(w).Encode(in)
}

func TestGoogleCloudDNSEnsure(t *testing.T) {
	other := gcdRRSet{Name: "www.f.example.com.", Type: "A", TTL: 60, RRDatas: []string{"192.0.2.9"}}
	f := &fakeCloudDNS{sets: map[string]gcdRRSet{"www.f.example.com.|A": other}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	tok := func(context.Context) (string, error) { return "tok", nil }
	p := &GoogleCloudDNS{Project: "p1", Zone: "z1", Base: srv.URL, HTTP: srv.Client(), Token: tok}
	ctx := context.Background()
	rec, _ := For("*.f.example.com", "edge.example.net")

	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || f.writes != 1 {
		t.Fatalf("create: %v %v writes=%d", changed, err, f.writes)
	}
	got := f.sets["*.f.example.com.|CNAME"]
	if got.TTL != 300 || len(got.RRDatas) != 1 || got.RRDatas[0] != "edge.example.net." {
		t.Fatalf("a CNAME target ends in a dot: %+v", got)
	}
	if changed, err := Ensure(ctx, p, rec); err != nil || changed || f.writes != 1 {
		t.Fatalf("an unchanged record must not be written: %v %v writes=%d", changed, err, f.writes)
	}
	// someone changed it in the console: it is put back by PATCH, not a second record set
	got.RRDatas = []string{"elsewhere.example.org."}
	f.sets["*.f.example.com.|CNAME"] = got
	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || f.writes != 2 || len(f.sets) != 2 {
		t.Fatalf("repair: %v %v writes=%d sets=%d", changed, err, f.writes, len(f.sets))
	}
	if f.sets["*.f.example.com.|CNAME"].RRDatas[0] != "edge.example.net." {
		t.Fatalf("not repaired: %+v", f.sets)
	}
	// an address target: an A record, no trailing dot
	a, _ := For("*.g.example.com", "198.51.100.7")
	if _, err := Ensure(ctx, p, a); err != nil || f.sets["*.g.example.com.|A"].RRDatas[0] != "198.51.100.7" {
		t.Fatalf("A: %v %+v", err, f.sets)
	}
	if f.sets["www.f.example.com.|A"].RRDatas[0] != "192.0.2.9" {
		t.Fatal("another record of the zone was changed")
	}

	// a bad token: the API message, and not the token
	bad := &GoogleCloudDNS{Project: "p1", Zone: "z1", Base: srv.URL, HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return "wrong-secret", nil }}
	if _, err := Ensure(ctx, bad, rec); err == nil || !strings.Contains(err.Error(), "invalid authentication") ||
		strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad token: %v", err)
	}
	// no credentials
	none := &GoogleCloudDNS{Project: "p1", Zone: "z1", Base: srv.URL, HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return "", errors.New("could not find default credentials") }}
	if _, err := Ensure(ctx, none, rec); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("no credentials: %v", err)
	}
	// a wrong zone is a 404 on the read and then on the create: an error, not "in sync"
	wrong := &GoogleCloudDNS{Project: "p1", Zone: "nope", Base: srv.URL, HTTP: srv.Client(), Token: tok}
	if _, err := Ensure(ctx, wrong, rec); err == nil {
		t.Fatal("a missing zone must fail")
	}
}
