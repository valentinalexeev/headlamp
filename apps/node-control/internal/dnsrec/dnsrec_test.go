package dnsrec

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
)

func TestFor(t *testing.T) {
	for _, c := range []struct{ name, target, typ, want string }{
		{"*.f.example.com.", "edge.example.net.", "CNAME", "edge.example.net"},
		{"*.f.example.com", "203.0.113.7", "A", "203.0.113.7"},
		{"*.f.example.com", "2001:db8::7", "AAAA", "2001:db8::7"},
		{"*.f.example.com", "::ffff:203.0.113.7", "A", "203.0.113.7"},
	} {
		r, err := For(c.name, c.target)
		if err != nil || r.Type != c.typ || r.Target != c.want || r.Name != "*.f.example.com" {
			t.Errorf("For(%q,%q) = %+v %v", c.name, c.target, r, err)
		}
	}
	if _, err := For("", "x"); err == nil {
		t.Error("empty name accepted")
	}
}

// fakeCF is the Cloudflare v4 API for one zone, enough to list, create and update records.
type fakeCF struct {
	mu      sync.Mutex
	recs    map[string]cfRecord // id -> record
	auth    string
	writes  int
	failAll bool
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.auth || f.failAll {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"Authentication error"}]}`))
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/zones/Z1/dns_records") {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"no zone"}]}`))
		return
	}
	reply := func(v any) { b, _ := json.Marshal(map[string]any{"success": true, "result": v}); _, _ = w.Write(b) }
	switch r.Method {
	case http.MethodGet:
		out := []cfRecord{}
		for _, rec := range f.recs {
			if rec.Name == r.URL.Query().Get("name") && rec.Type == r.URL.Query().Get("type") {
				out = append(out, rec)
			}
		}
		reply(out)
	case http.MethodPost, http.MethodPut:
		var in cfRecord
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := strings.TrimPrefix(r.URL.Path, "/zones/Z1/dns_records/")
		if r.Method == http.MethodPost {
			id = "rec" + string(rune('0'+len(f.recs)))
		}
		in.ID = id
		f.recs[id] = in
		f.writes++
		reply(in)
	}
}

func TestCloudflareEnsure(t *testing.T) {
	f := &fakeCF{recs: map[string]cfRecord{}, auth: "tok"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := &Cloudflare{Zone: "Z1", Token: "tok", Base: srv.URL, HTTP: srv.Client()}
	rec, _ := For("*.f.example.com", "edge.example.net")
	ctx := context.Background()

	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || f.writes != 1 {
		t.Fatalf("create: %v %v writes=%d", changed, err, f.writes)
	}
	if changed, err := Ensure(ctx, p, rec); err != nil || changed || f.writes != 1 {
		t.Fatalf("an unchanged record must not be written: %v %v writes=%d", changed, err, f.writes)
	}
	// someone changed it in the dashboard: it is put back, by update, not by a second record
	for id, r := range f.recs {
		r.Content = "elsewhere.example.org"
		f.recs[id] = r
	}
	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || len(f.recs) != 1 {
		t.Fatalf("repair: %v %v recs=%d", changed, err, len(f.recs))
	}
	for _, r := range f.recs {
		if r.Content != "edge.example.net" || r.Proxied || r.Type != "CNAME" {
			t.Fatalf("record %+v", r)
		}
	}
	// a bad token: an error that names the cause and not the token
	bad := &Cloudflare{Zone: "Z1", Token: "wrong-secret", Base: srv.URL, HTTP: srv.Client()}
	_, err := Ensure(ctx, bad, rec)
	if err == nil || !strings.Contains(err.Error(), "Authentication error") || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad token: %v", err)
	}
}

// fakeR53 is the Route 53 REST/XML API for one zone, enough for the real SDK client.
type fakeR53 struct {
	mu      sync.Mutex
	name    string // stored as Route 53 does: escaped, with a trailing dot
	typ     string
	value   string
	changes int
}

func (f *fakeR53) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
		w.WriteHeader(403)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/hostedzone/Z9/rrset"):
		sets := ""
		if f.name != "" {
			sets = `<ResourceRecordSet><Name>` + f.name + `</Name><Type>` + f.typ + `</Type><TTL>300</TTL><ResourceRecords><ResourceRecord><Value>` + f.value + `</Value></ResourceRecord></ResourceRecords></ResourceRecordSet>`
		}
		_, _ = io.WriteString(w, `<?xml version="1.0"?><ListResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ResourceRecordSets>`+sets+`</ResourceRecordSets><IsTruncated>false</IsTruncated><MaxItems>1</MaxItems></ListResourceRecordSetsResponse>`)
	case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/hostedzone/Z9/rrset") || strings.HasSuffix(r.URL.Path, "/hostedzone/Z9/rrset/")):
		var in struct {
			Changes []struct {
				Action string `xml:"Action"`
				Set    struct {
					Name   string   `xml:"Name"`
					Type   string   `xml:"Type"`
					Values []string `xml:"ResourceRecords>ResourceRecord>Value"`
				} `xml:"ResourceRecordSet"`
			} `xml:"ChangeBatch>Changes>Change"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&in)
		if len(in.Changes) != 1 || in.Changes[0].Action != "UPSERT" {
			w.WriteHeader(400)
			return
		}
		c := in.Changes[0].Set
		f.name, f.typ, f.value = c.Name, c.Type, c.Values[0]
		f.changes++
		_, _ = io.WriteString(w, `<?xml version="1.0"?><ChangeResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><ChangeInfo><Id>/change/C1</Id><Status>PENDING</Status><SubmittedAt>2026-01-01T00:00:00Z</SubmittedAt></ChangeInfo></ChangeResourceRecordSetsResponse>`)
	default:
		w.WriteHeader(404)
	}
}

func TestRoute53EnsureWithTheRealSDK(t *testing.T) {
	f := &fakeR53{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	client := route53.New(route53.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(),
		Credentials: aws.AnonymousCredentials{},
	}, func(o *route53.Options) {
		o.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "AKIDTEST", SecretAccessKey: "secret"}, nil
		})
	})
	p := &Route53{Zone: "Z9", Client: client}
	rec, _ := For("*.f.example.com", "203.0.113.7")
	ctx := context.Background()
	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || f.changes != 1 {
		t.Fatalf("create: %v %v changes=%d", changed, err, f.changes)
	}
	if f.name != `\052.f.example.com.` || f.typ != "A" || f.value != "203.0.113.7" {
		t.Fatalf("stored %q %q %q (the wildcard must be escaped as \\052)", f.name, f.typ, f.value)
	}
	if changed, err := Ensure(ctx, p, rec); err != nil || changed || f.changes != 1 {
		t.Fatalf("an unchanged record must not be written: %v %v changes=%d", changed, err, f.changes)
	}
	f.value = "198.51.100.1"
	if changed, err := Ensure(ctx, p, rec); err != nil || !changed || f.value != "203.0.113.7" {
		t.Fatalf("repair: %v %v value=%s", changed, err, f.value)
	}
}
