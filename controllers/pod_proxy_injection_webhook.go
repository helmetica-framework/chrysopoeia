package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

const (
	// ProxyInjectionWebhookPath is the path the pod proxy injection webhook is served on.
	ProxyInjectionWebhookPath = "/mutate--v1-pod"

	// proxyAPIAccessVolumeName is the name of the projected volume replacing the service account
	// volume Kubernetes mounts into every pod.
	proxyAPIAccessVolumeName = "chrysopoeia-proxy-api-access"
	// serviceAccountMountPath is where controller-runtime, and client-go in general, expect the
	// service account token and the API server's CA certificate.
	serviceAccountMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
	// serviceAccountTokenExpirationSeconds is the token lifetime requested for the projected token,
	// matching what kubelet requests for the automatically mounted one.
	serviceAccountTokenExpirationSeconds int64 = 3607

	defaultServiceAccountName = "default"
)

// PodProxyInjector points the pods of harnessed operators at the harness proxy instead of the
// Kubernetes API server. It is registered by the [OperatorHarnessManager] through a
// MutatingWebhookConfiguration per OperatorHarness, scoped to the operator's namespace.
type PodProxyInjector struct {
	client.Client
	Decoder admission.Decoder

	Proxy ProxyInjection
}

func (i *PodProxyInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	l := log.FromContext(ctx).WithName("PodProxyInjector.Handle")

	var pod corev1.Pod
	if err := i.Decoder.Decode(req, &pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// The pod's own namespace is not necessarily set yet on CREATE.
	namespace := req.Namespace
	serviceAccount := pod.Spec.ServiceAccountName
	if serviceAccount == "" {
		serviceAccount = defaultServiceAccountName
	}

	var harnesses chrysopoeiav1.OperatorHarnessList
	if err := i.List(ctx, &harnesses); err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to list OperatorHarnesses: %w", err))
	}

	harness := matchingHarness(harnesses.Items, namespace, serviceAccount)
	if harness == nil {
		return admission.Allowed("no OperatorHarness harnesses this pod")
	}

	injectProxyConfiguration(&pod.Spec, i.Proxy)

	patched, err := json.Marshal(&pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("unable to marshal the patched pod: %w", err))
	}

	l.Info("Injected the harness proxy configuration",
		"harness", harness.Name, "namespace", namespace, "serviceAccount", serviceAccount)

	return admission.PatchResponseFromRaw(req.Object.Raw, patched)
}

// matchingHarness returns the harness that harnesses pods running as serviceAccount in namespace, if any.
func matchingHarness(harnesses []chrysopoeiav1.OperatorHarness, namespace, serviceAccount string) *chrysopoeiav1.OperatorHarness {
	for i := range harnesses {
		harness := &harnesses[i]
		if !harness.Spec.Operator.InjectProxyConfiguration || harness.Spec.Operator.Namespace != namespace {
			continue
		}
		if slices.Contains(harness.Spec.Operator.ServiceAccounts, serviceAccount) {
			return harness
		}
	}
	return nil
}

// injectProxyConfiguration overrides the three parameters client-go uses to connect to the
// Kubernetes API server in-cluster: the API server's host and port, and the CA certificate to verify
// it with. The service account token is kept as-is, it authenticates the operator to the proxy just
// as it would to the API server.
//
// It is idempotent, so that a pod that was already patched, by an earlier reinvocation or by the
// harness having been applied twice, ends up with the same spec.
func injectProxyConfiguration(spec *corev1.PodSpec, proxy ProxyInjection) {
	// Kubernetes only skips projecting the service account token if this is false. The in-tree
	// service account admission plugin runs before this webhook though, so its volume is replaced
	// below instead of being prevented.
	spec.AutomountServiceAccountToken = ptr.To(false)

	replacedVolumes := make(map[string]bool)
	for _, container := range allContainers(spec) {
		container.VolumeMounts = slices.DeleteFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
			if m.MountPath != serviceAccountMountPath {
				return false
			}
			replacedVolumes[m.Name] = true
			return true
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      proxyAPIAccessVolumeName,
			MountPath: serviceAccountMountPath,
			ReadOnly:  true,
		})

		overrideEnv(container, corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: proxy.ServiceHost})
		overrideEnv(container, corev1.EnvVar{Name: "KUBERNETES_SERVICE_PORT", Value: proxy.ServicePort})
	}

	// Only drop the replaced volumes that nothing mounts anymore. A volume mounted at another path,
	// too, has to stay for the pod to remain valid.
	stillMounted := make(map[string]bool)
	for _, container := range allContainers(spec) {
		for _, m := range container.VolumeMounts {
			stillMounted[m.Name] = true
		}
	}
	spec.Volumes = slices.DeleteFunc(spec.Volumes, func(v corev1.Volume) bool {
		if v.Name == proxyAPIAccessVolumeName {
			return true
		}
		return replacedVolumes[v.Name] && !stillMounted[v.Name]
	})
	spec.Volumes = append(spec.Volumes, proxyAPIAccessVolume())
}

// proxyAPIAccessVolume mirrors the volume Kubernetes projects into every pod, with the API server's
// CA certificate swapped for the harness proxy's.
func proxyAPIAccessVolume() corev1.Volume {
	return corev1.Volume{
		Name: proxyAPIAccessVolumeName,
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To[int32](420),
				Sources: []corev1.VolumeProjection{
					{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							ExpirationSeconds: ptr.To(serviceAccountTokenExpirationSeconds),
							Path:              "token",
						},
					},
					{
						ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: ProxyCAConfigMapName},
							Items:                []corev1.KeyToPath{{Key: ProxyCACertKey, Path: "ca.crt"}},
						},
					},
					{
						DownwardAPI: &corev1.DownwardAPIProjection{
							Items: []corev1.DownwardAPIVolumeFile{{
								Path:     "namespace",
								FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
							}},
						},
					},
				},
			},
		},
	}
}

// allContainers returns pointers to every container of the pod that talks to the API server.
func allContainers(spec *corev1.PodSpec) []*corev1.Container {
	containers := make([]*corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	for i := range spec.InitContainers {
		containers = append(containers, &spec.InitContainers[i])
	}
	for i := range spec.Containers {
		containers = append(containers, &spec.Containers[i])
	}
	return containers
}

// overrideEnv sets env on the container, replacing an environment variable of the same name if the
// container already declares one, and appending it otherwise. Kubernetes injects the API server's
// host and port into every container, so an existing variable usually has to be overridden.
func overrideEnv(container *corev1.Container, env corev1.EnvVar) {
	if i := slices.IndexFunc(container.Env, func(e corev1.EnvVar) bool { return e.Name == env.Name }); i >= 0 {
		container.Env[i] = env
		return
	}
	container.Env = append(container.Env, env)
}
