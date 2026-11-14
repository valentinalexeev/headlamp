package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/valentinalexeev/headlamp/apps/gateway-controller/internal/translate"
)

// routeObject gets a route and a pointer to its RouteStatus.
func (r *GatewayReconciler) routeObject(ctx context.Context, kind, ns, name string) (client.Object, *gwv1.RouteStatus, error) {
	key := types.NamespacedName{Namespace: ns, Name: name}
	switch kind {
	case translate.KindHTTPRoute:
		var o gwv1.HTTPRoute
		if err := r.Get(ctx, key, &o); err != nil {
			return nil, nil, err
		}
		return &o, &o.Status.RouteStatus, nil
	case translate.KindTCPRoute:
		var o gwv1.TCPRoute
		if err := r.Get(ctx, key, &o); err != nil {
			return nil, nil, err
		}
		return &o, &o.Status.RouteStatus, nil
	}
	return nil, nil, fmt.Errorf("unknown route kind %s", kind)
}

// writeRouteStatus writes one parent status entry per parentRef that names gw, and drops the entries of this
// controller for gw from routes that no longer name it. The tailnet addresses go into the Accepted message:
// RouteParentStatus has no address field.
func (r *GatewayReconciler) writeRouteStatus(ctx context.Context, gw *gwv1.Gateway, routes []translate.Route,
	res translate.Result, addrs []string) error {
	gwKey := types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}
	for _, rt := range routes {
		parents := res.Parents[rt.Key()]
		obj, st, err := r.routeObject(ctx, rt.Kind, rt.Namespace, rt.Name)
		if err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return err
		}
		old := st.DeepCopy()
		st.Parents = dropParents(st.Parents, rt.Namespace, gwKey)
		for _, p := range parents {
			st.Parents = append(st.Parents, r.parentStatus(rt, p, addrs, old.Parents))
		}
		if reflect.DeepEqual(old.Parents, st.Parents) {
			continue
		}
		if err := r.Status().Update(ctx, obj); err != nil {
			return fmt.Errorf("writing status of %s: %w", rt.Key(), err)
		}
	}
	return nil
}

// clearRoutes drops the status entries for a deleted Gateway.
func (r *GatewayReconciler) clearRoutes(ctx context.Context, gwKey types.NamespacedName) error {
	in, err := r.input(ctx, translate.ModeServe, &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Namespace: gwKey.Namespace, Name: gwKey.Name}})
	if err != nil {
		return err
	}
	gw := &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: gwKey.Namespace, Name: gwKey.Name}}
	return r.writeRouteStatus(ctx, gw, in.Routes, translate.Result{}, nil)
}

// dropParents removes the entries of this controller whose parentRef names gwKey.
func dropParents(ps []gwv1.RouteParentStatus, routeNS string, gwKey types.NamespacedName) []gwv1.RouteParentStatus {
	out := ps[:0:0]
	for _, p := range ps {
		ns := routeNS
		if p.ParentRef.Namespace != nil {
			ns = string(*p.ParentRef.Namespace)
		}
		if p.ControllerName == ControllerName && ns == gwKey.Namespace && string(p.ParentRef.Name) == gwKey.Name {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (r *GatewayReconciler) parentStatus(rt translate.Route, p translate.ParentResult, addrs []string,
	old []gwv1.RouteParentStatus) gwv1.RouteParentStatus {
	ps := gwv1.RouteParentStatus{ParentRef: p.Ref, ControllerName: ControllerName}
	for _, o := range old {
		if o.ControllerName == ControllerName && reflect.DeepEqual(o.ParentRef, p.Ref) {
			ps.Conditions = append([]metav1.Condition(nil), o.Conditions...)
		}
	}
	now := metav1.NewTime(r.Now())
	cond := func(typ string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&ps.Conditions, metav1.Condition{Type: typ, Status: s, Reason: reason,
			Message: msg, ObservedGeneration: rt.Generation, LastTransitionTime: now})
	}
	msg := p.AcceptMessage
	if p.Accepted && p.Resolved && len(addrs) > 0 {
		msg = fmt.Sprintf("the route is accepted; listener %s is on the tailnet at %s", p.Listener,
			strings.Join(addrs, ", "))
	}
	cond(string(gwv1.RouteConditionAccepted), p.Accepted, p.AcceptReason, msg)
	rMsg := p.ResolvedMessage
	if rMsg == "" {
		rMsg = "all references are resolved"
	}
	cond(string(gwv1.RouteConditionResolvedRefs), p.Resolved, p.ResolvedReason, rMsg)
	return ps
}

// equalGatewayStatus compares two statuses, so an unchanged status is not written again. SetStatusCondition
// keeps the transition time of a condition whose status does not change.
func equalGatewayStatus(a, b *gwv1.GatewayStatus) bool {
	return reflect.DeepEqual(a, b)
}
