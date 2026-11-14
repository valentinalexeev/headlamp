package translate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

func gateway(mode Mode, listeners ...gwv1.Listener) *gwv1.Gateway {
	return &gwv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "gw"},
		Spec: gwv1.GatewaySpec{
			GatewayClassName: gwv1.ObjectName("headlamp-" + string(mode)),
			Listeners:        listeners,
			Infrastructure: &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{
				Kind: "Secret", Name: "gw-authkey"}},
		},
	}
}

func listener(name string, proto gwv1.ProtocolType, port int32, host string) gwv1.Listener {
	l := gwv1.Listener{Name: gwv1.SectionName(name), Protocol: proto, Port: gwv1.PortNumber(port)}
	if host != "" {
		l.Hostname = ptr(gwv1.Hostname(host))
	}
	return l
}

func service(ns, name, ip string, port int32) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{ClusterIP: ip, Ports: []corev1.ServicePort{{Port: port}}}}
}

func httpRoute(ns, name string, match *gwv1.HTTPRouteMatch, backendNS string) *gwv1.HTTPRoute {
	b := gwv1.HTTPBackendRef{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{
		Name: "web", Port: ptr(gwv1.PortNumber(80))}}}
	if backendNS != "" {
		b.Namespace = ptr(gwv1.Namespace(backendNS))
	}
	rule := gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{b}}
	if match != nil {
		rule.Matches = []gwv1.HTTPRouteMatch{*match}
	}
	return &gwv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: gwv1.HTTPRouteSpec{
			CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{
				Name: "gw", Namespace: ptr(gwv1.Namespace("apps"))}}},
			Rules: []gwv1.HTTPRouteRule{rule},
		},
	}
}

func serveInput(routes ...*gwv1.HTTPRoute) Input {
	in := Input{Mode: ModeServe, Gateway: gateway(ModeServe, listener("web", gwv1.HTTPSProtocolType, 443, "grafana.example.com")),
		Services: map[string]*corev1.Service{"apps/web": service("apps", "web", "10.96.0.10", 80),
			"other/web": service("other", "web", "10.96.0.20", 80)}}
	for _, r := range routes {
		in.Routes = append(in.Routes, FromHTTPRoute(r))
	}
	return in
}

func TestTranslateHTTPRouteToFunnelServices(t *testing.T) {
	res := Translate(serveInput(httpRoute("apps", "r", nil, "")))
	if !res.Accepted {
		t.Fatalf("gateway not accepted: %s %s", res.AcceptReason, res.AcceptMessage)
	}
	if got, want := res.FunnelServices(), "grafana=443@10.96.0.10:80"; got != want {
		t.Fatalf("FUNNEL_SERVICES = %q, want %q", got, want)
	}
	p := res.Parents["HTTPRoute/apps/r"]
	if len(p) != 1 || !p[0].Accepted || !p[0].Resolved {
		t.Fatalf("route status = %+v", p)
	}
	if res.Listeners[0].AttachedRoutes != 1 {
		t.Fatalf("attachedRoutes = %d", res.Listeners[0].AttachedRoutes)
	}
}

func TestRootPrefixMatchIsAccepted(t *testing.T) {
	m := &gwv1.HTTPRouteMatch{Path: &gwv1.HTTPPathMatch{Type: ptr(gwv1.PathMatchPathPrefix), Value: ptr("/")}}
	res := Translate(serveInput(httpRoute("apps", "r", m, "")))
	if p := res.Parents["HTTPRoute/apps/r"]; !p[0].Accepted {
		t.Fatalf("PathPrefix / rejected: %+v", p[0])
	}
}

func TestUnsupportedMatchesAndFilters(t *testing.T) {
	cases := map[string]func(*gwv1.HTTPRoute){
		"path": func(r *gwv1.HTTPRoute) {
			r.Spec.Rules[0].Matches = []gwv1.HTTPRouteMatch{{Path: &gwv1.HTTPPathMatch{Value: ptr("/api")}}}
		},
		"header": func(r *gwv1.HTTPRoute) {
			r.Spec.Rules[0].Matches = []gwv1.HTTPRouteMatch{{Headers: []gwv1.HTTPHeaderMatch{{Name: "x", Value: "y"}}}}
		},
		"filter": func(r *gwv1.HTTPRoute) {
			r.Spec.Rules[0].Filters = []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestHeaderModifier}}
		},
		"backend filter": func(r *gwv1.HTTPRoute) {
			r.Spec.Rules[0].BackendRefs[0].Filters = []gwv1.HTTPRouteFilter{{Type: gwv1.HTTPRouteFilterRequestHeaderModifier}}
		},
		"two rules": func(r *gwv1.HTTPRoute) { r.Spec.Rules = append(r.Spec.Rules, r.Spec.Rules[0]) },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			r := httpRoute("apps", "r", nil, "")
			mod(r)
			res := Translate(serveInput(r))
			p := res.Parents["HTTPRoute/apps/r"]
			if len(p) != 1 || p[0].Accepted || p[0].AcceptReason != string(gwv1.RouteReasonUnsupportedValue) {
				t.Fatalf("status = %+v", p)
			}
			if res.FunnelServices() != "" {
				t.Fatalf("a rejected route was published: %s", res.FunnelServices())
			}
		})
	}
}

func TestCrossNamespaceBackendNeedsReferenceGrant(t *testing.T) {
	in := serveInput(httpRoute("apps", "r", nil, "other"))
	res := Translate(in)
	p := res.Parents["HTTPRoute/apps/r"][0]
	if !p.Accepted || p.Resolved || p.ResolvedReason != string(gwv1.RouteReasonRefNotPermitted) {
		t.Fatalf("without a grant: %+v", p)
	}
	if res.FunnelServices() != "" {
		t.Fatal("published without a grant")
	}
	in.Grants = GrantsFrom([]gwv1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "g"},
		Spec: gwv1.ReferenceGrantSpec{
			From: []gwv1.ReferenceGrantFrom{{Group: gwv1.GroupName, Kind: "HTTPRoute", Namespace: "apps"}},
			To:   []gwv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr(gwv1.ObjectName("web"))}},
		},
	}})
	res = Translate(in)
	if got := res.FunnelServices(); got != "grafana=443@10.96.0.20:80" {
		t.Fatalf("with a grant: %q %+v", got, res.Parents)
	}
	// a grant for another kind or another name does not count
	in.Grants[0].ToName = "db"
	if res = Translate(in); res.FunnelServices() != "" {
		t.Fatal("a grant for another Service permitted the reference")
	}
}

func TestGatewayNeedsParametersSecret(t *testing.T) {
	in := serveInput(httpRoute("apps", "r", nil, ""))
	in.Gateway.Spec.Infrastructure = nil
	res := Translate(in)
	if res.Accepted || res.AcceptReason != string(gwv1.GatewayReasonInvalidParameters) {
		t.Fatalf("accepted without parametersRef: %+v", res)
	}
	if ParametersSecret(in.Gateway) != "" {
		t.Fatal("ParametersSecret without parametersRef")
	}
}

func TestListenerValidation(t *testing.T) {
	gw := gateway(ModeServe,
		listener("plain", gwv1.HTTPProtocolType, 80, "a.example.com"),
		listener("wild", gwv1.HTTPSProtocolType, 443, "*.apps.example.com"),
		listener("db", gwv1.TCPProtocolType, 5432, ""),
	)
	res := Translate(Input{Mode: ModeServe, Gateway: gw})
	if res.Listeners[0].Valid || res.Listeners[0].Reason != string(gwv1.ListenerReasonUnsupportedProtocol) {
		t.Errorf("HTTP in serve mode: %+v", res.Listeners[0])
	}
	if res.Listeners[1].Valid || !strings.Contains(res.Listeners[1].Message, "wildcard") {
		t.Errorf("wildcard: %+v", res.Listeners[1])
	}
	if !res.Listeners[2].Valid || res.Listeners[2].SupportedKinds[0] != KindTCPRoute {
		t.Errorf("TCP: %+v", res.Listeners[2])
	}
	if !res.Accepted {
		t.Error("a Gateway with one valid listener is not accepted")
	}
	res = Translate(Input{Mode: ModeFunnel, Gateway: gateway(ModeFunnel, listener("p", gwv1.HTTPProtocolType, 80, "a.example.com"))})
	if !res.Listeners[0].Valid {
		t.Errorf("HTTP in funnel mode: %+v", res.Listeners[0])
	}
	res = Translate(Input{Mode: ModeFunnel, Gateway: gateway(ModeFunnel, listener("p", gwv1.HTTPSProtocolType, 443, "a.example.com"))})
	if res.Accepted || res.AcceptReason != string(gwv1.GatewayReasonListenersNotValid) {
		t.Errorf("HTTPS only in funnel mode: %+v", res)
	}
}

func TestConflictingListeners(t *testing.T) {
	gw := gateway(ModeServe, listener("a", gwv1.HTTPSProtocolType, 443, "a.example.com"),
		listener("b", gwv1.HTTPSProtocolType, 443, "b.example.com"))
	res := Translate(Input{Mode: ModeServe, Gateway: gw})
	if !res.Listeners[0].Conflicted || !res.Listeners[1].Conflicted || res.Accepted {
		t.Fatalf("same port: %+v", res)
	}
}

func TestTCPRoute(t *testing.T) {
	gw := gateway(ModeServe, listener("db", gwv1.TCPProtocolType, 5432, ""))
	tr := &gwv1.TCPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "t"},
		Spec: gwv1.TCPRouteSpec{CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
			Rules: []gwv1.TCPRouteRule{{BackendRefs: []gwv1.BackendRef{{BackendObjectReference: gwv1.BackendObjectReference{
				Name: "pg", Port: ptr(gwv1.PortNumber(5432))}}}}}}}
	in := Input{Mode: ModeServe, Gateway: gw, Routes: []Route{FromTCPRoute(tr)},
		Services: map[string]*corev1.Service{"apps/pg": service("apps", "pg", "10.96.0.30", 5432)}}
	res := Translate(in)
	if got := res.FunnelServices(); got != "db=5432@10.96.0.30:5432" {
		t.Fatalf("FUNNEL_SERVICES = %q (%+v)", got, res.Parents)
	}
	// an HTTPRoute cannot attach to a TCP listener
	in.Routes = []Route{FromHTTPRoute(httpRoute("apps", "r", nil, ""))}
	p := Translate(in).Parents["HTTPRoute/apps/r"][0]
	if p.Accepted || p.AcceptReason != string(gwv1.RouteReasonNotAllowedByListeners) {
		t.Fatalf("HTTPRoute on a TCP listener: %+v", p)
	}
}

func TestSecondRouteOnListenerIsRejected(t *testing.T) {
	res := Translate(serveInput(httpRoute("apps", "first", nil, ""), httpRoute("apps", "second", nil, "")))
	if !res.Parents["HTTPRoute/apps/first"][0].Accepted {
		t.Fatal("the first route is not accepted")
	}
	if p := res.Parents["HTTPRoute/apps/second"][0]; p.Accepted {
		t.Fatalf("the second route is accepted: %+v", p)
	}
	if len(res.Services) != 1 {
		t.Fatalf("services = %v", res.Services)
	}
}

func TestBackendErrors(t *testing.T) {
	in := serveInput(httpRoute("apps", "r", nil, ""))
	delete(in.Services, "apps/web")
	if p := Translate(in).Parents["HTTPRoute/apps/r"][0]; p.Resolved || p.ResolvedReason != string(gwv1.RouteReasonBackendNotFound) {
		t.Fatalf("missing Service: %+v", p)
	}
	in.Services["apps/web"] = service("apps", "web", "None", 80)
	if p := Translate(in).Parents["HTTPRoute/apps/r"][0]; p.Resolved {
		t.Fatalf("headless Service: %+v", p)
	}
	in.Services["apps/web"] = service("apps", "web", "10.96.0.10", 8080)
	if p := Translate(in).Parents["HTTPRoute/apps/r"][0]; p.Resolved {
		t.Fatalf("wrong port: %+v", p)
	}
}

func TestRouteFromOtherNamespaceNeedsAllowedRoutes(t *testing.T) {
	in := serveInput(httpRoute("team", "r", nil, ""))
	in.Services["team/web"] = service("team", "web", "10.96.0.40", 80)
	if p := Translate(in).Parents["HTTPRoute/team/r"][0]; p.Accepted {
		t.Fatalf("route from another namespace accepted with from=Same: %+v", p)
	}
	in.Gateway.Spec.Listeners[0].AllowedRoutes = &gwv1.AllowedRoutes{Namespaces: &gwv1.RouteNamespaces{
		From: ptr(gwv1.NamespacesFromAll)}}
	if got := Translate(in).FunnelServices(); got != "grafana=443@10.96.0.40:80" {
		t.Fatalf("from=All: %q", got)
	}
}
