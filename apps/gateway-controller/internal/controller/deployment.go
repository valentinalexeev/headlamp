package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/valentinalexeev/headlamp/apps/gateway-controller/internal/translate"
)

// Labels on the Deployment and its pods.
const (
	LabelGateway   = "gateway.headlamp.dev/gateway"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "headlamp-gateway-controller"
	authKeyKey     = "authkey"
	certMountPath  = "/etc/funnel-sidecar/tls"
	caMountPath    = "/etc/funnel-sidecar/ca"
	runAsUser      = 10001
	tmpSizeLimit   = 64 << 20 // bytes of the /tmp emptyDir
	readyPeriod    = 10       // seconds between readiness probes
	readyPortNum   = 8081     // FUNNEL_READY_ADDR of the sidecar
)

// DeploymentName is the name of the sidecar Deployment of a Gateway.
func DeploymentName(gw *gwv1.Gateway) string { return gw.Name + "-funnel" }

// buildDeployment gives the desired sidecar Deployment of gw. The auth key stays in its Secret: the
// pod reads it through secretKeyRef, so the controller never reads Secrets. With no service to
// publish the Deployment
// has 0 replicas (the sidecar refuses an empty FUNNEL_SERVICES).
func buildDeployment(
	cfg Config,
	mode translate.Mode,
	gw *gwv1.Gateway,
	res translate.Result,
) *appsv1.Deployment {
	labels := map[string]string{LabelGateway: gw.Name, LabelManagedBy: managedBy}
	replicas := int32(1)
	services := res.FunnelServices()
	if services == "" {
		replicas = 0
	}
	env := []corev1.EnvVar{
		{
			Name: "TS_AUTHKEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: translate.ParametersSecret(gw),
				}, Key: authKeyKey}},
		},
		{Name: "FUNNEL_SERVICES", Value: services},
		{Name: "FUNNEL_HOSTNAME", Value: gw.Name},
		{Name: "FUNNEL_STATE_DIR", Value: "/tmp/funnel-sidecar"},
	}
	if cfg.ControlURL != "" {
		env = append(env, corev1.EnvVar{Name: "FUNNEL_CONTROL_URL", Value: cfg.ControlURL})
	}
	env, mounts, vols := modeConfig(cfg, mode, gw, res, env)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DeploymentName(gw),
			Namespace: gw.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			// one ephemeral tailnet node per Gateway: stop the old pod before the new one joins
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec(cfg.Image, env, mounts, vols),
			},
		},
	}
}

// modeConfig adds the settings of the mode (funnel, serve with a certificate Secret, serve with
// node-control) and of the CA bundle.
func modeConfig(cfg Config, mode translate.Mode, gw *gwv1.Gateway, res translate.Result,
	env []corev1.EnvVar) ([]corev1.EnvVar, []corev1.VolumeMount, []corev1.Volume) {
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	vols := []corev1.Volume{
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(tmpSizeLimit, resource.BinarySI)}}},
	}
	switch {
	case mode == translate.ModeFunnel:
		env = append(env, corev1.EnvVar{Name: "FUNNEL_NS", Value: gw.Namespace},
			corev1.EnvVar{Name: "FUNNEL_API_URL", Value: cfg.APIURL})
	case res.CertSecret != "":
		env = append(
			env,
			corev1.EnvVar{Name: "FUNNEL_SERVE_CERT_FILE", Value: certMountPath + "/tls.crt"},
			corev1.EnvVar{Name: "FUNNEL_SERVE_KEY_FILE", Value: certMountPath + "/tls.key"},
		)
		vols = append(vols, corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: res.CertSecret}}})
		mounts = append(
			mounts,
			corev1.VolumeMount{Name: "tls", MountPath: certMountPath, ReadOnly: true},
		)
	default:
		env = append(env, corev1.EnvVar{Name: "FUNNEL_SERVE_URL", Value: cfg.ServeURL})
	}
	if cfg.CABundle != "" {
		env = append(env, corev1.EnvVar{Name: "SSL_CERT_FILE", Value: caMountPath + "/ca.crt"})
		vols = append(vols, corev1.Volume{Name: "ca", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cfg.CABundle},
			}}})
		mounts = append(
			mounts,
			corev1.VolumeMount{Name: "ca", MountPath: caMountPath, ReadOnly: true},
		)
	}
	return env, mounts, vols
}

func podSpec(
	image string,
	env []corev1.EnvVar,
	mounts []corev1.VolumeMount,
	vols []corev1.Volume,
) corev1.PodSpec {
	f := false
	t := true
	uid := int64(runAsUser)
	return corev1.PodSpec{
		AutomountServiceAccountToken: &f,
		EnableServiceLinks:           &f,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &t,
			RunAsUser:      &uid,
			RunAsGroup:     &uid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Volumes: vols,
		Containers: []corev1.Container{{
			Name: "funnel-sidecar", Image: image, Env: env, VolumeMounts: mounts,
			Ports: []corev1.ContainerPort{{Name: "readyz", ContainerPort: readyPortNum}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/readyz", Port: intstr.FromString("readyz")}},
				PeriodSeconds: readyPeriod,
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &f,
				ReadOnlyRootFilesystem:   &t,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
	}
}
