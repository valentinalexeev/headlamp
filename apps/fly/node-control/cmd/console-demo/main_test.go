package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
	for _, name := range []string{"attention", "clear", "minimal", "partial"} {
		t.Run(name, func(t *testing.T) {
			addr := startDemo(t, name)
			labels := navLabels(t, addr)
			switch name {
			case "minimal":
				if len(labels) != 1 {
					t.Fatalf("minimal: want one item, got %v", labels)
				}
			case "partial":
				if len(labels) != 4 {
					t.Fatalf("partial: want Overview, Nodes, Devices and Activity, got %v", labels)
				}
			default:
				if len(labels) < 6 {
					t.Fatalf("%s: want the full menu, got %v", name, labels)
				}
			}
		})
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
