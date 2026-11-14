// Command gateway-controller publishes Services on the tailnet from Gateway API resources. It owns the
// GatewayClasses headlamp-serve and headlamp-funnel and runs one funnel-sidecar Deployment per Gateway (#338).
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/valentinalexeev/headlamp/apps/gateway-controller/internal/controller"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway-controller:", err)
		os.Exit(1)
	}
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func loadConfig() (controller.Config, error) {
	cfg := controller.Config{
		ServeClass:  env("GATEWAY_SERVE_CLASS", "headlamp-serve"),
		FunnelClass: env("GATEWAY_FUNNEL_CLASS", "headlamp-funnel"),
		Image:       env("GATEWAY_SIDECAR_IMAGE", ""),
		ControlURL:  env("FUNNEL_CONTROL_URL", ""),
		ServeURL:    env("FUNNEL_SERVE_URL", ""),
		APIURL:      env("FUNNEL_API_URL", ""),
		CABundle:    env("GATEWAY_CA_CONFIGMAP", ""),
		Requeue:     time.Minute,
	}
	if cfg.Image == "" {
		return cfg, errors.New("GATEWAY_SIDECAR_IMAGE is required")
	}
	if cfg.ServeURL == "" && cfg.APIURL == "" {
		return cfg, errors.New("set FUNNEL_SERVE_URL (serve class), FUNNEL_API_URL (funnel class), or both")
	}
	return cfg, nil
}

func run() error {
	ctrl.SetLogger(zap.New())
	probeAddr := flag.String("health-probe-bind-address", ":8080", "address of /healthz and /readyz")
	flag.Parse()
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := gwv1.Install(scheme); err != nil {
		return err
	}
	rc := ctrl.GetConfigOrDie()
	cfg.TCPRoutes, err = hasTCPRoute(rc)
	if err != nil {
		return fmt.Errorf("reading the API resources: %w", err)
	}
	podSel := labels.SelectorFromSet(labels.Set{controller.LabelManagedBy: "headlamp-gateway-controller"})
	mgr, err := ctrl.NewManager(rc, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: *probeAddr,
		// only the sidecar pods and Deployments go into the cache, not every pod of the cluster
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:        {Label: podSel},
			&appsv1.Deployment{}: {Label: podSel},
		}},
	})
	if err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ready := controller.HTTPReady(&http.Client{Timeout: 5 * time.Second})
	if err := controller.Setup(mgr, cfg, ready); err != nil {
		return err
	}
	ctrl.Log.Info("starting", "serveClass", cfg.ServeClass, "funnelClass", cfg.FunnelClass, "tcpRoutes", cfg.TCPRoutes)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// hasTCPRoute reports whether the TCPRoute CRD (gateway.networking.k8s.io/v1) is installed.
func hasTCPRoute(rc *rest.Config) (bool, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return false, err
	}
	list, err := dc.ServerResourcesForGroupVersion(gwv1.GroupVersion.String())
	if err != nil {
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Kind == "TCPRoute" {
			return true, nil
		}
	}
	return false, nil
}
