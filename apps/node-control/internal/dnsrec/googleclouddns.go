package dnsrec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GoogleCloudDNS writes the record in one Cloud DNS managed zone (#360), with the Cloud DNS v1
// resource record set API. Token gives an OAuth access token with the scope
// ndev.clouddns.readwrite (Application Default Credentials: the VM's service account on GCE).
// The service account needs roles/dns.admin on that one zone.
type GoogleCloudDNS struct {
	Project string // project of the zone
	Zone    string // managed zone name (not the DNS name)
	Base    string // default https://dns.googleapis.com/dns/v1
	HTTP    *http.Client
	Token   func(ctx context.Context) (string, error)
}

// gcdMaxBody caps an API answer: one record set or an error.
const gcdMaxBody = 1 << 20

// GoogleCloudDNSScope is the OAuth scope the token needs.
const GoogleCloudDNSScope = "https://www.googleapis.com/auth/ndev.clouddns.readwrite"

func (g *GoogleCloudDNS) Name() string { return "googleclouddns" }

type gcdRRSet struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int      `json:"ttl"`
	RRDatas []string `json:"rrdatas"`
}

func (g *GoogleCloudDNS) base() string {
	if g.Base != "" {
		return strings.TrimRight(g.Base, "/")
	}
	return "https://dns.googleapis.com/dns/v1"
}

// Cloud DNS names and CNAME targets end in a dot.
func fqdn(n string) string { return strings.TrimSuffix(n, ".") + "." }

func (g *GoogleCloudDNS) rrsets() string {
	return g.base() + "/projects/" + url.PathEscape(g.Project) +
		"/managedZones/" + url.PathEscape(g.Zone) + "/rrsets"
}

func (g *GoogleCloudDNS) one(name, typ string) string {
	return g.rrsets() + "/" + url.PathEscape(fqdn(name)) + "/" + url.PathEscape(typ)
}

// do sends one call. A 404 gives found=false and no error.
func (g *GoogleCloudDNS) do(ctx context.Context, method, u string, in, out any) (bool, error) {
	tok, err := g.Token(ctx)
	if err != nil {
		return false, fmt.Errorf("googleclouddns: credentials: %w", err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("googleclouddns: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, gcdMaxBody))
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return false, fmt.Errorf("googleclouddns: %s %s/%s: %s", method, g.Project, g.Zone, msg)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return false, fmt.Errorf("googleclouddns: %w", err)
		}
	}
	return true, nil
}

func (g *GoogleCloudDNS) Get(ctx context.Context, name, typ string) (string, bool, error) {
	var rs gcdRRSet
	found, err := g.do(ctx, http.MethodGet, g.one(name, typ), nil, &rs)
	if err != nil || !found || len(rs.RRDatas) == 0 {
		return "", false, err
	}
	return rs.RRDatas[0], true, nil
}

// Put replaces the record set of that name and type (PATCH), or creates it when there is none.
// It never touches another record of the zone.
func (g *GoogleCloudDNS) Put(ctx context.Context, r Record) error {
	data := r.Target
	if r.Type == "CNAME" {
		data = fqdn(data)
	}
	rs := gcdRRSet{Name: fqdn(r.Name), Type: r.Type, TTL: r.TTL, RRDatas: []string{data}}
	found, err := g.do(ctx, http.MethodPatch, g.one(r.Name, r.Type), rs, nil)
	if err != nil || found {
		return err
	}
	created, err := g.do(ctx, http.MethodPost, g.rrsets(), rs, nil)
	if err == nil && !created {
		err = fmt.Errorf("googleclouddns: no managed zone %q in project %q", g.Zone, g.Project)
	}
	return err
}
