package claimdef_test

import (
	"testing"

	imagereflectorv1 "github.com/fluxcd/image-reflector-controller/api/v1"
	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
	"github.com/helmetica-framework/chrysopoeia/pkg/claimdef"
)

var testClaimGVK = schema.GroupVersionKind{
	Group:   "testing.io",
	Version: "v1",
	Kind:    "Instance",
}

func Test_FindDefinitionForClaim_NoOptionalObjs(t *testing.T) {
	c := fakeClient(t,
		&apiextv1.CustomResourceDefinition{
			Name: "instances.testing.io",
			Annotations: map[string]string{
				"chrysopoeia.io/source-ref-name":      "testing-instances",
				"chrysopoeia.io/source-ref-namespace": "controller",
			},
		},
		&chrysopoeiav1.ClaimDefinition{
			Name:      "testing-instances",
			Namespace: "controller",
			Spec: chrysopoeiav1.ClaimDefinitionSpec{
				Reference: chrysopoeiav1.SourceReference{
					Name: "testing-instances",
				},
			},
		},
		&sourcev1.OCIRepository{
			Name:      "testing-instances",
			Namespace: "controller",
		},
	)
	cs, err := claimdef.FindDefinitionForClaim(t.Context(), c, objWithGVK(testClaimGVK))
	require.NoError(t, err)
	assert.Equal(t, "testing-instances", cs.ClaimDefinition.Name)
	assert.Equal(t, "testing-instances", cs.OCIRepository.Name)
	assert.Nil(t, cs.OCIRepositoryCertSecret)
	assert.Nil(t, cs.ImageRepository)
}

func Test_FindDefinitionForClaim_OptionalObjs(t *testing.T) {
	c := fakeClient(t,
		&apiextv1.CustomResourceDefinition{
			Name: "instances.testing.io",
			Annotations: map[string]string{
				"chrysopoeia.io/source-ref-name":      "testing-instances",
				"chrysopoeia.io/source-ref-namespace": "controller",
			},
		},
		&chrysopoeiav1.ClaimDefinition{
			Name:      "testing-instances",
			Namespace: "controller",
			Spec: chrysopoeiav1.ClaimDefinitionSpec{
				Reference: chrysopoeiav1.SourceReference{
					Name: "testing-instances",
				},
				VersionDiscovery: chrysopoeiav1.VersionDiscovery{
					Reference: chrysopoeiav1.DiscoveryReference{
						Name: "testing-instances",
					},
				},
			},
		},
		&sourcev1.OCIRepository{
			Name:      "testing-instances",
			Namespace: "controller",
			Spec: sourcev1.OCIRepositorySpec{
				CertSecretRef: &fluxmeta.LocalObjectReference{
					Name: "testing-instances-cert",
				},
			},
		},
		&corev1.Secret{
			Name:      "testing-instances-cert",
			Namespace: "controller",
		},
		&imagereflectorv1.ImageRepository{
			Name:      "testing-instances",
			Namespace: "controller",
		},
	)
	cs, err := claimdef.FindDefinitionForClaim(t.Context(), c, objWithGVK(testClaimGVK))
	require.NoError(t, err)
	assert.Equal(t, "testing-instances", cs.ClaimDefinition.Name)
	assert.Equal(t, "testing-instances", cs.OCIRepository.Name)
	if assert.NotNil(t, cs.OCIRepositoryCertSecret) {
		assert.Equal(t, "testing-instances-cert", cs.OCIRepositoryCertSecret.Name)
	}
	if assert.NotNil(t, cs.ImageRepository) {
		assert.Equal(t, "testing-instances", cs.ImageRepository.Name)
	}
}

func objWithGVK(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := new(unstructured.Unstructured)
	u.SetGroupVersionKind(gvk)
	return u
}

func fakeClient(t *testing.T, initObjs ...client.Object) client.WithWatch {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiextv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, chrysopoeiav1.AddToScheme(scheme))
	require.NoError(t, imagereflectorv1.AddToScheme(scheme))
	require.NoError(t, sourcev1.AddToScheme(scheme))

	rm := meta.NewDefaultRESTMapper([]schema.GroupVersion{})
	rm.Add(testClaimGVK, meta.RESTScopeNamespace)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithRESTMapper(rm).
		Build()
}
