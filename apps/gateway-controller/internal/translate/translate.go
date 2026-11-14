// Package translate turns one Gateway and the routes attached to it into the configuration of one
// funnel-sidecar (FUNNEL_SERVICES) and into the status conditions of the Gateway, its listeners and
// the routes. It has no Kubernetes client: the controller reads the objects and passes them in, so
// the rules
// are tested without a cluster (#338).
package translate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Mode is the publishing mode of a GatewayClass.
type Mode string

const (
	// ModeServe publishes tailnet-only names with TLS (funnel-sidecar serve mode).
	ModeServe Mode = "serve"
	// ModeFunnel publishes public names through funnel-ingress.
	ModeFunnel Mode = "funnel"
)

// Route kinds the controller reads.
const (
	KindHTTPRoute = "HTTPRoute"
	KindTCPRoute  = "TCPRoute"
)

// Reasons the controller sets in addition to the standard Gateway API reasons.
const (
	ReasonNoRoutes = "NoRoutes"
)

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Rule is one rule of a route, reduced to what the sidecar can do. Unsupported is empty when the
// sidecar can apply the rule, else it says why not (a path match, a header match, a filter).
type Rule struct {
	Unsupported string
	BackendRefs []gwv1.BackendRef
}

// Route is an HTTPRoute or a TCPRoute, reduced to the fields the translation reads.
type Route struct {
	Kind       string
	Namespace  string
	Name       string
	Generation int64
	ParentRefs []gwv1.ParentReference
	Hostnames  []gwv1.Hostname
	Rules      []Rule
}

// Key is "<kind>/<namespace>/<name>".
func (r Route) Key() string { return r.Kind + "/" + r.Namespace + "/" + r.Name }

// Grant is one "from" and one "to" entry of a ReferenceGrant in namespace Namespace.
type Grant struct {
	Namespace     string // the namespace of the ReferenceGrant (the target namespace)
	FromKind      string
	FromNamespace string
	ToName        string // empty: every Service in Namespace
}

// Input is everything the translation of one Gateway reads.
type Input struct {
	Mode     Mode
	Gateway  *gwv1.Gateway
	Routes   []Route                    // sorted oldest first by the caller; ties by Key
	Services map[string]*corev1.Service // "<namespace>/<name>"
	Grants   []Grant
}

// Service is one entry of FUNNEL_SERVICES.
type Service struct {
	Name        string
	TailnetPort int32
	Target      string // host:port
}

// String gives the FUNNEL_SERVICES form name=port@host:port.
func (s Service) String() string { return fmt.Sprintf("%s=%d@%s", s.Name, s.TailnetPort, s.Target) }

// Result is the outcome of one translation.
type Result struct {
	Accepted      bool
	AcceptReason  string
	AcceptMessage string
	Services      []Service
	CertSecret    string // serve mode with HTTPS listeners: the Secret with tls.crt and tls.key
	Listeners     []ListenerResult
	// Parents maps a route key to one result per parentRef that names this Gateway.
	Parents map[string][]ParentResult
}

// FunnelServices is the FUNNEL_SERVICES value, empty when no listener has a route.
func (r Result) FunnelServices() string {
	parts := make([]string, 0, len(r.Services))
	for _, s := range r.Services {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, ",")
}

// ListenerResult is the status of one listener.
type ListenerResult struct {
	Name           gwv1.SectionName
	Valid          bool
	Reason         string
	Message        string
	Conflicted     bool
	AttachedRoutes int32
	SupportedKinds []string
	service        string // the service name in FUNNEL_SERVICES
}

// ParentResult is the status of one route for one parentRef.
type ParentResult struct {
	Ref             gwv1.ParentReference
	Accepted        bool
	AcceptReason    string
	AcceptMessage   string
	Resolved        bool
	ResolvedReason  string
	ResolvedMessage string
	Listener        gwv1.SectionName // the listener the route got, when accepted and resolved
}

// Translate computes the sidecar configuration and the status of one Gateway.
func Translate(in Input) Result {
	res := Result{Parents: map[string][]ParentResult{}}
	gw := in.Gateway
	if msg := checkParameters(gw); msg != "" {
		res.AcceptReason, res.AcceptMessage = string(gwv1.GatewayReasonInvalidParameters), msg
		return res
	}
	res.Listeners, res.CertSecret = checkListeners(in.Mode, gw)
	anyValid := false
	for _, l := range res.Listeners {
		anyValid = anyValid || (l.Valid && !l.Conflicted)
	}
	if !anyValid {
		res.AcceptReason = string(gwv1.GatewayReasonListenersNotValid)
		res.AcceptMessage = "no listener is valid; see the listener conditions"
		return res
	}
	res.Accepted, res.AcceptReason = true, string(gwv1.GatewayReasonAccepted)
	res.AcceptMessage = "the Gateway is accepted"
	taken := map[gwv1.SectionName]string{} // listener -> route key
	for _, rt := range in.Routes {
		for _, ref := range rt.ParentRefs {
			if !refersTo(ref, rt.Namespace, gw) {
				continue
			}
			pr := attach(in, &res, rt, ref, taken)
			res.Parents[rt.Key()] = append(res.Parents[rt.Key()], pr)
		}
	}
	sort.Slice(
		res.Services,
		func(i, j int) bool { return res.Services[i].TailnetPort < res.Services[j].TailnetPort },
	)
	return res
}

// checkParameters checks spec.infrastructure.parametersRef: it must name a Secret (the auth key).
func checkParameters(gw *gwv1.Gateway) string {
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		return "spec.infrastructure.parametersRef must name the Secret " +
			"with the auth key (key authkey)"
	}
	p := gw.Spec.Infrastructure.ParametersRef
	if p.Group != "" || p.Kind != "Secret" {
		return "spec.infrastructure.parametersRef must be a core Secret (group \"\", kind Secret)"
	}
	return ""
}

// ParametersSecret is the name of the Secret with the auth key, or "" when parametersRef is not
// valid.
func ParametersSecret(gw *gwv1.Gateway) string {
	if checkParameters(gw) != "" {
		return ""
	}
	return gw.Spec.Infrastructure.ParametersRef.Name
}

// refersTo reports whether ref (of a route in routeNS) names gw.
func refersTo(ref gwv1.ParentReference, routeNS string, gw *gwv1.Gateway) bool {
	if ref.Group != nil && *ref.Group != gwv1.GroupName {
		return false
	}
	if ref.Kind != nil && *ref.Kind != "Gateway" {
		return false
	}
	ns := routeNS
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	return ns == gw.Namespace && string(ref.Name) == gw.Name
}
