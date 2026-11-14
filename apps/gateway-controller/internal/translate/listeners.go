package translate

import (
	"fmt"
	"strings"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// listenerKinds gives the route kind a listener protocol takes in a mode, or "" when the mode does
// not support the protocol. Serve mode terminates TLS in the sidecar (HTTPS). Funnel mode gets
// plain HTTP from
// funnel-ingress, which terminates the public TLS. TCP forwards bytes in both modes.
func listenerKinds(mode Mode, p gwv1.ProtocolType) string {
	switch {
	case p == gwv1.TCPProtocolType:
		return KindTCPRoute
	case mode == ModeServe && p == gwv1.HTTPSProtocolType:
		return KindHTTPRoute
	case mode == ModeFunnel && p == gwv1.HTTPProtocolType:
		return KindHTTPRoute
	}
	return ""
}

// serviceName is the FUNNEL_SERVICES name of a listener: the first label of its hostname, or the
// listener
// name when it has no hostname (TCP). Wildcards are not supported: one listener is one name.
func serviceName(l gwv1.Listener) (string, string) {
	if l.Hostname == nil || *l.Hostname == "" {
		if !labelRE.MatchString(string(l.Name)) {
			return "", "a listener without a hostname uses its name as the service name; " +
				"it must be a DNS label"
		}
		return string(l.Name), ""
	}
	h := string(*l.Hostname)
	if strings.HasPrefix(h, "*") {
		return "", "wildcard hostnames are not supported: " +
			"put a Gateway implementation behind a wildcard " +
			"serve service instead"
	}
	first, _, _ := strings.Cut(h, ".")
	return first, ""
}

// checkListeners validates each listener and finds the certificate Secret of the serve mode.
func checkListeners(mode Mode, gw *gwv1.Gateway) ([]ListenerResult, string) {
	out := make([]ListenerResult, 0, len(gw.Spec.Listeners))
	names := map[string]int{}
	ports := map[gwv1.PortNumber]int{}
	cert := ""
	for _, l := range gw.Spec.Listeners {
		lr := ListenerResult{Name: l.Name, Valid: true, Reason: string(gwv1.ListenerReasonAccepted)}
		kind := listenerKinds(mode, l.Protocol)
		if kind != "" {
			lr.SupportedKinds = []string{kind}
		}
		var msg string
		lr.service, msg = serviceName(l)
		switch {
		case kind == "":
			lr.Reason = string(gwv1.ListenerReasonUnsupportedProtocol)
			msg = fmt.Sprintf("protocol %s is not supported by the %s mode", l.Protocol, mode)
		case msg != "":
			lr.Reason = string(gwv1.ListenerReasonUnsupportedValue)
		default:
			msg = checkAllowed(l, kind)
			if msg == "" {
				msg = checkTLS(mode, l, &cert)
			}
			if msg != "" {
				lr.Reason = string(gwv1.ListenerReasonUnsupportedValue)
			}
		}
		if msg != "" {
			lr.Valid, lr.Message = false, msg
		} else {
			names[lr.service]++
			ports[l.Port]++
		}
		out = append(out, lr)
	}
	for i, l := range gw.Spec.Listeners {
		if out[i].Valid && (names[out[i].service] > 1 || ports[l.Port] > 1) {
			out[i].Conflicted = true
			out[i].Reason = string(gwv1.ListenerReasonHostnameConflict)
			out[i].Message = "two listeners have the same service name or port; " +
				"the sidecar needs one of each"
		}
	}
	return out, cert
}

// checkAllowed checks allowedRoutes: namespaces Same or All, and only the kind of the protocol.
func checkAllowed(l gwv1.Listener, kind string) string {
	if l.AllowedRoutes == nil {
		return ""
	}
	if ns := l.AllowedRoutes.Namespaces; ns != nil && ns.From != nil &&
		*ns.From != gwv1.NamespacesFromSame && *ns.From != gwv1.NamespacesFromAll {
		return "allowedRoutes.namespaces.from must be Same or All"
	}
	for _, k := range l.AllowedRoutes.Kinds {
		if string(k.Kind) != kind || (k.Group != nil && *k.Group != gwv1.GroupName) {
			return fmt.Sprintf("allowedRoutes.kinds: this listener takes %s only", kind)
		}
	}
	return ""
}

// checkTLS checks the tls block. Serve mode: HTTPS terminates in the sidecar. Without
// certificateRefs the sidecar gets its certificate from node-control. With them, all HTTPS
// listeners must name the same one
// Secret in the namespace of the Gateway (the sidecar has one certificate).
func checkTLS(mode Mode, l gwv1.Listener, cert *string) string {
	if l.TLS == nil || mode != ModeServe {
		return ""
	}
	if l.TLS.Mode != nil && *l.TLS.Mode != gwv1.TLSModeTerminate {
		return "tls.mode must be Terminate"
	}
	refs := l.TLS.CertificateRefs
	switch {
	case len(refs) == 0:
		return ""
	case len(refs) > 1:
		return "tls.certificateRefs: give one Secret"
	}
	r := refs[0]
	if (r.Group != nil && *r.Group != "") || (r.Kind != nil && *r.Kind != "Secret") ||
		r.Namespace != nil {
		return "tls.certificateRefs: give a Secret in the namespace of the Gateway"
	}
	if *cert != "" && *cert != string(r.Name) {
		return "tls.certificateRefs: all listeners must name the same Secret"
	}
	*cert = string(r.Name)
	return ""
}
