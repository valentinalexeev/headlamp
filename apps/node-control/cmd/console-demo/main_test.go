package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// startDemo runs a scenario on an ephemeral port and returns its admin address once seeded.
func startDemo(t *testing.T, scenario string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, options{addr: "127.0.0.1:0", scenario: scenario}, func(a string) { addrs <- a })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the demo did not stop")
		}
	})
	select {
	case a := <-addrs:
		return a
	case err := <-done:
		t.Fatalf("scenario %s stopped: %v", scenario, err)
	case <-time.After(150 * time.Second):
		t.Fatalf("scenario %s was not ready in time", scenario)
	}
	return ""
}

func get(t *testing.T, addr, path string) (int, []byte) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func navLabels(t *testing.T, addr string) []string {
	t.Helper()
	code, body := get(t, addr, "/_nav.json")
	if code != 200 {
		t.Fatalf("/_nav.json: %d", code)
	}
	var items []struct{ Label string }
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

func TestDemoServesEveryScenario(t *testing.T) {
	for _, name := range []string{"attention", "clear", "minimal", "partial", "viewer"} {
		t.Run(name, func(t *testing.T) {
			addr := startDemo(t, name)
			labels := navLabels(t, addr)
			switch name {
			case "minimal":
				if len(labels) != 1 {
					t.Fatalf("minimal: want one item, got %v", labels)
				}
			case "partial":
				want := []string{"Overview", "Nodes", "Devices", "Fleet", "Activity"}
				if !slices.Equal(labels, want) {
					t.Fatalf("partial: want %v, got %v", want, labels)
				}
			case "viewer":
				if !slices.Contains(labels, "Serve") {
					t.Fatalf("viewer: the Serve page is missing from %v", labels)
				}
			default:
				if !slices.Contains(labels, "Serve") {
					t.Fatalf("%s: the Serve page is missing from %v", name, labels)
				}
				if len(labels) < 6 {
					t.Fatalf("%s: want the full menu, got %v", name, labels)
				}
			}
		})
	}
}

// The clear scenario promises a front page with nothing to act on. startDemo returns only after the seeding has
// finished, so this reads the settled state: a read taken mid-seeding shows false alerts (an unapproved bridge,
// a funnel with no report yet).
func TestClearScenarioIsClear(t *testing.T) {
	addr := startDemo(t, "clear")
	code, body := get(t, addr, "/_attention.json")
	if code != 200 {
		t.Fatalf("/_attention.json: %d %s", code, body)
	}
	var att struct {
		Items []struct{ ID, Title string } `json:"items"`
		Lines []struct {
			Name, State string
		} `json:"systems"`
	}
	if err := json.Unmarshal(body, &att); err != nil {
		t.Fatal(err)
	}
	if len(att.Items) != 0 {
		t.Errorf("clear should have nothing needing attention, got %+v", att.Items)
	}
	if len(att.Lines) == 0 {
		t.Error("no system lines reported")
	}
	for _, l := range att.Lines {
		if l.State != "ok" {
			t.Errorf("system %s is %q, want ok", l.Name, l.State)
		}
	}
}

func TestAttentionScenarioSeedsEveryProblem(t *testing.T) {
	addr := startDemo(t, "attention")

	_, body := get(t, addr, "/enroll/ingresses")
	var list struct {
		Ingresses []struct {
			Name     string `json:"name"`
			Approval string `json:"approval"`
			Sync     *struct {
				Pending bool `json:"pending"`
			} `json:"sync"`
		} `json:"ingresses"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("ingresses: %v", err)
	}
	seen := map[string]string{}
	pending := map[string]bool{}
	for _, in := range list.Ingresses {
		seen[in.Name] = in.Approval
		pending[in.Name] = in.Sync != nil && in.Sync.Pending
	}
	if seen["bridge-3"] != "pending" {
		t.Errorf("bridge-3 should wait for approval, got %q", seen["bridge-3"])
	}
	if !pending["bridge-2"] {
		t.Errorf("bridge-2 should be out of sync: %v", pending)
	}

	_, body = get(t, addr, "/tasks.json")
	var tasks []struct {
		ID     string `json:"id"`
		Streak int    `json:"streak"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Fatal(err)
	}
	failing := 0
	for _, tk := range tasks {
		if tk.ID == "login-check" && tk.Streak >= 3 {
			failing++
		}
	}
	if len(tasks) != 6 || failing != 1 {
		t.Errorf("want 6 tasks with login-check failing 3 times, got %+v", tasks)
	}

	_, body = get(t, addr, "/policy/state")
	var st struct {
		Pending *struct{ ID int64 } `json:"pending"`
	}
	if err := json.Unmarshal(body, &st); err != nil || st.Pending == nil {
		t.Errorf("a policy change should be pending: %s", body)
	}

	_, body = get(t, addr, "/nodes/exit-b/status")
	var provider struct{ Status string }
	_ = json.Unmarshal(body, &provider)
	_, body = get(t, addr, "/tailnet/status")
	var tn map[string]struct{ Online bool }
	_ = json.Unmarshal(body, &tn)
	if provider.Status != "STARTED" || tn["exit-b"].Online {
		t.Errorf("exit-b should be running but offline: %q, %+v", provider.Status, tn["exit-b"])
	}
}

func TestDemoRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8099", "192.0.2.1:8099", ":8099", "example.net:80"} {
		if err := serve(context.Background(), options{addr: addr, scenario: "clear"}, nil); err == nil {
			t.Errorf("%s was accepted", addr)
		}
	}
}

// Both full scenarios seed the funnel through its public API: namespaces demo and lab, a manual route, a block,
// publisher keys and a report from the ingress with registered routes. Only attention leaves demo waiting.
func TestFunnelSeededInBothFullScenarios(t *testing.T) {
	for _, name := range []string{"attention", "clear"} {
		t.Run(name, func(t *testing.T) {
			addr := startDemo(t, name)
			code, body := get(t, addr, "/funnel/state")
			if code != 200 {
				t.Fatalf("state: %d %s", code, body)
			}
			var st struct {
				Namespaces []struct{ Name, State string } `json:"namespaces"`
				Routes     []struct{ NS, Service string } `json:"routes"`
				Blocks     []struct{ NS, Service string } `json:"blocks"`
				Ingress    *struct {
					Stale  bool `json:"stale"`
					Report struct {
						Routes []struct {
							NS      string `json:"ns"`
							Service string `json:"service"`
							Owner   string `json:"owner_node_id"`
							Online  bool   `json:"target_online"`
						} `json:"routes"`
					} `json:"report"`
				} `json:"ingress"`
			}
			if err := json.Unmarshal(body, &st); err != nil {
				t.Fatal(err)
			}
			states := map[string]string{}
			for _, n := range st.Namespaces {
				states[n.Name] = n.State
			}
			wantDemo := "active"
			if name == "attention" {
				wantDemo = "pending"
			}
			if states["lab"] != "active" || states["demo"] != wantDemo || len(states) != 2 {
				t.Errorf("namespaces: %v", states)
			}
			if len(st.Routes) != 1 || st.Routes[0].Service != "grafana" || len(st.Blocks) != 1 || st.Blocks[0].Service != "test" {
				t.Errorf("manual routes %v, blocks %v", st.Routes, st.Blocks)
			}
			if st.Ingress == nil || st.Ingress.Stale {
				t.Fatalf("ingress: %+v", st.Ingress)
			}
			registered, offline := 0, 0
			for _, r := range st.Ingress.Report.Routes {
				if r.Owner != "" {
					registered++
				}
				if !r.Online {
					offline++
				}
			}
			if registered < 3 || offline != map[string]int{"attention": 1, "clear": 0}[name] {
				t.Errorf("report routes: %d registered, %d offline", registered, offline)
			}
			code, body = get(t, addr, "/funnel/namespaces/lab/keys")
			var keys struct {
				Keys []struct{ ID int } `json:"keys"`
			}
			if code != 200 || json.Unmarshal(body, &keys) != nil || len(keys.Keys) != 2 {
				t.Errorf("keys: %d %s", code, body)
			}
			for _, p := range []string{"/funnel", "/funnel/config"} {
				if code, _ := get(t, addr, p); code != 200 {
					t.Errorf("%s: %d", p, code)
				}
			}
		})
	}
}

// The attention scenario shows the bridge settings and an acknowledged item; clear has neither problem.
func TestAttentionScenarioShowsIntermittentBridgeAndAcknowledgedItem(t *testing.T) {
	addr := startDemo(t, "attention")
	_, body := get(t, addr, "/_attention.json")
	var att struct {
		Items []struct {
			ID           string `json:"id"`
			Level        string `json:"level"`
			AckToken     string `json:"ack_token"`
			Acknowledged *struct {
				By string `json:"by"`
			} `json:"acknowledged"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &att); err != nil {
		t.Fatal(err)
	}
	levels := map[string]string{}
	acked := map[string]string{}
	for _, it := range att.Items {
		levels[it.ID] = it.Level
		if it.Acknowledged != nil {
			acked[it.ID] = it.Acknowledged.By
		}
	}
	if levels["bridge-sync-bridge-4"] != "info" {
		t.Errorf("the intermittent bridge is a notice: %v", levels)
	}
	if levels["bridge-sync-bridge-2"] != "critical" {
		t.Errorf("the other unreachable bridge is a risk: %v", levels)
	}
	if acked["devices-expiring"] != "alice" || len(acked) != 1 {
		t.Errorf("one acknowledged item: %v", acked)
	}

	_, body = get(t, addr, "/_activity.json")
	if !strings.Contains(string(body), `"action":"attention.acknowledge"`) || !strings.Contains(string(body), "within 7 days") {
		t.Errorf("the acknowledgement is in Activity: %s", body)
	}

	_, body = get(t, addr, "/enroll/ingresses")
	if !strings.Contains(string(body), `"node":"vm-4"`) || !strings.Contains(string(body), `"intermittent":true`) {
		t.Errorf("%s", body)
	}
}

// The Serve page (#129) has its fixtures in the full scenarios: a wildcard, a rotation in progress and an offline
// service. The admin may change them; in the viewer scenario the same data reads as view only.
func TestServeSeeded(t *testing.T) {
	for name, admin := range map[string]bool{"attention": true, "viewer": false} {
		t.Run(name, func(t *testing.T) {
			addr := startDemo(t, name)
			code, data, err := call(t.Context(), &http.Client{Timeout: 10 * time.Second}, addr, "GET", "/serve/state", nil)
			if err != nil || code != 200 {
				t.Fatalf("%d %v", code, err)
			}
			var st struct {
				CanAdmin bool `json:"can_admin"`
				Services []struct {
					Name  string            `json:"name"`
					Keys  []json.RawMessage `json:"keys"`
					Nodes []struct {
						Online bool `json:"online"`
					} `json:"nodes"`
				} `json:"services"`
			}
			if err := json.Unmarshal(data, &st); err != nil {
				t.Fatal(err)
			}
			if st.CanAdmin != admin {
				t.Fatalf("can_admin %v, want %v", st.CanAdmin, admin)
			}
			by := map[string]int{}
			for i, s := range st.Services {
				by[s.Name] = i
			}
			apps, ok1 := by["*.apps.svc.example.net"]
			wiki, ok2 := by["wiki.svc.example.net"]
			_, ok3 := by["grafana.svc.example.net"]
			if !ok1 || !ok2 || !ok3 {
				t.Fatalf("services %v", by)
			}
			if len(st.Services[apps].Keys) != 2 {
				t.Error("*.apps is not in a rotation")
			}
			if n := st.Services[wiki].Nodes; len(n) != 1 || n[0].Online {
				t.Error("wiki is not offline")
			}
			if code, _, _ := call(t.Context(), &http.Client{Timeout: 10 * time.Second}, addr, "GET", "/serve", nil); code != 200 {
				t.Errorf("the page: %d", code)
			}
		})
	}
}
