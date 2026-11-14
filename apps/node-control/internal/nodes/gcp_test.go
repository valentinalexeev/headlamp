package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// fakeGCE is a minimal Compute API: instances by zone, aggregatedList with the name filter, get, start and stop.
type fakeGCE struct {
	mu    sync.Mutex
	vms   map[string]*gcpInstance // zone/name
	calls []string
	token string
}

func (f *fakeGCE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, `{"error":{"message":"bad token"}}`, http.StatusUnauthorized)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/projects/p1")
	switch {
	case p == "/aggregated/instances" && r.Method == http.MethodGet:
		if !strings.Contains(r.URL.Query().Get("filter"), `labels.headlamp-exit-node = "true"`) {
			http.Error(w, "filter without the control label", http.StatusBadRequest)
			return
		}
		items := map[string]map[string][]gcpInstance{}
		for k, v := range f.vms {
			z := strings.SplitN(k, "/", 2)[0]
			if strings.Contains(r.URL.Query().Get("filter"), `name = "`+v.Name+`"`) {
				items["zones/"+z] = map[string][]gcpInstance{"instances": append(items["zones/"+z]["instances"], *v)}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case strings.HasPrefix(p, "/zones/"):
		parts := strings.Split(strings.TrimPrefix(p, "/zones/"), "/") // zone, "instances", name[, action]
		if len(parts) < 3 {
			http.NotFound(w, r)
			return
		}
		vm := f.vms[parts[0]+"/"+parts[2]]
		if vm == nil {
			http.Error(w, `{"error":{"message":"The resource was not found"}}`, http.StatusNotFound)
			return
		}
		switch {
		case len(parts) == 3 && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(vm)
		case len(parts) == 4 && r.Method == http.MethodPost && parts[3] == "start":
			vm.Status = "STAGING"
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "RUNNING"})
		case len(parts) == 4 && r.Method == http.MethodPost && parts[3] == "stop":
			vm.Status = "STOPPING"
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "RUNNING"})
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGCE) add(zone, name, status string, labelled bool) {
	labels := map[string]string{"team": "net"}
	if labelled {
		labels[gcpLabel] = "true"
	}
	f.vms[zone+"/"+name] = &gcpInstance{Name: name, Status: status, Labels: labels,
		Zone:        "https://www.googleapis.com/compute/v1/projects/p1/zones/" + zone,
		MachineType: "https://www.googleapis.com/compute/v1/projects/p1/zones/" + zone + "/machineTypes/e2-micro"}
}

func (f *fakeGCE) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func newGCPTest(t *testing.T, env map[string]string) (*gcpProvider, *fakeGCE) {
	t.Helper()
	f := &fakeGCE{vms: map[string]*gcpInstance{}, token: "ya29.test"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	if env == nil {
		env = map[string]string{}
	}
	env["GCP_COMPUTE_ENDPOINT"] = srv.URL
	p := NewGCP(core.MapEnv(env), srv.Client()).(*gcpProvider)
	p.tokenSource = func(context.Context) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test"}), nil
	}
	return p, f
}

func TestGCPStatusStartStop(t *testing.T) {
	p, f := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1"})
	f.add("europe-west3-a", "exit-1", "TERMINATED", true)
	n := Target{Name: "exit-1", Provider: "gcp"}
	ctx := context.Background()

	st, err := p.Status(ctx, n)
	if err != nil || st["status"] != "STOPPED" || st["detail"] != "europe-west3-a" || st["shape"] != "e2-micro" {
		t.Fatalf("status of a stopped (TERMINATED) VM: %v %v", st, err)
	}
	prev, cur, err := p.Start(ctx, n)
	if err != nil || prev != "STOPPED" || cur != "STARTING" || f.count("POST /projects/p1/zones/europe-west3-a/instances/exit-1/start") != 1 {
		t.Fatalf("start: %s %s %v %v", prev, cur, err, f.calls)
	}
	// Starting an already starting VM sends nothing.
	if prev, cur, err = p.Start(ctx, n); err != nil || prev != "STARTING" || cur != "STARTING" || f.count("POST") != 1 {
		t.Fatalf("idempotent start: %s %s %v", prev, cur, err)
	}
	f.vms["europe-west3-a/exit-1"].Status = "RUNNING"
	if prev, cur, err = p.Stop(ctx, n); err != nil || prev != "RUNNING" || cur != "STOPPING" {
		t.Fatalf("stop: %s %s %v", prev, cur, err)
	}
	// The zone is found once by the aggregated list and then cached.
	if got := f.count("GET /projects/p1/aggregated/instances"); got != 1 {
		t.Fatalf("aggregated list calls: %d", got)
	}
}

func TestGCPRefusesAnUnlabelledVM(t *testing.T) {
	p, f := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1"})
	f.add("europe-west3-a", "prod-db", "RUNNING", false)
	// Found by a pinned zone (no list), but refused because of the missing label.
	n := Target{Name: "prod-db", Provider: "gcp", Block: map[string]any{"zone": "europe-west3-a"}}
	_, _, err := p.Stop(context.Background(), n)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || !strings.Contains(ae.Message, gcpLabel) {
		t.Fatalf("unlabelled VM must be refused: %v", err)
	}
	if f.count("POST") != 0 {
		t.Fatalf("no start or stop may be sent: %v", f.calls)
	}
	// The list also ignores it.
	if _, err := p.Status(context.Background(), Target{Name: "prod-db", Provider: "gcp"}); !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("unlabelled VM must not be listed: %v", err)
	}
}

func TestGCPAmbiguousNameNeedsAZone(t *testing.T) {
	p, f := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1"})
	f.add("europe-west3-a", "exit-1", "RUNNING", true)
	f.add("us-east1-b", "exit-1", "RUNNING", true)
	var ae *APIError
	if _, err := p.Status(context.Background(), Target{Name: "exit-1"}); !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("two VMs of one name must need a zone: %v", err)
	}
	p2, f2 := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1", "GCP_ZONES": "us-east1-b"})
	f2.add("europe-west3-a", "exit-1", "RUNNING", true)
	f2.add("us-east1-b", "exit-1", "TERMINATED", true)
	if st, err := p2.Status(context.Background(), Target{Name: "exit-1"}); err != nil || st["detail"] != "us-east1-b" {
		t.Fatalf("GCP_ZONES must pick the zone: %v %v", st, err)
	}
}

func TestGCPProjectFromTheMetadataServer(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/computeMetadata/v1/project/project-id" || r.Header.Get("Metadata-Flavor") != "Google" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = w.Write([]byte("p1"))
	}))
	defer md.Close()
	p, f := newGCPTest(t, map[string]string{"GCE_METADATA_HOST": strings.TrimPrefix(md.URL, "http://")})
	f.add("europe-west3-a", "exit-1", "RUNNING", true)
	if st, err := p.Status(context.Background(), Target{Name: "exit-1"}); err != nil || st["status"] != "RUNNING" {
		t.Fatalf("project from the metadata server (#221): %v %v", st, err)
	}
}

func TestGCPNoProjectIsNotConfigured(t *testing.T) {
	p, _ := newGCPTest(t, map[string]string{"GCE_METADATA_HOST": "127.0.0.1:1"})
	_, err := p.Status(context.Background(), Target{Name: "exit-1"})
	if !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "GCP_PROJECT") {
		t.Fatalf("no project must be not configured and name GCP_PROJECT: %v", err)
	}
}

func TestGCPCredentialsErrorIsNotConfigured(t *testing.T) {
	p, _ := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1"})
	p.tokenSource = func(context.Context) (oauth2.TokenSource, error) {
		return nil, errors.New("could not find default credentials")
	}
	if _, err := p.Status(context.Background(), Target{Name: "exit-1"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing credentials must be not configured: %v", err)
	}
}

func TestGCPGoneVMIsForgotten(t *testing.T) {
	p, f := newGCPTest(t, map[string]string{"GCP_PROJECT": "p1"})
	f.add("europe-west3-a", "exit-1", "RUNNING", true)
	n := Target{Name: "exit-1"}
	if _, err := p.Status(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	delete(f.vms, "europe-west3-a/exit-1")
	f.add("europe-west3-b", "exit-1", "RUNNING", true) // re-created in another zone
	if _, err := p.Status(context.Background(), n); err == nil {
		t.Fatal("the cached zone answers 404 once")
	}
	if st, err := p.Status(context.Background(), n); err != nil || st["detail"] != "europe-west3-b" {
		t.Fatalf("after a 404 the zone is looked up again: %v %v", st, err)
	}
}
