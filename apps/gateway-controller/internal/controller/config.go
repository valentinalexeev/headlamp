// Package controller runs the reconcilers of the gateway controller: it accepts the GatewayClasses it owns,
// runs one funnel-sidecar Deployment per Gateway and writes the status of Gateways and routes (#338).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ControllerName is the spec.controllerName of the GatewayClasses this controller owns.
const ControllerName = "github.com/valentinalexeev/headlamp/gateway-controller"

// Config is the configuration of the controller (flags and environment).
type Config struct {
	ServeClass  string // name of the serve GatewayClass (default headlamp-serve)
	FunnelClass string // name of the funnel GatewayClass (default headlamp-funnel)
	Image       string // the funnel-sidecar image
	ControlURL  string // FUNNEL_CONTROL_URL, optional
	ServeURL    string // FUNNEL_SERVE_URL: node-control on the tailnet (serve mode without certificateRefs)
	APIURL      string // FUNNEL_API_URL: the funnel-ingress registration API (funnel mode)
	CABundle    string // optional ConfigMap (key ca.crt) mounted as SSL_CERT_FILE into the sidecars
	TCPRoutes   bool   // the TCPRoute CRD is installed
	Requeue     time.Duration
}

// Readiness is the /readyz body of funnel-sidecar.
type Readiness struct {
	Ready     bool     `json:"ready"`
	Addresses []string `json:"addresses"`
}

// ReadyFunc reads the readiness of the sidecar pod at podIP.
type ReadyFunc func(ctx context.Context, podIP string) (Readiness, error)

// sidecarReadyPort is the port of funnel-sidecar's /readyz (FUNNEL_READY_ADDR).
const sidecarReadyPort = "8081"

// HTTPReady reads http://<podIP>:8081/readyz. A 503 still has a body with the addresses.
func HTTPReady(client *http.Client) ReadyFunc {
	return func(ctx context.Context, podIP string) (Readiness, error) {
		u := "http://" + net.JoinHostPort(podIP, sidecarReadyPort) + "/readyz"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return Readiness{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return Readiness{}, err
		}
		defer resp.Body.Close()
		var r Readiness
		const maxBody = 1 << 16
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&r); err != nil {
			return Readiness{}, fmt.Errorf("readyz body: %w", err)
		}
		return r, nil
	}
}
