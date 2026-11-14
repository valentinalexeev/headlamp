package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ClassReconciler sets Accepted on the GatewayClasses this controller owns.
type ClassReconciler struct {
	client.Client
	Cfg Config
	Now func() time.Time
}

// Reconcile accepts an owned GatewayClass whose name is one of the two configured classes.
func (r *ClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var gc gwv1.GatewayClass
	if err := r.Get(ctx, req.NamespacedName, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != ControllerName {
		return ctrl.Result{}, nil
	}
	c := metav1.Condition{
		Type:               string(gwv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gwv1.GatewayClassReasonAccepted),
		ObservedGeneration: gc.Generation,
		LastTransitionTime: metav1.NewTime(r.Now()),
	}
	switch {
	case modeOf(r.Cfg, &gc) == "":
		c.Status, c.Reason = metav1.ConditionFalse, string(gwv1.GatewayClassReasonInvalidParameters)
		c.Message = fmt.Sprintf(
			"the controller serves the classes %s and %s only",
			r.Cfg.ServeClass,
			r.Cfg.FunnelClass,
		)
	case gc.Spec.ParametersRef != nil:
		c.Status, c.Reason = metav1.ConditionFalse, string(gwv1.GatewayClassReasonInvalidParameters)
		c.Message = "parametersRef is not supported on the class; " +
			"set spec.infrastructure.parametersRef on each Gateway"
	default:
		c.Message = "the class is accepted (mode " + string(modeOf(r.Cfg, &gc)) + ")"
	}
	old := gc.Status.DeepCopy()
	meta.SetStatusCondition(&gc.Status.Conditions, c)
	if equalConds(old.Conditions, gc.Status.Conditions) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, &gc)
}

func equalConds(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Setup registers both reconcilers with the manager. Any change to a route, Service, ReferenceGrant
// or sidecar pod reconciles every Gateway: there are few Gateways, and a full pass keeps the "one
// route per
// listener" choice stable.
func Setup(mgr ctrl.Manager, cfg Config, ready ReadyFunc) error {
	now := time.Now
	if err := ctrl.NewControllerManagedBy(mgr).Named("gatewayclass").For(&gwv1.GatewayClass{}).
		Complete(&ClassReconciler{Client: mgr.GetClient(), Cfg: cfg, Now: now}); err != nil {
		return err
	}
	all := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []reconcile.Request {
			var gws gwv1.GatewayList
			if err := mgr.GetClient().List(ctx, &gws); err != nil {
				return nil
			}
			out := make([]reconcile.Request, 0, len(gws.Items))
			for _, g := range gws.Items {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&g)})
			}
			return out
		},
	)
	b := ctrl.NewControllerManagedBy(mgr).Named("gateway").For(&gwv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Watches(&gwv1.GatewayClass{}, all).
		Watches(&gwv1.HTTPRoute{}, all).
		Watches(&gwv1.ReferenceGrant{}, all).
		Watches(&corev1.Service{}, all).
		Watches(&corev1.Pod{}, all)
	if cfg.TCPRoutes {
		b = b.Watches(&gwv1.TCPRoute{}, all)
	}
	return b.Complete(&GatewayReconciler{Client: mgr.GetClient(), Cfg: cfg, Ready: ready, Now: now})
}
