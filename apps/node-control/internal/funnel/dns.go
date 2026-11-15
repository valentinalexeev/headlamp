package funnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/dnsrec"
)

// dnsManager keeps the one public record of funnel, *.<FUNNEL_PUBLIC_DOMAIN> -> FUNNEL_INGRESS_HOSTNAME, in the
// provider's zone (#127). With on-demand certificates at the edge every namespace lives under that one record, so
// adding a namespace needs no DNS change; the manager only creates the record and puts it back if someone changes it.
type dnsManager struct {
	p dnsrec.Provider
	// check, when set, replaces Ensure: the record is checked by lookup only
	// (FUNNEL_DNS_CHECK=lookup, #384), because the edge keeps it.
	check  func(ctx context.Context, r dnsrec.Record) (bool, error)
	rec    dnsrec.Record
	every  time.Duration
	mu     sync.Mutex
	status dnsrec.Status
}

// dnsFromEnv reads FUNNEL_DNS_PROVIDER (route53 or cloudflare) and its settings. Unset: nil, and the console shows
// the manual checklist as before. A Fly.io target (the record points at <app>.fly.dev) keeps the manual checklist:
// Fly issues its certificates through `fly certs`, not at our edge.
func dnsFromEnv(env core.Env, s *Settings, hc *http.Client) (*dnsManager, error) {
	prov := strings.ToLower(strings.TrimSpace(env.Getenv("FUNNEL_DNS_PROVIDER")))
	check := strings.ToLower(strings.TrimSpace(env.Getenv("FUNNEL_DNS_CHECK")))
	switch {
	case check != "" && check != "lookup":
		return nil, errors.New("FUNNEL_DNS_CHECK must be lookup")
	case check == "lookup" && prov != "":
		return nil, errors.New("set FUNNEL_DNS_CHECK=lookup or FUNNEL_DNS_PROVIDER, not both")
	case check == "lookup":
		return lookupFromEnv(s)
	case prov == "":
		return nil, nil
	}
	slog.Warn("FUNNEL_DNS_PROVIDER is deprecated (#384): let the edge keep the record " +
		"(Caddy dynamic_dns) and set FUNNEL_DNS_CHECK=lookup. It is removed before 1.0.0.")
	if s.IngressHostname == "" {
		return nil, errors.New("FUNNEL_DNS_PROVIDER needs FUNNEL_INGRESS_HOSTNAME: the edge name or address the record points at")
	}
	if strings.HasSuffix(s.IngressHostname, ".fly.dev") {
		return nil, errors.New("FUNNEL_DNS_PROVIDER does not apply to a Fly.io target: Fly issues certificates with `fly certs`; unset it and use the checklist")
	}
	zone := strings.TrimSpace(env.Getenv("FUNNEL_DNS_ZONE"))
	if zone == "" {
		return nil, errors.New("FUNNEL_DNS_PROVIDER needs FUNNEL_DNS_ZONE (the hosted zone id)")
	}
	rec, err := dnsrec.For("*."+s.PublicDomain, s.IngressHostname)
	if err != nil {
		return nil, err
	}
	var p dnsrec.Provider
	switch prov {
	case "cloudflare":
		tok := strings.TrimSpace(env.Getenv("FUNNEL_DNS_CLOUDFLARE_TOKEN"))
		if tok == "" {
			return nil, errors.New("FUNNEL_DNS_PROVIDER=cloudflare needs FUNNEL_DNS_CLOUDFLARE_TOKEN (DNS edit on the one zone)")
		}
		p = &dnsrec.Cloudflare{Zone: zone, Token: tok, Base: env.Getenv("FUNNEL_DNS_CLOUDFLARE_URL"), HTTP: hc}
	case "route53":
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"), awsconfig.WithHTTPClient(hc))
		if err != nil {
			return nil, fmt.Errorf("FUNNEL_DNS_PROVIDER=route53: AWS credentials: %w", err)
		}
		p = &dnsrec.Route53{Zone: zone, Client: route53.NewFromConfig(cfg, func(o *route53.Options) {
			if u := env.Getenv("AWS_ENDPOINT_URL_ROUTE_53"); u != "" {
				o.BaseEndpoint = &u
			}
		})}
	default:
		return nil, fmt.Errorf("FUNNEL_DNS_PROVIDER must be route53 or cloudflare")
	}
	return &dnsManager{p: p, rec: rec, every: 10 * time.Minute, status: dnsrec.Status{Provider: p.Name(), Record: rec}}, nil
}

// lookupFromEnv checks the record by DNS lookup only: the edge keeps it (#384).
func lookupFromEnv(s *Settings) (*dnsManager, error) {
	if s.IngressHostname == "" {
		return nil, errors.New("FUNNEL_DNS_CHECK=lookup needs FUNNEL_INGRESS_HOSTNAME: " +
			"the edge name or address the record must point at")
	}
	rec, err := dnsrec.For("*."+s.PublicDomain, s.IngressHostname)
	if err != nil {
		return nil, err
	}
	l := &dnsrec.Lookup{}
	return &dnsManager{p: l, check: l.Check, rec: rec, every: dnsCheckEvery,
		status: dnsrec.Status{Provider: l.Name(), Record: rec}}, nil
}

// dnsCheckEvery is how often the record is checked (and, with a provider, put back).
const dnsCheckEvery = 10 * time.Minute

func (d *dnsManager) sync(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var err error
	if d.check != nil {
		_, err = d.check(ctx, d.rec)
	} else {
		_, err = dnsrec.Ensure(ctx, d.p, d.rec)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.Checked = time.Now().UTC()
	d.status.InSync, d.status.Error = err == nil, ""
	if err != nil {
		d.status.Error = err.Error()
	}
}

func (d *dnsManager) run(ctx context.Context) {
	d.sync(ctx)
	t := time.NewTicker(d.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sync(ctx)
		}
	}
}

func (d *dnsManager) snapshot() dnsrec.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}
