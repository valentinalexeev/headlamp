package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var testCfg = Config{ServeClass: "headlamp-serve", FunnelClass: "headlamp-funnel", Image: "funnel-sidecar:test",
	ServeURL: "http://100.64.0.1"}

func newScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gwv1.Install(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtures(match *gwv1.HTTPRouteMatch) []client.Object {
	gc := &gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "headlamp-serve"},
		Spec: gwv1.GatewayClassSpec{ControllerName: ControllerName}}
	gw := &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "gw", Generation: 2},
		Spec: gwv1.GatewaySpec{GatewayClassName: "headlamp-serve",
			Listeners: []gwv1.Listener{{Name: "web", Protocol: gwv1.HTTPSProtocolType, Port: 443,
				Hostname: ptr(gwv1.Hostname("grafana.example.com"))}},
			Infrastructure: &gwv1.GatewayInfrastructure{ParametersRef: &gwv1.LocalParametersReference{
				Kind: "Secret", Name: "gw-authkey"}}}}
	rule := gwv1.HTTPRouteRule{BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
		BackendObjectReference: gwv1.BackendObjectReference{Name: "web", Port: ptr(gwv1.PortNumber(80))}}}}}
	if match != nil {
		rule.Matches = []gwv1.HTTPRouteMatch{*match}
	}
	hr := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "r", Generation: 1},
		Spec: gwv1.HTTPRouteSpec{CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: "gw"}}},
			Rules: []gwv1.HTTPRouteRule{rule}}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10", Ports: []corev1.ServicePort{{Port: 80}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "gw-funnel-x",
		Labels: map[string]string{LabelGateway: "gw", LabelManagedBy: managedBy}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.5"}}
	return []client.Object{gc, gw, hr, svc, pod}
}

func newReconciler(t *testing.T, objs []client.Object, ready Readiness) (*GatewayReconciler, client.Client) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gwv1.Gateway{}, &gwv1.HTTPRoute{}, &gwv1.GatewayClass{}).Build()
	r := &GatewayReconciler{Client: c, Cfg: testCfg, Now: time.Now,
		Ready: func(_ context.Context, ip string) (Readiness, error) {
			if ip != "10.244.0.5" {
				t.Errorf("readyz asked at %s", ip)
			}
			return ready, nil
		}}
	return r, c
}

func reconcileGW(t *testing.T, r *GatewayReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "apps", Name: "gw"}}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileProgramsGatewayAndRoute(t *testing.T) {
	r, c := newReconciler(t, fixtures(nil), Readiness{Ready: true, Addresses: []string{"100.64.0.7", "fd7a:115c:a1e0::7"}})
	reconcileGW(t, r)

	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw-funnel"}, &dep); err != nil {
		t.Fatalf("no Deployment: %v", err)
	}
	envs := map[string]corev1.EnvVar{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		envs[e.Name] = e
	}
	if got := envs["FUNNEL_SERVICES"].Value; got != "grafana=443@10.96.0.10:80" {
		t.Errorf("FUNNEL_SERVICES = %q", got)
	}
	if ref := envs["TS_AUTHKEY"].ValueFrom; ref == nil || ref.SecretKeyRef.Name != "gw-authkey" || ref.SecretKeyRef.Key != "authkey" {
		t.Errorf("TS_AUTHKEY = %+v", envs["TS_AUTHKEY"])
	}
	if envs["FUNNEL_SERVE_URL"].Value != testCfg.ServeURL {
		t.Errorf("FUNNEL_SERVE_URL = %q", envs["FUNNEL_SERVE_URL"].Value)
	}
	if len(dep.OwnerReferences) != 1 || dep.OwnerReferences[0].Name != "gw" {
		t.Errorf("owner = %+v", dep.OwnerReferences)
	}

	var gw gwv1.Gateway
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw"}, &gw)
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionAccepted)) ||
		!meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed)) {
		t.Fatalf("gateway conditions = %+v", gw.Status.Conditions)
	}
	if len(gw.Status.Addresses) != 2 || gw.Status.Addresses[0].Value != "100.64.0.7" {
		t.Errorf("addresses = %+v", gw.Status.Addresses)
	}
	if len(gw.Status.Listeners) != 1 || gw.Status.Listeners[0].AttachedRoutes != 1 {
		t.Errorf("listeners = %+v", gw.Status.Listeners)
	}
	var hr gwv1.HTTPRoute
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "r"}, &hr)
	if len(hr.Status.Parents) != 1 || hr.Status.Parents[0].ControllerName != ControllerName ||
		!meta.IsStatusConditionTrue(hr.Status.Parents[0].Conditions, string(gwv1.RouteConditionAccepted)) {
		t.Fatalf("route status = %+v", hr.Status.Parents)
	}

	// a second pass with nothing changed writes nothing new
	rv := gw.ResourceVersion
	reconcileGW(t, r)
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw"}, &gw)
	if gw.ResourceVersion != rv {
		t.Error("an unchanged status was written again")
	}
}

func TestReconcilePendingUntilSidecarReady(t *testing.T) {
	r, c := newReconciler(t, fixtures(nil), Readiness{Ready: false, Addresses: []string{"100.64.0.7"}})
	reconcileGW(t, r)
	var gw gwv1.Gateway
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw"}, &gw)
	p := meta.FindStatusCondition(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed))
	if p == nil || p.Status != metav1.ConditionFalse || p.Reason != string(gwv1.GatewayReasonPending) {
		t.Fatalf("Programmed = %+v", p)
	}
	if len(gw.Status.Addresses) != 0 {
		t.Errorf("addresses before ready: %+v", gw.Status.Addresses)
	}
}

func TestReconcilePathMatchRejected(t *testing.T) {
	m := &gwv1.HTTPRouteMatch{Path: &gwv1.HTTPPathMatch{Type: ptr(gwv1.PathMatchPathPrefix), Value: ptr("/api")}}
	r, c := newReconciler(t, fixtures(m), Readiness{})
	reconcileGW(t, r)
	var hr gwv1.HTTPRoute
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "r"}, &hr)
	a := meta.FindStatusCondition(hr.Status.Parents[0].Conditions, string(gwv1.RouteConditionAccepted))
	if a == nil || a.Status != metav1.ConditionFalse || a.Reason != string(gwv1.RouteReasonUnsupportedValue) {
		t.Fatalf("Accepted = %+v", a)
	}
	var dep appsv1.Deployment
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw-funnel"}, &dep)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Errorf("replicas with no route = %v", dep.Spec.Replicas)
	}
}

func TestReconcileIgnoresOtherClasses(t *testing.T) {
	objs := fixtures(nil)
	objs[0].(*gwv1.GatewayClass).Spec.ControllerName = "example.com/other"
	r, c := newReconciler(t, objs, Readiness{})
	reconcileGW(t, r)
	var dep appsv1.Deployment
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "gw-funnel"}, &dep); err == nil {
		t.Fatal("a Deployment for a Gateway of another controller")
	}
}

func TestClassReconciler(t *testing.T) {
	objs := []client.Object{
		&gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "headlamp-serve"}, Spec: gwv1.GatewayClassSpec{ControllerName: ControllerName}},
		&gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: "other"}, Spec: gwv1.GatewayClassSpec{ControllerName: ControllerName}},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gwv1.GatewayClass{}).Build()
	cr := &ClassReconciler{Client: c, Cfg: testCfg, Now: time.Now}
	for _, n := range []string{"headlamp-serve", "other"} {
		if _, err := cr.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: n}}); err != nil {
			t.Fatal(err)
		}
	}
	var gc gwv1.GatewayClass
	_ = c.Get(context.Background(), types.NamespacedName{Name: "headlamp-serve"}, &gc)
	if !meta.IsStatusConditionTrue(gc.Status.Conditions, string(gwv1.GatewayClassConditionStatusAccepted)) {
		t.Errorf("headlamp-serve: %+v", gc.Status.Conditions)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "other"}, &gc)
	if meta.IsStatusConditionTrue(gc.Status.Conditions, string(gwv1.GatewayClassConditionStatusAccepted)) {
		t.Errorf("unknown class accepted: %+v", gc.Status.Conditions)
	}
}
