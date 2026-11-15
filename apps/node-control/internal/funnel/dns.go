package funnel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/dnsrec"
)

// dnsManager keeps the one public record of funnel, *.<FUNNEL_PUBLIC_DOMAIN> -> FUNNEL_INGRESS_HOSTNAME, in the
// provider's zone (#127). With on-demand certificates at the edge every namespace lives under that one record, so
// adding a namespace needs no DNS change; the manager only creates the record and puts it back if someone changes it.
type dnsManager struct {
	p      dnsrec.Provider
	rec    dnsrec.Record
	every  time.Duration
	mu     sync.Mutex
	status dnsrec.Status
}

// dnsFromEnv reads FUNNEL_DNS_PROVIDER (route53, cloudflare or googleclouddns) and its
// settings. Unset: nil, and the console shows
// the manual checklist as before. A Fly.io target (the record points at <app>.fly.dev) keeps the manual checklist:
// Fly issues its certificates through `fly certs`, not at our edge.
func dnsFromEnv(env core.Env, s *Settings, hc *http.Client) (*dnsManager, error) {
	prov := strings.ToLower(strings.TrimSpace(env.Getenv("FUNNEL_DNS_PROVIDER")))
	if prov == "" {
		return nil, nil
	}
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
	case "googleclouddns":
		project := strings.TrimSpace(env.Getenv("FUNNEL_DNS_GCP_PROJECT"))
		if project == "" {
			project = strings.TrimSpace(env.Getenv("GCP_PROJECT"))
		}
		if project == "" {
			return nil, errors.New("FUNNEL_DNS_PROVIDER=googleclouddns needs " +
				"FUNNEL_DNS_GCP_PROJECT (or GCP_PROJECT): the project of the managed zone")
		}
		p = &dnsrec.GoogleCloudDNS{Project: project, Zone: zone,
			Base: env.Getenv("FUNNEL_DNS_GCP_URL"), HTTP: hc, Token: gcpToken(hc)}
	default:
		return nil, errors.New("FUNNEL_DNS_PROVIDER must be route53, cloudflare or googleclouddns")
	}
	return &dnsManager{p: p, rec: rec, every: 10 * time.Minute, status: dnsrec.Status{Provider: p.Name(), Record: rec}}, nil
}

// gcpToken returns Application Default Credentials for Cloud DNS (#360), found on the first
// call and then reused: GOOGLE_APPLICATION_CREDENTIALS, or the service account of the VM on GCE.
func gcpToken(hc *http.Client) func(ctx context.Context) (string, error) {
	var (
		mu  sync.Mutex
		src oauth2.TokenSource
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if src == nil {
			ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
			c, err := google.FindDefaultCredentials(ctx, dnsrec.GoogleCloudDNSScope)
			if err != nil {
				return "", err
			}
			src = oauth2.ReuseTokenSource(nil, c.TokenSource)
		}
		t, err := src.Token()
		if err != nil {
			return "", err
		}
		return t.AccessToken, nil
	}
}

func (d *dnsManager) sync(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := dnsrec.Ensure(ctx, d.p, d.rec)
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
