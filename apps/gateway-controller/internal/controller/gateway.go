package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/valentinalexeev/headlamp/apps/gateway-controller/internal/translate"
)

// GatewayReconciler reconciles the Gateways of the owned GatewayClasses.
type GatewayReconciler struct {
	client.Client
	Cfg   Config
	Ready ReadyFunc
	Now   func() time.Time
}

// modeOf gives the mode of a GatewayClass this controller owns, or "" when it does not own it.
func modeOf(cfg Config, gc *gwv1.GatewayClass) translate.Mode {
	if gc.Spec.ControllerName != ControllerName {
		return ""
	}
	switch gc.Name {
	case cfg.ServeClass:
		return translate.ModeServe
	case cfg.FunnelClass:
		return translate.ModeFunnel
	}
	return ""
}

// Reconcile brings the sidecar Deployment and the status of one Gateway and its routes up to date.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var gw gwv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			// the Deployment goes with its owner reference; drop route status entries for the Gateway
			return ctrl.Result{}, r.clearRoutes(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}
	var gc gwv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	mode := modeOf(r.Cfg, &gc)
	if mode == "" {
		return ctrl.Result{}, nil
	}
	in, err := r.input(ctx, mode, &gw)
	if err != nil {
		return ctrl.Result{}, err
	}
	res := translate.Translate(in)
	ready, depErr := r.syncDeployment(ctx, mode, &gw, res)
	if err := r.writeGatewayStatus(ctx, &gw, res, ready, depErr); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.writeRouteStatus(ctx, &gw, in.Routes, res, ready.Addresses); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.Cfg.Requeue}, depErr
}

// input lists the routes, Services and ReferenceGrants the translation reads.
func (r *GatewayReconciler) input(ctx context.Context, mode translate.Mode, gw *gwv1.Gateway) (translate.Input, error) {
	in := translate.Input{Mode: mode, Gateway: gw, Services: map[string]*corev1.Service{}}
	type aged struct {
		t  metav1.Time
		rt translate.Route
	}
	var all []aged
	var hr gwv1.HTTPRouteList
	if err := r.List(ctx, &hr); err != nil {
		return in, fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	for i := range hr.Items {
		all = append(all, aged{hr.Items[i].CreationTimestamp, translate.FromHTTPRoute(&hr.Items[i])})
	}
	if r.Cfg.TCPRoutes {
		var tr gwv1.TCPRouteList
		if err := r.List(ctx, &tr); err != nil {
			return in, fmt.Errorf("listing TCPRoutes: %w", err)
		}
		for i := range tr.Items {
			all = append(all, aged{tr.Items[i].CreationTimestamp, translate.FromTCPRoute(&tr.Items[i])})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].t.Equal(&all[j].t) {
			return all[i].t.Before(&all[j].t)
		}
		return all[i].rt.Key() < all[j].rt.Key()
	})
	for _, a := range all {
		in.Routes = append(in.Routes, a.rt)
	}
	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs); err != nil {
		return in, fmt.Errorf("listing Services: %w", err)
	}
	for i := range svcs.Items {
		s := &svcs.Items[i]
		in.Services[s.Namespace+"/"+s.Name] = s
	}
	var grants gwv1.ReferenceGrantList
	if err := r.List(ctx, &grants); err != nil {
		return in, fmt.Errorf("listing ReferenceGrants: %w", err)
	}
	in.Grants = translate.GrantsFrom(grants.Items)
	return in, nil
}

// syncDeployment creates or updates the sidecar Deployment and reads the readiness of its pod.
func (r *GatewayReconciler) syncDeployment(ctx context.Context, mode translate.Mode, gw *gwv1.Gateway,
	res translate.Result) (Readiness, error) {
	want := buildDeployment(r.Cfg, mode, gw, res)
	if !res.Accepted {
		err := r.Delete(ctx, want)
		return Readiness{}, client.IgnoreNotFound(err)
	}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = want.Labels
		dep.Spec = want.Spec
		return controllerutil.SetControllerReference(gw, dep, r.Scheme())
	})
	if err != nil {
		return Readiness{}, fmt.Errorf("writing Deployment %s: %w", want.Name, err)
	}
	if res.FunnelServices() == "" {
		return Readiness{}, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(gw.Namespace),
		client.MatchingLabels{LabelGateway: gw.Name, LabelManagedBy: managedBy}); err != nil {
		return Readiness{}, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" || p.DeletionTimestamp != nil {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		rd, err := r.Ready(c, p.Status.PodIP)
		cancel()
		if err == nil {
			return rd, nil
		}
		ctrl.LoggerFrom(ctx).Info("reading the sidecar /readyz failed", "pod", p.Name, "error", err.Error())
	}
	return Readiness{}, nil
}

// writeGatewayStatus sets Accepted and Programmed on the Gateway and its listeners, and the tailnet addresses.
func (r *GatewayReconciler) writeGatewayStatus(ctx context.Context, gw *gwv1.Gateway, res translate.Result,
	ready Readiness, depErr error) error {
	st := gw.Status.DeepCopy()
	gen := gw.Generation
	now := metav1.NewTime(r.Now())
	set := func(conds *[]metav1.Condition, typ string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(conds, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg,
			ObservedGeneration: gen, LastTransitionTime: now})
	}
	set(&st.Conditions, string(gwv1.GatewayConditionAccepted), res.Accepted, res.AcceptReason, res.AcceptMessage)
	prog, pReason, pMsg := programmed(res, ready, depErr)
	set(&st.Conditions, string(gwv1.GatewayConditionProgrammed), prog, pReason, pMsg)
	st.Addresses = nil
	if prog {
		for _, a := range ready.Addresses {
			st.Addresses = append(st.Addresses, gwv1.GatewayStatusAddress{
				Type: ptr(gwv1.IPAddressType), Value: a})
		}
	}
	st.Listeners = listenerStatus(st.Listeners, res, prog, set)
	if equalGatewayStatus(&gw.Status, st) {
		return nil
	}
	gw.Status = *st
	if err := r.Status().Update(ctx, gw); err != nil {
		return fmt.Errorf("writing Gateway status: %w", err)
	}
	return nil
}

// programmed decides the Programmed condition of the Gateway: the sidecar answers /readyz with ready and
// at least one tailnet address.
func programmed(res translate.Result, ready Readiness, depErr error) (bool, string, string) {
	switch {
	case !res.Accepted:
		return false, string(gwv1.GatewayReasonInvalid), "the Gateway is not accepted"
	case depErr != nil:
		return false, string(gwv1.GatewayReasonInvalid), depErr.Error()
	case len(res.Services) == 0:
		return false, string(gwv1.GatewayReasonPending), "no route is attached; the sidecar has 0 replicas"
	case !ready.Ready || len(ready.Addresses) == 0:
		return false, string(gwv1.GatewayReasonPending), "the funnel-sidecar pod is not ready yet"
	}
	return true, string(gwv1.GatewayReasonProgrammed),
		"published on the tailnet at " + strings.Join(ready.Addresses, ", ")
}

func listenerStatus(old []gwv1.ListenerStatus, res translate.Result, prog bool,
	set func(*[]metav1.Condition, string, bool, string, string)) []gwv1.ListenerStatus {
	out := make([]gwv1.ListenerStatus, 0, len(res.Listeners))
	for _, l := range res.Listeners {
		ls := gwv1.ListenerStatus{Name: l.Name, AttachedRoutes: l.AttachedRoutes, SupportedKinds: []gwv1.RouteGroupKind{}}
		for _, o := range old {
			if o.Name == l.Name {
				ls.Conditions = o.Conditions
			}
		}
		for _, k := range l.SupportedKinds {
			ls.SupportedKinds = append(ls.SupportedKinds, gwv1.RouteGroupKind{
				Group: ptr(gwv1.Group(gwv1.GroupName)), Kind: gwv1.Kind(k)})
		}
		ok := l.Valid && !l.Conflicted
		msg := l.Message
		if msg == "" {
			msg = "the listener is valid"
		}
		set(&ls.Conditions, string(gwv1.ListenerConditionAccepted), l.Valid, l.Reason, msg)
		cReason := string(gwv1.ListenerReasonNoConflicts)
		if l.Conflicted {
			cReason = string(gwv1.ListenerReasonHostnameConflict)
		}
		set(&ls.Conditions, string(gwv1.ListenerConditionConflicted), l.Conflicted, cReason, msg)
		set(&ls.Conditions, string(gwv1.ListenerConditionResolvedRefs), ok, resolvedReason(ok), msg)
		pReason := string(gwv1.ListenerReasonProgrammed)
		if !(ok && prog) {
			pReason = string(gwv1.ListenerReasonPending)
		}
		set(&ls.Conditions, string(gwv1.ListenerConditionProgrammed), ok && prog, pReason, msg)
		out = append(out, ls)
	}
	return out
}

func resolvedReason(ok bool) string {
	if ok {
		return string(gwv1.ListenerReasonResolvedRefs)
	}
	return string(gwv1.ListenerReasonInvalid)
}

func ptr[T any](v T) *T { return &v }
