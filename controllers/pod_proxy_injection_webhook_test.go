package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

var testProxy = ProxyInjection{
	ServiceHost: "chrysopoeia-proxy.chrysopoeia-proxy-system.svc",
	ServicePort: "8443",
}

func TestInjectProxyConfiguration(t *testing.T) {
	// A pod as the in-tree service account admission plugin leaves it, before this webhook runs.
	spec := corev1.PodSpec{
		ServiceAccountName: "operator-mariadb-operator",
		InitContainers: []corev1.Container{{
			Name: "init",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "kube-api-access-abcde", MountPath: serviceAccountMountPath, ReadOnly: true},
			},
		}},
		Containers: []corev1.Container{{
			Name: "manager",
			Env: []corev1.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "10.96.0.1"},
				{Name: "WATCH_NAMESPACE", Value: ""},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: "/etc/config"},
				{Name: "kube-api-access-abcde", MountPath: serviceAccountMountPath, ReadOnly: true},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "config"},
			{Name: "kube-api-access-abcde", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{},
			}},
		},
	}

	injectProxyConfiguration(&spec, testProxy)

	assert.Equal(t, ptr.To(false), spec.AutomountServiceAccountToken,
		"Kubernetes must not project its own service account volume")
	assert.Equal(t, "operator-mariadb-operator", spec.ServiceAccountName,
		"the token still authenticates the operator, the service account must be kept")

	assert.Equal(t, []corev1.Volume{{Name: "config"}, proxyAPIAccessVolume()}, spec.Volumes,
		"the service account volume must be replaced, unrelated volumes kept")

	proxyMount := corev1.VolumeMount{
		Name: proxyAPIAccessVolumeName, MountPath: serviceAccountMountPath, ReadOnly: true,
	}
	assert.Equal(t, []corev1.VolumeMount{proxyMount}, spec.InitContainers[0].VolumeMounts)
	assert.Equal(t, []corev1.VolumeMount{{Name: "config", MountPath: "/etc/config"}, proxyMount},
		spec.Containers[0].VolumeMounts)

	for _, container := range allContainers(&spec) {
		assert.Contains(t, container.Env,
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: testProxy.ServiceHost},
			"container %q must be pointed at the proxy", container.Name)
		assert.Contains(t, container.Env,
			corev1.EnvVar{Name: "KUBERNETES_SERVICE_PORT", Value: testProxy.ServicePort},
			"container %q must be pointed at the proxy", container.Name)
	}
	assert.Contains(t, spec.Containers[0].Env, corev1.EnvVar{Name: "WATCH_NAMESPACE"},
		"unrelated environment variables must be kept")
	assert.Len(t, spec.Containers[0].Env, 3, "the API server host must be overridden, not appended")
}

func TestInjectProxyConfiguration_Idempotent(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "manager",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "kube-api-access-abcde", MountPath: serviceAccountMountPath, ReadOnly: true},
			},
		}},
		Volumes: []corev1.Volume{{Name: "kube-api-access-abcde"}},
	}

	injectProxyConfiguration(&spec, testProxy)
	injected := *spec.DeepCopy()

	injectProxyConfiguration(&spec, testProxy)

	assert.Equal(t, injected, spec, "injecting twice must not change the pod any further")
}

func TestInjectProxyConfiguration_KeepsVolumeMountedElsewhere(t *testing.T) {
	// Pathological, but dropping the volume while a mount still references it would make the pod invalid.
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "manager",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "shared", MountPath: serviceAccountMountPath},
				{Name: "shared", MountPath: "/elsewhere"},
			},
		}},
		Volumes: []corev1.Volume{{Name: "shared"}},
	}

	injectProxyConfiguration(&spec, testProxy)

	require.Len(t, spec.Volumes, 2)
	assert.Equal(t, "shared", spec.Volumes[0].Name)
	assert.Equal(t, []corev1.VolumeMount{
		{Name: "shared", MountPath: "/elsewhere"},
		{Name: proxyAPIAccessVolumeName, MountPath: serviceAccountMountPath, ReadOnly: true},
	}, spec.Containers[0].VolumeMounts)
}

func TestMatchingHarness(t *testing.T) {
	harnesses := []chrysopoeiav1.OperatorHarness{
		harness("mariadb-operator", "mariadb-operator", true, "operator-mariadb-operator"),
		harness("k8up", "k8up-system", true, "k8up-operator", "default"),
		harness("disabled", "disabled-operator", false, "disabled-operator"),
	}

	for _, tc := range []struct {
		name           string
		namespace      string
		serviceAccount string
		want           string
	}{
		{"matches namespace and service account", "mariadb-operator", "operator-mariadb-operator", "mariadb-operator"},
		{"matches any of the service accounts", "k8up-system", "default", "k8up"},
		{"other service account in a harnessed namespace", "mariadb-operator", "some-job", ""},
		{"harnessed service account in another namespace", "other", "operator-mariadb-operator", ""},
		{"proxy injection disabled", "disabled-operator", "disabled-operator", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match := matchingHarness(harnesses, tc.namespace, tc.serviceAccount)
			if tc.want == "" {
				assert.Nil(t, match)
				return
			}
			require.NotNil(t, match)
			assert.Equal(t, tc.want, match.Name)
		})
	}
}

func harness(name, namespace string, injectProxyConfiguration bool, serviceAccounts ...string) chrysopoeiav1.OperatorHarness {
	return chrysopoeiav1.OperatorHarness{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: chrysopoeiav1.OperatorHarnessSpec{
			ScopeToLabel: RequiresLabelPrefix + name,
			Operator: chrysopoeiav1.OperatorHarnessOperator{
				Namespace:                namespace,
				ServiceAccounts:          serviceAccounts,
				InjectProxyConfiguration: injectProxyConfiguration,
			},
		},
	}
}
