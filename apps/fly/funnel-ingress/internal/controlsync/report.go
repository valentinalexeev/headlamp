package controlsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"funnel-ingress/internal/routes"
)

// RouteState is a route as reported: the route plus what the peer list says about its owner.
type RouteState struct {
	routes.Route
	OwnerName    string
	TargetOnline bool
}

// State is what the ingress reports after each sync cycle. It holds no secrets: no keys, no URLs.
type State struct {
	AppliedVersion string
	SentAt         time.Time
	SnapshotLoaded bool
	PeerCount      int
	LastReconcile  time.Time
	ConfigAge      time.Duration
	Routes         []RouteState
	Errors         []string
}

// Reporter delivers a State to node-control.
type Reporter interface {
	Report(ctx context.Context, s State) error
}

type wireState struct {
	AppliedVersion   string           `json:"applied_version"`
	SentAt           time.Time        `json:"sent_at"`
	SnapshotLoaded   bool             `json:"snapshot_loaded"`
	PeerCount        int              `json:"peer_count"`
	LastReconcile    *time.Time       `json:"last_reconcile"`
	ConfigAgeSeconds int64            `json:"config_age_seconds"`
	Routes           []wireStateRoute `json:"routes"`
	Errors           []string         `json:"errors"`
}

type wireStateRoute struct {
	NS           string `json:"ns"`
	Service      string `json:"service"`
	Target       string `json:"target"`
	Port         int    `json:"port"`
	Scheme       string `json:"scheme"`
	OwnerNodeID  string `json:"owner_node_id"`
	OwnerName    string `json:"owner_name"`
	TargetOnline bool   `json:"target_online"`
}

func encodeState(s State) ([]byte, error) {
	w := wireState{
		AppliedVersion: s.AppliedVersion, SentAt: s.SentAt.UTC(), SnapshotLoaded: s.SnapshotLoaded, PeerCount: s.PeerCount,
		ConfigAgeSeconds: int64(s.ConfigAge / time.Second), Routes: []wireStateRoute{}, Errors: append([]string{}, s.Errors...),
	}
	if !s.LastReconcile.IsZero() {
		t := s.LastReconcile.UTC()
		w.LastReconcile = &t
	}
	for _, r := range s.Routes {
		w.Routes = append(w.Routes, wireStateRoute{
			NS: r.NS, Service: r.Service, Target: r.Target, Port: r.Port, Scheme: r.Scheme,
			OwnerNodeID: r.NodeID, OwnerName: r.OwnerName, TargetOnline: r.TargetOnline,
		})
	}
	// Stay under the body cap: drop routes from the end until it fits and say so.
	for {
		b, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		if len(b) <= MaxBody || len(w.Routes) == 0 {
			return b, nil
		}
		w.Routes = w.Routes[:len(w.Routes)/2]
		if len(w.Errors) == 0 || w.Errors[len(w.Errors)-1] != "routes truncated to fit the report size" {
			w.Errors = append(w.Errors, "routes truncated to fit the report size")
		}
	}
}

type nodeControlReporter struct {
	base   string
	client *http.Client
}

// NewNodeControlReporter posts to {baseURL}/ingress/v1/state.
func NewNodeControlReporter(baseURL string, client *http.Client) Reporter {
	return nodeControlReporter{strings.TrimRight(baseURL, "/"), client}
}

func (n nodeControlReporter) Report(ctx context.Context, s State) error {
	body, err := encodeState(s)
	if err != nil {
		return errors.New("cannot encode the state report")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+"/ingress/v1/state", bytes.NewReader(body))
	if err != nil {
		return errors.New("bad state URL")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("reporting state: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node-control answered %d to the state report", resp.StatusCode)
	}
	return nil
}
