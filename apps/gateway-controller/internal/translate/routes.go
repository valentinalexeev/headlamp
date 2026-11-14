package translate

import (
	"fmt"
	"net"
	"strconv"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// attach decides whether rt (through ref) attaches to a listener of the Gateway, and adds its target to
// FUNNEL_SERVICES. One listener takes one route: the oldest route wins, the others get Accepted=False.
func attach(in Input, res *Result, rt Route, ref gwv1.ParentReference, taken map[gwv1.SectionName]string) ParentResult {
	pr := ParentResult{Ref: ref, Resolved: true, ResolvedReason: string(gwv1.RouteReasonResolvedRefs)}
	reject := func(reason, msg string) ParentResult {
		pr.Accepted, pr.AcceptReason, pr.AcceptMessage = false, reason, msg
		return pr
	}
	if msg := unsupportedRule(rt); msg != "" {
		return reject(string(gwv1.RouteReasonUnsupportedValue), msg)
	}
	li, reason, msg := pickListener(in, res, rt, ref)
	if li < 0 {
		return reject(reason, msg)
	}
	l := &res.Listeners[li]
	if owner, ok := taken[l.Name]; ok && owner != rt.Key() {
		return reject(string(gwv1.RouteReasonNotAllowedByListeners),
			fmt.Sprintf("listener %s already has route %s; the sidecar sends one name to one backend", l.Name, owner))
	}
	target, rReason, rMsg := resolveBackend(in, rt)
	pr.Accepted, pr.AcceptReason = true, string(gwv1.RouteReasonAccepted)
	pr.AcceptMessage = "the route is accepted"
	pr.Listener = l.Name
	l.AttachedRoutes++
	if rReason != "" {
		pr.Resolved, pr.ResolvedReason, pr.ResolvedMessage = false, rReason, rMsg
		return pr
	}
	if _, ok := taken[l.Name]; !ok {
		taken[l.Name] = rt.Key()
		res.Services = append(res.Services, Service{Name: l.service, TailnetPort: int32(listenerPort(in, l.Name)),
			Target: target})
	}
	return pr
}

// unsupportedRule gives the reason the sidecar cannot apply the route, or "". The sidecar forwards a whole
// name to one backend: it has one rule, no matches other than "everything", no filters, one backendRef.
func unsupportedRule(rt Route) string {
	if len(rt.Rules) != 1 {
		return fmt.Sprintf("the route has %d rules; the sidecar supports exactly one", len(rt.Rules))
	}
	r := rt.Rules[0]
	if r.Unsupported != "" {
		return r.Unsupported
	}
	if len(r.BackendRefs) != 1 {
		return fmt.Sprintf("the rule has %d backendRefs; the sidecar supports exactly one", len(r.BackendRefs))
	}
	return ""
}

// pickListener finds the listener ref selects: by sectionName, port and route kind. It returns -1 and the
// reason when there is none.
func pickListener(in Input, res *Result, rt Route, ref gwv1.ParentReference) (int, string, string) {
	found := -1
	reason, msg := string(gwv1.RouteReasonNoMatchingParent), "no listener of the Gateway matches the parentRef"
	for i, lr := range res.Listeners {
		l := in.Gateway.Spec.Listeners[i]
		if ref.SectionName != nil && *ref.SectionName != l.Name {
			continue
		}
		if ref.Port != nil && *ref.Port != l.Port {
			continue
		}
		if !lr.Valid || lr.Conflicted {
			continue
		}
		if len(lr.SupportedKinds) == 0 || lr.SupportedKinds[0] != rt.Kind {
			reason, msg = string(gwv1.RouteReasonNotAllowedByListeners), "no listener takes "+rt.Kind
			continue
		}
		if !namespaceAllowed(l, in.Gateway.Namespace, rt.Namespace) {
			reason, msg = string(gwv1.RouteReasonNotAllowedByListeners), "allowedRoutes does not permit this namespace"
			continue
		}
		if !hostnameMatches(l, rt.Hostnames) {
			reason = string(gwv1.RouteReasonNoMatchingListenerHostname)
			msg = "no hostname of the route matches the listener hostname"
			continue
		}
		if found >= 0 {
			return -1, string(gwv1.RouteReasonUnsupportedValue),
				"the parentRef matches more than one listener; set sectionName"
		}
		found = i
	}
	return found, reason, msg
}

func namespaceAllowed(l gwv1.Listener, gwNS, routeNS string) bool {
	from := gwv1.NamespacesFromSame
	if l.AllowedRoutes != nil && l.AllowedRoutes.Namespaces != nil && l.AllowedRoutes.Namespaces.From != nil {
		from = *l.AllowedRoutes.Namespaces.From
	}
	return from == gwv1.NamespacesFromAll || gwNS == routeNS
}

// hostnameMatches: a route without hostnames matches every listener; otherwise one hostname must equal the
// listener hostname (listener wildcards are rejected earlier).
func hostnameMatches(l gwv1.Listener, hs []gwv1.Hostname) bool {
	if len(hs) == 0 || l.Hostname == nil {
		return true
	}
	for _, h := range hs {
		if h == *l.Hostname {
			return true
		}
	}
	return false
}

func listenerPort(in Input, name gwv1.SectionName) gwv1.PortNumber {
	for _, l := range in.Gateway.Spec.Listeners {
		if l.Name == name {
			return l.Port
		}
	}
	return 0
}

// resolveBackend gives host:port of the ClusterIP of the one backendRef, or a ResolvedRefs reason.
func resolveBackend(in Input, rt Route) (string, string, string) {
	b := rt.Rules[0].BackendRefs[0]
	if (b.Group != nil && *b.Group != "") || (b.Kind != nil && *b.Kind != "Service") {
		return "", string(gwv1.RouteReasonInvalidKind), "backendRefs must name a core Service"
	}
	ns := rt.Namespace
	if b.Namespace != nil {
		ns = string(*b.Namespace)
	}
	if ns != rt.Namespace && !granted(in.Grants, rt, ns, string(b.Name)) {
		return "", string(gwv1.RouteReasonRefNotPermitted),
			fmt.Sprintf("no ReferenceGrant in namespace %s permits %s from namespace %s", ns, rt.Kind, rt.Namespace)
	}
	if b.Port == nil {
		return "", string(gwv1.RouteReasonUnsupportedValue), "backendRefs[].port is required"
	}
	svc, ok := in.Services[ns+"/"+string(b.Name)]
	if !ok {
		return "", string(gwv1.RouteReasonBackendNotFound), fmt.Sprintf("Service %s/%s not found", ns, b.Name)
	}
	ip := svc.Spec.ClusterIP
	if ip == "" || ip == "None" {
		return "", string(gwv1.RouteReasonUnsupportedValue),
			fmt.Sprintf("Service %s/%s has no ClusterIP (headless or ExternalName)", ns, b.Name)
	}
	for _, p := range svc.Spec.Ports {
		if p.Port == int32(*b.Port) {
			return net.JoinHostPort(ip, strconv.Itoa(int(p.Port))), "", ""
		}
	}
	return "", string(gwv1.RouteReasonBackendNotFound),
		fmt.Sprintf("Service %s/%s has no port %d", ns, b.Name, *b.Port)
}

// granted reports whether a ReferenceGrant in ns permits rt to refer to the Service name.
func granted(grants []Grant, rt Route, ns, name string) bool {
	for _, g := range grants {
		if g.Namespace == ns && g.FromKind == rt.Kind && g.FromNamespace == rt.Namespace &&
			(g.ToName == "" || g.ToName == name) {
			return true
		}
	}
	return false
}
