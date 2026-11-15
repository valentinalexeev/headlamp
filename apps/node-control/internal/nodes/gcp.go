package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

const (
	gcpTimeout  = 20 * time.Second
	gcpScope    = "https://www.googleapis.com/auth/compute"
	gcpAPI      = "https://compute.googleapis.com/compute/v1"
	gcpMetadata = "http://metadata.google.internal"
	// gcpLabel is the control label (#214): node-control acts only on VMs with it.
	gcpLabel = "headlamp-exit-node"
	// gcpMaxBody caps an API answer.
	gcpMaxBody = 4 << 20
	// gcpMaxListed is the page size of the aggregated list; a node name matches one or two VMs.
	gcpMaxListed = 500
	// gcpMetadataTimeout bounds the metadata server lookup, so a host off GCE is not slowed down.
	gcpMetadataTimeout = 2 * time.Second
	// gcpMaxProjectID caps the metadata answer (a project id is at most 30 characters).
	gcpMaxProjectID = 256
)

// gcpStatus maps GCE instance states to the RUNNING/STOPPED vocabulary of the other
// VM providers. TERMINATED is a stopped VM in GCE (also a preempted Spot VM with
// instance_termination_action = STOP).
var gcpStatus = map[string]string{
	"RUNNING":      "RUNNING",
	"TERMINATED":   "STOPPED",
	"STOPPED":      "STOPPED",
	"SUSPENDED":    "STOPPED",
	"PROVISIONING": "STARTING",
	"STAGING":      "STARTING",
	"REPAIRING":    "STARTING",
	"STOPPING":     "STOPPING",
	"SUSPENDING":   "STOPPING",
}

type gcpInstance struct {
	Name        string            `json:"name"`
	Zone        string            `json:"zone"` // full URL; the last path element is the zone
	Status      string            `json:"status"`
	MachineType string            `json:"machineType"`
	Labels      map[string]string `json:"labels"`
}

func (i gcpInstance) zone() string { return i.Zone[strings.LastIndex(i.Zone, "/")+1:] }

type gcpProvider struct {
	env  core.Env
	hc   *http.Client
	now  func() time.Time
	ids  idCache // project|name -> zone
	api  string  // Compute API base (GCP_COMPUTE_ENDPOINT overrides it)
	meta string  // metadata server base (GCE_METADATA_HOST overrides it)

	mu  sync.Mutex
	src oauth2.TokenSource
	// tokenSource makes the credentials; tests replace it.
	tokenSource func(ctx context.Context) (oauth2.TokenSource, error)

	projectOnce sync.Once
	hostProject string
}

// NewGCP returns the Google Compute Engine provider (#214).
//
// The target's block is {"project", "zone", "instance_name"}, all optional. The VM is
// found by its name (default Target.Name) in the project, in one aggregated list call
// over all zones (GCP_ZONES narrows it), and the zone is cached for 15 s. The project
// falls back to GCP_PROJECT, then to the host's project from the metadata server when
// node-control runs on GCE (#221). A VM without the label headlamp-exit-node=true is
// refused. Credentials are Application Default Credentials: GOOGLE_APPLICATION_CREDENTIALS
// (a service account key or a Workload Identity Federation config), or the metadata
// server on GCE.
func NewGCP(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	p := &gcpProvider{env: env, hc: hc, now: time.Now, api: gcpAPI, meta: gcpMetadata}
	if u := strings.TrimRight(env.Getenv("GCP_COMPUTE_ENDPOINT"), "/"); u != "" {
		p.api = u
	}
	if h := env.Getenv("GCE_METADATA_HOST"); h != "" {
		p.meta = "http://" + h
	}
	p.tokenSource = func(ctx context.Context) (oauth2.TokenSource, error) {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, p.hc)
		c, err := google.FindDefaultCredentials(ctx, gcpScope)
		if err != nil {
			return nil, err
		}
		return c.TokenSource, nil
	}
	return p
}

func (p *gcpProvider) token(ctx context.Context) (string, error) {
	p.mu.Lock()
	src := p.src
	p.mu.Unlock()
	if src == nil {
		s, err := p.tokenSource(ctx)
		if err != nil {
			return "", notConfigured("GCP credentials: " + scrub(err.Error(), nil) +
				" (set GOOGLE_APPLICATION_CREDENTIALS, or run on GCE with a service account)")
		}
		src = oauth2.ReuseTokenSource(nil, s)
		p.mu.Lock()
		p.src = src
		p.mu.Unlock()
	}
	t, err := src.Token()
	if err != nil {
		return "", &APIError{Message: "GCP token: " + scrub(err.Error(), nil)}
	}
	return t.AccessToken, nil
}

// metadataProject asks the metadata server for the host's project (#221), once.
// "" means not on GCE.
func (p *gcpProvider) metadataProject(ctx context.Context) string {
	p.projectOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, gcpMetadataTimeout)
		defer cancel()
		u := p.meta + "/computeMetadata/v1/project/project-id"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := p.hc.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, gcpMaxProjectID))
		fromGCE := resp.Header.Get("Metadata-Flavor") == "Google"
		if resp.StatusCode == http.StatusOK && fromGCE {
			p.hostProject = strings.TrimSpace(string(b))
		}
	})
	return p.hostProject
}

func gcpString(b map[string]any, k string) string {
	s, _ := b[k].(string)
	return strings.TrimSpace(s)
}

// target returns the project, zone (may be "") and VM name of n.
func (p *gcpProvider) target(ctx context.Context, n Target) (project, zone, name string,
	err error,
) {
	project = gcpString(n.Block, "project")
	zone = gcpString(n.Block, "zone")
	name = gcpString(n.Block, "instance_name")
	if name == "" {
		name = n.Name
	}
	if name == "" {
		return "", "", "", notConfigured(`node needs a name, or "gcp": {"instance_name": ...}`)
	}
	if project == "" {
		project = p.env.Getenv("GCP_PROJECT")
	}
	if project == "" {
		project = p.metadataProject(ctx)
	}
	if project == "" {
		return "", "", "", notConfigured(`no GCP project: set {"project": ...} in the ` +
			`provider block or GCP_PROJECT (on GCE it defaults to the host's project)`)
	}
	return project, zone, name, nil
}

func (p *gcpProvider) do(ctx context.Context, method, rawURL, label string) ([]byte, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, gcpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, &APIError{Message: label + ": " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, &APIError{Message: label + ": " + scrub(err.Error(), []string{tok})}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, gcpMaxBody))
	if err != nil {
		msg := label + ": " + scrub(err.Error(), []string{tok})
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := string(b)
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return nil, &APIError{Status: resp.StatusCode,
			Message: fmt.Sprintf("%s: %d %s", label, resp.StatusCode, scrub(msg, []string{tok}))}
	}
	return b, nil
}

// zones is the GCP_ZONES allow list ("" = all zones).
func (p *gcpProvider) zones() map[string]bool {
	z := map[string]bool{}
	for _, s := range strings.Split(p.env.Getenv("GCP_ZONES"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			z[s] = true
		}
	}
	return z
}

// find lists the VMs named name with the control label, in one aggregated call over
// all zones (#221).
func (p *gcpProvider) find(ctx context.Context, project, name string) ([]gcpInstance, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`(name = "%s") AND (labels.%s = "true")`, name, gcpLabel))
	q.Set("maxResults", fmt.Sprint(gcpMaxListed))
	q.Set("returnPartialSuccess", "true")
	u := p.api + "/projects/" + url.PathEscape(project) + "/aggregated/instances?" + q.Encode()
	b, err := p.do(ctx, http.MethodGet, u, "instances.aggregatedList")
	if err != nil {
		return nil, err
	}
	var out struct {
		Items map[string]struct {
			Instances []gcpInstance `json:"instances"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, &APIError{Message: "instances.aggregatedList: bad answer"}
	}
	allow := p.zones()
	var found []gcpInstance
	for _, it := range out.Items {
		for _, i := range it.Instances {
			inZone := len(allow) == 0 || allow[i.zone()]
			if i.Name == name && i.Labels[gcpLabel] == "true" && inZone {
				found = append(found, i)
			}
		}
	}
	sort.Slice(found, func(a, b int) bool { return found[a].zone() < found[b].zone() })
	return found, nil
}

func (p *gcpProvider) cacheKey(project, name string) string { return project + "|" + name }

func (p *gcpProvider) instanceURL(project, zone, name string) string {
	return p.api + "/projects/" + url.PathEscape(project) + "/zones/" + url.PathEscape(zone) +
		"/instances/" + url.PathEscape(name)
}

// get reads one VM and refuses it without the control label.
func (p *gcpProvider) get(ctx context.Context, project, zone, name string) (gcpInstance, error) {
	b, err := p.do(ctx, http.MethodGet, p.instanceURL(project, zone, name), "instances.get")
	if err != nil {
		return gcpInstance{}, err
	}
	var i gcpInstance
	if err := json.Unmarshal(b, &i); err != nil {
		return gcpInstance{}, &APIError{Message: "instances.get: bad answer"}
	}
	if i.Labels[gcpLabel] != "true" {
		return gcpInstance{}, &APIError{Status: http.StatusForbidden, Message: fmt.Sprintf(
			"VM %s has no label %s=true: node-control does not control it", name, gcpLabel)}
	}
	return i, nil
}

// resolve finds the VM of n: by its zone from the block or the cache, else by the
// aggregated list.
func (p *gcpProvider) resolve(ctx context.Context, n Target) (gcpInstance, string, error) {
	project, zone, name, err := p.target(ctx, n)
	if err != nil {
		return gcpInstance{}, "", err
	}
	key := p.cacheKey(project, name)
	if zone == "" {
		if z, ok := p.ids.get(key, p.now()); ok {
			zone = z
		}
	}
	if zone == "" {
		found, err := p.find(ctx, project, name)
		if err != nil {
			return gcpInstance{}, "", err
		}
		switch len(found) {
		case 0:
			return gcpInstance{}, "", &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf(
				"no VM %s with label %s=true in project %s", name, gcpLabel, project)}
		case 1:
			zone = found[0].zone()
			p.ids.put(key, zone, p.now())
		default:
			zs := make([]string, 0, len(found))
			for _, f := range found {
				zs = append(zs, f.zone())
			}
			return gcpInstance{}, "", &APIError{Status: http.StatusConflict, Message: fmt.Sprintf(
				`more than one VM %s (zones %s): set {"zone": ...} in the provider block `+
					`or GCP_ZONES`, name, strings.Join(zs, ", "))}
		}
	}
	i, err := p.get(ctx, project, zone, name)
	if err != nil {
		p.ids.forgetOnNotFound(key, err)
		return gcpInstance{}, "", err
	}
	return i, project, nil
}

func gcpMapStatus(s string) string {
	if v, ok := gcpStatus[s]; ok {
		return v
	}
	return s
}

func (p *gcpProvider) Status(ctx context.Context, n Target) (Status, error) {
	i, _, err := p.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	st := Status{"status": gcpMapStatus(i.Status), "detail": i.zone()}
	if mt := i.MachineType; mt != "" {
		st["shape"] = mt[strings.LastIndex(mt, "/")+1:]
	}
	return st, nil
}

func (p *gcpProvider) power(ctx context.Context, n Target, want, transient, action string) (
	string, string, error,
) {
	i, project, err := p.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	prev := gcpMapStatus(i.Status)
	if prev == want || prev == transient {
		return prev, prev, nil
	}
	u := p.instanceURL(project, i.zone(), i.Name) + "/" + action
	if _, err := p.do(ctx, http.MethodPost, u, "instances."+action); err != nil {
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a RUNNING or already starting VM is left alone. A stopped VM keeps
// its boot disk, so it comes back as the same headscale node (#214).
func (p *gcpProvider) Start(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, "RUNNING", "STARTING", "start")
}

// Stop is idempotent: a stopped or already stopping VM is left alone.
func (p *gcpProvider) Stop(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, "STOPPED", "STOPPING", "stop")
}
