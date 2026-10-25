package headscale

import (
	"encoding/json"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestListNodesDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/node" || r.Header.Get("Authorization") != "Bearer k1" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(`{"nodes":[
		 {"id":"7","name":"a","givenName":"a","online":true,"lastSeen":"2026-01-02T03:04:05Z",
		  "tags":["tag:x"],"forcedTags":["tag:x","tag:y"],"validTags":["tag:z"],
		  "ipAddresses":["100.64.0.1"],"approvedRoutes":["10.0.0.0/24"],"availableRoutes":["10.0.0.0/24","10.1.0.0/24"]},
		 {"id":8,"name":"b","online":false,"lastSeen":null,"forced_tags":["tag:q"]}
		]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "k1"}
	nodes, err := c.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0].ID != 7 || nodes[1].ID != 8 {
		t.Fatalf("%+v", nodes)
	}
	n := nodes[0]
	if !n.Online || n.LastSeen == nil || n.LastSeen.Year() != 2026 || n.GivenName != "a" {
		t.Fatalf("%+v", n)
	}
	if strings.Join(n.Tags, ",") != "tag:x,tag:y,tag:z" {
		t.Fatalf("tags %v", n.Tags)
	}
	if len(n.AvailableRoutes) != 2 || len(n.ApprovedRoutes) != 1 || len(n.IPAddresses) != 1 {
		t.Fatalf("%+v", n)
	}
	if nodes[1].LastSeen != nil || len(nodes[1].Tags) != 1 {
		t.Fatalf("%+v", nodes[1])
	}
}

func TestNotConfigured(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	_, err := c.ListNodes(context.Background())
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("%v", err)
	}
}

func TestErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "sekrit-key"}
	_, err := c.ListNodes(context.Background())
	if err == nil || strings.Contains(err.Error(), "sekrit-key") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("%v", err)
	}
	c2 := &Client{BaseURL: "http://127.0.0.1:1", APIKey: "sekrit-key"}
	if _, err := c2.ListNodes(context.Background()); err == nil || strings.Contains(err.Error(), "sekrit-key") {
		t.Fatalf("%v", err)
	}
}

type countSrc struct{ n atomic.Int32 }

func (s *countSrc) ListNodes(ctx context.Context) ([]Node, error) {
	s.n.Add(1)
	time.Sleep(50 * time.Millisecond)
	return []Node{{Name: "a"}}, nil
}

func TestCachedSharesOneFetch(t *testing.T) {
	src := &countSrc{}
	cs := cached(src, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ns, err := cs.Nodes(context.Background()); err != nil || len(ns) != 1 {
				t.Errorf("%v %v", ns, err)
			}
		}()
	}
	wg.Wait()
	cs.Nodes(context.Background())
	if got := src.n.Load(); got != 1 {
		t.Fatalf("fetches = %d", got)
	}
}

func TestCachedExpires(t *testing.T) {
	src := &countSrc{}
	cs := cached(src, 10*time.Millisecond)
	cs.Nodes(context.Background())
	time.Sleep(30 * time.Millisecond)
	cs.Nodes(context.Background())
	if got := src.n.Load(); got != 2 {
		t.Fatalf("fetches = %d", got)
	}
}

func TestLongUpstreamBodyGivesShortUTF8SafeDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(strings.Repeat("\u00e9", 5000))) // 2-byte runes: any odd cut splits one
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, APIKey: "sekrit-key"}
	_, err := c.ListNodes(context.Background())
	if err == nil || len(err.Error()) > 600 || !utf8.ValidString(err.Error()) {
		t.Fatalf("%d bytes valid=%v", len(fmt.Sprint(err)), utf8.ValidString(fmt.Sprint(err)))
	}
}

func TestMultiByteBodyCutIsRuneSafe(t *testing.T) {
	// one ASCII byte shifts every rune boundary, so a cut at 512 lands mid-rune
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("x" + strings.Repeat("\u00e9", 5000)))
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL, APIKey: "k"}).ListNodes(context.Background())
	if err == nil || !utf8.ValidString(err.Error()) {
		t.Fatalf("%v", err)
	}
}

func TestNodeMachineKey(t *testing.T) {
	for name, doc := range map[string]string{
		"camel": `{"id":"1","machineKey":"mkey:abc"}`,
		"snake": `{"id":"1","machine_key":"mkey:abc"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var n Node
			if err := json.Unmarshal([]byte(doc), &n); err != nil || n.MachineKey != "mkey:abc" {
				t.Fatalf("%+v %v", n, err)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		var n Node
		if err := json.Unmarshal([]byte(`{"id":"1"}`), &n); err != nil || n.MachineKey != "" {
			t.Fatalf("%+v %v", n, err)
		}
	})
}
