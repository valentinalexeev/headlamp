// Package dnsrec keeps one public DNS record in a provider's zone (#127). Funnel needs a single wildcard record,
// *.<public domain>, pointing at the TLS edge: with on-demand certificates every namespace and service lives under
// it, so node-control writes it once and checks it from time to time, and adding a namespace changes no DNS.
package dnsrec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Record is the desired record. Type is A, AAAA or CNAME, chosen from Target.
type Record struct {
	Name   string // full name, for example *.funnel.example.com
	Type   string
	Target string
	TTL    int
}

// For builds the record for a name and a target: an address gives A or AAAA, a host name gives CNAME.
func For(name, target string) (Record, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	target = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target)), ".")
	if name == "" || target == "" {
		return Record{}, errors.New("dnsrec: empty name or target")
	}
	r := Record{Name: name, Target: target, TTL: 300}
	if ip, err := netip.ParseAddr(target); err == nil {
		r.Type = "A"
		if ip.Is6() && !ip.Is4In6() {
			r.Type = "AAAA"
		}
		r.Target = ip.Unmap().String()
		return r, nil
	}
	r.Type = "CNAME"
	return r, nil
}

// Provider reads and writes one record in one zone.
type Provider interface {
	// Get returns the current record of that name and type, or ok=false.
	Get(ctx context.Context, name, typ string) (target string, ok bool, err error)
	// Put creates or replaces the record.
	Put(ctx context.Context, r Record) error
	Name() string
}

// Status is what the console shows.
type Status struct {
	Provider string    `json:"provider"`
	Record   Record    `json:"record"`
	InSync   bool      `json:"in_sync"`
	Checked  time.Time `json:"checked,omitzero"`
	Error    string    `json:"error,omitempty"`
}

// Ensure makes the record match r: it reads it and writes only on a difference.
func Ensure(ctx context.Context, p Provider, r Record) (changed bool, err error) {
	cur, ok, err := p.Get(ctx, r.Name, r.Type)
	if err != nil {
		return false, err
	}
	if ok && strings.EqualFold(strings.TrimSuffix(cur, "."), r.Target) {
		return false, nil
	}
	return true, p.Put(ctx, r)
}

// --- Cloudflare ---

// Cloudflare uses the v4 API with a token that may edit DNS of one zone.
type Cloudflare struct {
	Zone  string // zone id
	Token string
	Base  string // default https://api.cloudflare.com/client/v4
	HTTP  *http.Client
}

func (c *Cloudflare) Name() string { return "cloudflare" }

func (c *Cloudflare) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

func (c *Cloudflare) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.Unmarshal(b, &env); err != nil || resp.StatusCode >= 300 || !env.Success {
		msg := resp.Status
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return fmt.Errorf("cloudflare: %s %s: %s", method, strings.SplitN(path, "?", 2)[0], msg)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func (c *Cloudflare) find(ctx context.Context, name, typ string) (*cfRecord, error) {
	var recs []cfRecord
	q := url.Values{"name": {name}, "type": {typ}}
	if err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(c.Zone)+"/dns_records?"+q.Encode(), nil, &recs); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

func (c *Cloudflare) Get(ctx context.Context, name, typ string) (string, bool, error) {
	r, err := c.find(ctx, name, typ)
	if err != nil || r == nil {
		return "", false, err
	}
	return r.Content, true, nil
}

func (c *Cloudflare) Put(ctx context.Context, r Record) error {
	cur, err := c.find(ctx, r.Name, r.Type)
	if err != nil {
		return err
	}
	// Not proxied: the edge does TLS itself, and a proxy would hide the client address and ACME HTTP-01.
	rec := cfRecord{Type: r.Type, Name: r.Name, Content: r.Target, TTL: r.TTL}
	if cur == nil {
		return c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(c.Zone)+"/dns_records", rec, nil)
	}
	return c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(c.Zone)+"/dns_records/"+url.PathEscape(cur.ID), rec, nil)
}
