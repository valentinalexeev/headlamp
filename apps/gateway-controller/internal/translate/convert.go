package translate

import (
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// FromHTTPRoute reduces an HTTPRoute. A rule may have no matches, or matches that are all the
// default
// "PathPrefix /" with no headers, query parameters or method. Rule filters are not supported.
func FromHTTPRoute(r *gwv1.HTTPRoute) Route {
	out := Route{
		Kind:       KindHTTPRoute,
		Namespace:  r.Namespace,
		Name:       r.Name,
		Generation: r.Generation,
		ParentRefs: r.Spec.ParentRefs,
		Hostnames:  r.Spec.Hostnames,
	}
	for _, rule := range r.Spec.Rules {
		rr := Rule{Unsupported: httpRuleUnsupported(rule)}
		for _, b := range rule.BackendRefs {
			rr.BackendRefs = append(
				rr.BackendRefs,
				gwv1.BackendRef{BackendObjectReference: b.BackendObjectReference,
					Weight: b.Weight},
			)
			if len(b.Filters) > 0 && rr.Unsupported == "" {
				rr.Unsupported = "backendRefs[].filters are not supported"
			}
		}
		out.Rules = append(out.Rules, rr)
	}
	return out
}

const oneBackend = "the sidecar forwards the whole name to one backend"

func httpRuleUnsupported(rule gwv1.HTTPRouteRule) string {
	if len(rule.Filters) > 0 {
		return "filters are not supported: " + oneBackend
	}
	for _, m := range rule.Matches {
		switch {
		case len(m.Headers) > 0:
			return "header matches are not supported: " + oneBackend
		case len(m.QueryParams) > 0:
			return "query parameter matches are not supported"
		case m.Method != nil:
			return "method matches are not supported"
		case m.Path != nil && !isRootPrefix(m.Path):
			return "path matches are not supported: " + oneBackend
		}
	}
	return ""
}

func isRootPrefix(p *gwv1.HTTPPathMatch) bool {
	t := gwv1.PathMatchPathPrefix
	if p.Type != nil {
		t = *p.Type
	}
	return t == gwv1.PathMatchPathPrefix && (p.Value == nil || *p.Value == "/")
}

// FromTCPRoute reduces a TCPRoute.
func FromTCPRoute(r *gwv1.TCPRoute) Route {
	out := Route{Kind: KindTCPRoute, Namespace: r.Namespace, Name: r.Name, Generation: r.Generation,
		ParentRefs: r.Spec.ParentRefs}
	for _, rule := range r.Spec.Rules {
		out.Rules = append(out.Rules, Rule{BackendRefs: rule.BackendRefs})
	}
	return out
}

// GrantsFrom flattens ReferenceGrants into one Grant per from/to pair that permits a route to a
// Service.
func GrantsFrom(list []gwv1.ReferenceGrant) []Grant {
	var out []Grant
	for _, rg := range list {
		for _, f := range rg.Spec.From {
			if f.Group != gwv1.GroupName || (f.Kind != KindHTTPRoute && f.Kind != KindTCPRoute) {
				continue
			}
			for _, t := range rg.Spec.To {
				if t.Group != "" || t.Kind != "Service" {
					continue
				}
				g := Grant{
					Namespace:     rg.Namespace,
					FromKind:      string(f.Kind),
					FromNamespace: string(f.Namespace),
				}
				if t.Name != nil {
					g.ToName = string(*t.Name)
				}
				out = append(out, g)
			}
		}
	}
	return out
}
