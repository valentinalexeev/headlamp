package controlsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"funnel-ingress/internal/routes"
)

func TestNodeControlReporterPostsJSON(t *testing.T) {
	var path, method, ctype string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method, ctype = r.URL.Path, r.Method, r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s := State{AppliedVersion: "v1", SentAt: time.Unix(10, 0), LastReconcile: time.Unix(5, 0), SnapshotLoaded: true}
	if err := NewNodeControlReporter(srv.URL+"/", srv.Client()).Report(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if path != "/ingress/v1/state" || method != "POST" || ctype != "application/json" {
		t.Fatalf("%s %s %s", method, path, ctype)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["last_reconcile"] == nil || m["routes"] == nil || m["errors"] == nil {
		t.Fatalf("body: %s", body)
	}
}

func TestNodeControlReporterErrors(t *testing.T) {
	for _, code := range []int{403, 413, 422, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		if err := NewNodeControlReporter(srv.URL, srv.Client()).Report(context.Background(), State{}); err == nil {
			t.Errorf("status %d accepted", code)
		}
		srv.Close()
	}
}

func TestEncodeStateTruncatesToFitTheCap(t *testing.T) {
	var rs []RouteState
	for i := 0; i < 20000; i++ {
		rs = append(rs, RouteState{Route: routes.Route{Key: routes.Key{NS: "pdl", Service: fmt.Sprintf("service-%d", i)}, Target: "100.64.0.5", Port: 80, Scheme: "http", NodeID: "node-with-a-long-stable-id"}, OwnerName: "owner-host-name"})
	}
	b, err := encodeState(State{Routes: rs})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > MaxBody {
		t.Fatalf("report is %d bytes", len(b))
	}
	var m struct {
		Errors []string `json:"errors"`
	}
	json.Unmarshal(b, &m)
	if len(m.Errors) == 0 {
		t.Fatal("truncation not reported")
	}
}
