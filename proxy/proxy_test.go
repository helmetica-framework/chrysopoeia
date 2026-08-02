package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/helmetica-framework/chrysopoeia/proxy"
	"github.com/helmetica-framework/chrysopoeia/testutil"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

const testScope = "scope.test/a"

func Test_Proxy(t *testing.T) {
	scheme, adminRestCfg := testutil.SetupEnvtestEnv(t)
	adminClientset, err := kubernetes.NewForConfig(adminRestCfg)
	require.NoError(t, err)

	proxySAName, proxyRestConfig, err := authenticatedServiceAccount(t, adminRestCfg, scheme)
	require.NoError(t, err)
	setupProxyRBAC(t, adminClientset, proxySAName)

	hcSAName, harnessedControllerRestConfig, err := authenticatedServiceAccount(t, adminRestCfg, scheme)
	require.NoError(t, err)
	setupHarnessedControllerRBAC(t, adminClientset, hcSAName)

	rp, err := proxy.New(log.IntoContext(t.Context(), testr.NewWithOptions(t, testr.Options{Verbosity: 100})), proxyRestConfig)
	require.NoError(t, err)

	proxy := httptest.NewServer(rp)
	t.Cleanup(proxy.Close)
	requireProxyReady(t, proxy)

	harnessedControllerRestConfig.Host = proxy.URL

	hcClientset, err := kubernetes.NewForConfig(harnessedControllerRestConfig)
	require.NoError(t, err)

	t.Run("namespaced access", func(t *testing.T) {
		_, err := hcClientset.CoreV1().ConfigMaps("default").List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)

		_, err = hcClientset.CoreV1().Secrets("default").List(t.Context(), metav1.ListOptions{})
		require.Error(t, err, "forbidden")
		_, err = hcClientset.CoreV1().ConfigMaps("other-ns").List(t.Context(), metav1.ListOptions{})
		require.Error(t, err, "forbidden")
	})

	t.Run("cluster list of namespaces scoped to testScope", func(t *testing.T) {
		nss, err := hcClientset.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, nss.Items, 0)

		_, err = adminClientset.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-namespace-",
				Labels: map[string]string{
					testScope:    "",
					"test-label": "test-value",
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		_, err = adminClientset.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-namespace-",
				Labels: map[string]string{
					"other-scope": "",
					"test-label":  "test-value",
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		nss, err = hcClientset.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, nss.Items, 1)
		nss, err = hcClientset.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{
			LabelSelector: "test-label=test-value",
		})
		require.NoError(t, err)
		require.Len(t, nss.Items, 1)
	})

	t.Run("unauthorized access to namespaces outside of testScope", func(t *testing.T) {
		c := rest.CopyConfig(harnessedControllerRestConfig)
		c.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return &injectScopeRoundTripper{inner: rt, scope: "other-scope"}
		})
		cs, err := kubernetes.NewForConfig(c)
		require.NoError(t, err)
		nss, err := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		require.True(t, apierrors.IsForbidden(err), "expected forbidden error, got: %v", err)
		require.ErrorContains(t, err, "is not allowed to list cluster-scoped resources with label other-scope")
		require.Len(t, nss.Items, 0)
	})

	t.Run("invalid scope label", func(t *testing.T) {
		c := rest.CopyConfig(harnessedControllerRestConfig)
		c.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return &injectScopeRoundTripper{inner: rt, scope: "!invalid-scope"}
		})
		cs, err := kubernetes.NewForConfig(c)
		require.NoError(t, err)
		nss, err := cs.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
		require.True(t, apierrors.IsInvalid(err), "expected invalid error, got: %v", err)
		require.Len(t, nss.Items, 0)
	})

	t.Run("self subject access review", func(t *testing.T) {
		t.Run("no rewrite needed: harnessed controller can list namespaced objects", func(t *testing.T) {
			ssar, err := hcClientset.AuthorizationV1().SelfSubjectAccessReviews().Create(t.Context(), &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: "default",
						Verb:      "get",
						Group:     "",
						Resource:  "configmaps",
					},
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Truef(t, ssar.Status.Allowed, "SelfSubjectAccessReview was not allowed: %s", ssar.Status.Reason)

			ssar, err = hcClientset.AuthorizationV1().SelfSubjectAccessReviews().Create(t.Context(), &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: "default",
						Verb:      "get",
						Group:     "",
						Resource:  "secrets",
					},
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Falsef(t, ssar.Status.Allowed, "SelfSubjectAccessReview was allowed: %s", ssar.Status.Reason)
		})

		t.Run("rewrite needed: cluster-scoped access", func(t *testing.T) {
			ssar, err := hcClientset.AuthorizationV1().SelfSubjectAccessReviews().Create(t.Context(), &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Verb:     "watch",
						Group:    "",
						Resource: "namespaces",
					},
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Truef(t, ssar.Status.Allowed, "SelfSubjectAccessReview was not allowed: %s", ssar.Status.Reason)

			ssar, err = hcClientset.AuthorizationV1().SelfSubjectAccessReviews().Create(t.Context(), &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Verb:     "watch",
						Group:    "",
						Resource: "configmaps",
					},
				},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Falsef(t, ssar.Status.Allowed, "SelfSubjectAccessReview was allowed: %s", ssar.Status.Reason)
		})
	})
}

type injectScopeRoundTripper struct {
	scope string
	inner http.RoundTripper
}

func (rt *injectScopeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(proxy.ScopeHeader, rt.scope)
	return rt.inner.RoundTrip(req)
}

func requireProxyReady(t *testing.T, proxy *httptest.Server) {
	t.Helper()

	require.Eventually(t, func() bool {
		resp, err := http.Get(proxy.URL + "/_readyz")
		if err != nil {
			t.Logf("Error checking proxy readiness: %v", err)
			return false
		}
		defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Logf("Proxy not ready, status code: %d", resp.StatusCode)
			return false
		}
		return true
	}, 5*time.Second, 100*time.Millisecond)
}

func namespaceNameFromServiceAccountUsername(t *testing.T, username string) types.NamespacedName {
	t.Helper()

	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		t.Fatalf("Invalid service account username: %s", username)
	}
	return types.NamespacedName{
		Namespace: parts[2],
		Name:      parts[3],
	}
}

func setupHarnessedControllerRBAC(t *testing.T, adminClientset *kubernetes.Clientset, harnessedControllerSAName string) {
	t.Helper()

	namespaceName := namespaceNameFromServiceAccountUsername(t, harnessedControllerSAName)
	_, err := adminClientset.CoreV1().ServiceAccounts(namespaceName.Namespace).Apply(
		t.Context(),
		corev1ac.ServiceAccount(namespaceName.Name, namespaceName.Namespace).
			WithAnnotations(map[string]string{
				proxy.ScopeAnnotation: testScope,
			}),
		metav1.ApplyOptions{
			FieldManager: "sa-scope-annotator",
		})
	require.NoError(t, err)

	role, err := adminClientset.RbacV1().Roles("default").Create(t.Context(), &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harnessed-controller-role",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harnessed-controller-role-binding",
			Namespace: "default",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: "User",
				Name: harnessedControllerSAName,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			Name:     role.Name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	_, err = adminClientset.RbacV1().RoleBindings("default").Create(t.Context(), roleBinding, metav1.CreateOptions{})
	require.NoError(t, err)

	cr, err := adminClientset.RbacV1().ClusterRoles().Create(t.Context(), &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "harnessed-controller-role-",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Resources:     []string{"namespaces"},
				Verbs:         []string{"scopedlist"},
				ResourceNames: []string{testScope, "!invalid-scope"},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = adminClientset.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harnessed-controller-role-binding",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: "User",
				Name: harnessedControllerSAName,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     cr.Name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func setupProxyRBAC(t *testing.T, adminClientset *kubernetes.Clientset, proxySAName string) {
	t.Helper()

	var proxyRole rbacv1.ClusterRole
	require.NoError(t, yaml.UnmarshalStrict(mustReadFile(t, "../config/proxy/role.yaml"), &proxyRole))
	_, err := adminClientset.RbacV1().ClusterRoles().Create(t.Context(), &proxyRole, metav1.CreateOptions{})
	require.NoError(t, err)
	proxyRoleBinding := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "proxy-role-binding",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: "User",
				Name: proxySAName,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     proxyRole.Name,
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	_, err = adminClientset.RbacV1().ClusterRoleBindings().Create(t.Context(), &proxyRoleBinding, metav1.CreateOptions{})
	require.NoError(t, err)
}

func authenticatedServiceAccount(t *testing.T, adminCfg *rest.Config, scheme *runtime.Scheme) (string, *rest.Config, error) {
	t.Helper()

	adminClient, err := client.NewWithWatch(adminCfg, client.Options{
		Scheme: scheme,
	})
	require.NoError(t, err)
	adminClientset, err := kubernetes.NewForConfig(adminCfg)
	require.NoError(t, err)
	ns := testutil.TmpNamespace(t, adminClient)
	token, err := adminClientset.CoreV1().ServiceAccounts(ns).CreateToken(t.Context(), "default", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("Token %s/%s expires at %s", ns, "default", token.Status.ExpirationTimestamp.Format(time.RFC3339))
	restCfg := rest.CopyConfig(adminCfg)
	stripMTLSAuthFromRestConfig(restCfg)
	restCfg.BearerToken = token.Status.Token
	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)
	ssr, err := clientset.AuthenticationV1().SelfSubjectReviews().Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	require.Truef(t, strings.HasPrefix(ssr.Status.UserInfo.Username, "system:serviceaccount:"), "expected to be authenticated as a service account, got: %s", ssr.Status.UserInfo.Username)

	return ssr.Status.UserInfo.Username, restCfg, err
}

func stripMTLSAuthFromRestConfig(cfg *rest.Config) {
	cfg.TLSClientConfig.CertData = nil
	cfg.TLSClientConfig.KeyData = nil
	cfg.TLSClientConfig.CertFile = ""
	cfg.TLSClientConfig.KeyFile = ""
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
