package controllers

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSetClaimCondition_OnAClaimWithoutStatus(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{}}
	claim.SetGeneration(7)

	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type:    valuesResolvedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  "ValuesPreprocessingFailed",
		Message: "server.computed: division by zero",
	}))

	conditions, found, err := unstructured.NestedSlice(claim.Object, "status", "conditions")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, conditions, 1)

	cond := conditions[0].(map[string]any)
	assert.Equal(t, valuesResolvedCondition, cond["type"])
	assert.Equal(t, "False", cond["status"])
	assert.Equal(t, "ValuesPreprocessingFailed", cond["reason"])
	assert.Equal(t, "server.computed: division by zero", cond["message"])
	assert.EqualValues(t, 7, cond["observedGeneration"], "the condition records the generation it was set from")
	assert.NotEmpty(t, cond["lastTransitionTime"], "the API server requires it")
}

func TestSetClaimCondition_ReplacesTheSameType(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{}}
	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionFalse,
		Reason: "ValuesPreprocessingFailed", Message: "boom",
	}))
	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionTrue,
		Reason: "ValuesResolved",
	}))

	conditions, _, err := unstructured.NestedSlice(claim.Object, "status", "conditions")
	require.NoError(t, err)
	require.Len(t, conditions, 1, "a condition type appears at most once, the CRD list-map key enforces it")
	assert.Equal(t, "True", conditions[0].(map[string]any)["status"])
	assert.Equal(t, "ValuesResolved", conditions[0].(map[string]any)["reason"])
}

func TestSetClaimCondition_KeepsOtherTypes(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{}}
	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: "SomeOtherCondition", Status: metav1.ConditionTrue, Reason: "Fine",
	}))
	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionFalse, Reason: "ValuesPreprocessingFailed",
	}))

	conditions, _, err := unstructured.NestedSlice(claim.Object, "status", "conditions")
	require.NoError(t, err)
	assert.Len(t, conditions, 2)
}

func TestSetClaimCondition_KeepsTheRestOfTheStatus(t *testing.T) {
	// The release controller owns these fields. Writing a condition must not drop them.
	claim := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"releaseStatus":   "Ready",
			"appliedRevision": "prod-abc123",
		},
	}}

	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionTrue, Reason: "ValuesResolved",
	}))

	status, _, err := unstructured.NestedMap(claim.Object, "status")
	require.NoError(t, err)
	assert.Equal(t, "Ready", status["releaseStatus"])
	assert.Equal(t, "prod-abc123", status["appliedRevision"])
	assert.Contains(t, status, "conditions")
}

func TestSetClaimCondition_IsWritableBackToUnstructured(t *testing.T) {
	// The result is handed to r.Status().Update, which deep-copies the object. A value the
	// unstructured converter cannot handle panics there rather than here.
	claim := &unstructured.Unstructured{Object: map[string]any{}}
	claim.SetGeneration(3)
	require.NoError(t, setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionTrue, Reason: "ValuesResolved",
	}))

	assert.NotPanics(t, func() { _ = claim.DeepCopy() })
}

func TestSetClaimCondition_RejectsAMalformedConditionList(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"conditions": "not a list"},
	}}
	err := setClaimCondition(claim, metav1.Condition{
		Type: valuesResolvedCondition, Status: metav1.ConditionTrue, Reason: "ValuesResolved",
	})
	require.Error(t, err)
}

// versionClaim is a claim carrying a spec.version and a status.version. nil
// leaves a field out entirely; a non-string value builds the malformed cases.
func versionClaim(t *testing.T, spec, status any) unstructured.Unstructured {
	t.Helper()

	var claim unstructured.Unstructured
	claim.SetAPIVersion("v1.juiceshop.helmetica-bundles.io/bundle")
	claim.SetKind("Juiceshop")
	claim.SetNamespace("default")
	claim.SetName("my-instance")

	if spec != nil {
		require.NoError(t, unstructured.SetNestedField(claim.Object, spec, "spec", "version"))
	}
	if status != nil {
		require.NoError(t, unstructured.SetNestedField(claim.Object, status, "status", "version"))
	}
	return claim
}

func juiceshopGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "v1.juiceshop.helmetica-bundles.io",
		Version: "bundle",
		Kind:    "Juiceshop",
	}
}

// claimCRD is a generated claim CRD carrying the spec.version enum discovery
// writes, newest first.
func claimCRD(gvk schema.GroupVersionKind, versions ...string) apiextv1.CustomResourceDefinition {
	enum := make([]apiextv1.JSON, len(versions))
	for i, v := range versions {
		enum[i] = apiextv1.JSON{Raw: []byte(strconv.Quote(v))}
	}

	return apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: strings.ToLower(gvk.Kind) + "s." + gvk.Group},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: gvk.Group,
			Names: apiextv1.CustomResourceDefinitionNames{Kind: gvk.Kind},
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name: gvk.Version,
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextv1.JSONSchemaProps{
									"version": {Type: "string", Enum: enum},
								},
							},
						},
					},
				},
			}},
		},
	}
}

// revisionManagerWithCRDs is a RevisionManager backed by a client that indexes CRDs the way the
// manager does, so a lookup by group and kind resolves.
func revisionManagerWithCRDs(t *testing.T, crds ...apiextv1.CustomResourceDefinition) *RevisionManager {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, apiextv1.AddToScheme(scheme))

	objs := make([]client.Object, len(crds))
	for i := range crds {
		objs[i] = &crds[i]
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&apiextv1.CustomResourceDefinition{}, crdGroupKindField, indexCRDByGroupKind).
		WithObjects(objs...).
		Build()

	return &RevisionManager{Client: c}
}

func TestNewestAllowedVersion_TakesTheFirstEnumEntry(t *testing.T) {
	// Discovery sorts descending, so the first entry is the newest and nothing
	// here sorts anything.
	other := claimCRD(schema.GroupVersionKind{Group: "other.io", Version: "bundle", Kind: "Other"}, "9.9.9")
	r := revisionManagerWithCRDs(t, other, claimCRD(juiceshopGVK(), "2.1.0", "2.0.0"))

	got, err := r.newestAllowedVersion(t.Context(), juiceshopGVK())

	require.NoError(t, err)
	assert.Equal(t, "2.1.0", got, "and unquoted: the enum entries are raw JSON")
}

func TestNewestAllowedVersion_PicksTheCRDOfTheClaimsOwnKind(t *testing.T) {
	// Same kind in another group, and same group under another kind: neither
	// may answer for the claim.
	sameKind := claimCRD(schema.GroupVersionKind{Group: "other.io", Version: "bundle", Kind: "Juiceshop"}, "9.9.9")
	sameGroup := claimCRD(schema.GroupVersionKind{Group: juiceshopGVK().Group, Version: "bundle", Kind: "Other"}, "8.8.8")
	r := revisionManagerWithCRDs(t, sameKind, sameGroup, claimCRD(juiceshopGVK(), "2.1.0"))

	got, err := r.newestAllowedVersion(t.Context(), juiceshopGVK())

	require.NoError(t, err)
	assert.Equal(t, "2.1.0", got)
}

func TestNewestAllowedVersion_NoCRDForTheKindIsAnError(t *testing.T) {
	r := revisionManagerWithCRDs(t)

	got, err := r.newestAllowedVersion(t.Context(), juiceshopGVK())

	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "Juiceshop")
}

func TestNewestVersion_AnotherServedVersionIsNotUsed(t *testing.T) {
	// A claim served at bundle must not be answered from some other version's
	// schema, which may carry a different enum.
	crd := claimCRD(juiceshopGVK(), "2.1.0")
	crd.Spec.Versions[0].Name = "bundlev2"

	_, err := newestVersion(crd, juiceshopGVK())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle")
}

func TestNewestVersion_AnEmptyEnumIsAnError(t *testing.T) {
	// Discovery found nothing. Returning "" would build a revision against
	// whatever tag the registry resolves by default.
	_, err := newestVersion(claimCRD(juiceshopGVK()), juiceshopGVK())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Juiceshop")
}

func TestResolveVersion_PinnedWins(t *testing.T) {
	// A user who pinned a version keeps it, whatever the framework selected
	// earlier and whatever is running.
	got, err := resolveVersion(versionClaim(t, "1.0.0", "2.0.0"), "1.5.0")

	require.NoError(t, err)
	assert.Equal(t, "1.0.0", got)
}

func TestResolveVersion_SelectedWinsOverWhatIsRunning(t *testing.T) {
	// A bump adept wrote is the one thing that moves a managed instance, so it
	// has to beat the version already running.
	got, err := resolveVersion(versionClaim(t, nil, "2.0.0"), "1.5.0")

	require.NoError(t, err)
	assert.Equal(t, "2.0.0", got)
}

func TestResolveVersion_WhatIsRunningDecidesWhenNothingElseDoes(t *testing.T) {
	// The stickiness case, and the reason current exists. This controller
	// reconciles on any claim edit, so without it an unrelated change to
	// spec.values would rebuild the instance on the newest version going.
	got, err := resolveVersion(versionClaim(t, nil, nil), "2.0.0")

	require.NoError(t, err)
	assert.Equal(t, "2.0.0", got)
}

func TestResolveVersion_NothingDecidesIt(t *testing.T) {
	// A claim with no revision yet. The caller reaches for the newest version
	// the schema allows only in this case, so a kind without version discovery
	// still reconciles as long as something here decides.
	got, err := resolveVersion(versionClaim(t, nil, nil), "")

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveVersion_EmptyStringsAreAbsent(t *testing.T) {
	// An empty spec.version is the marker for a managed instance, so present
	// but empty has to read the same as absent in both fields.
	got, err := resolveVersion(versionClaim(t, "", ""), "")

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveVersion_AMalformedPinIsAnError(t *testing.T) {
	_, err := resolveVersion(versionClaim(t, int64(3), nil), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestResolveVersion_AMalformedSelectionIsAnError(t *testing.T) {
	// Falling through this would move the instance on the strength of a field
	// nobody could read.
	_, err := resolveVersion(versionClaim(t, nil, int64(3)), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestVersionWithoutDigest(t *testing.T) {
	// The other half of the Cut release_controller.go:102 makes: that one wants
	// the digest, this one wants the version.
	assert.Equal(t, "2.1.0", versionWithoutDigest("2.1.0@sha256:abc123"))
	assert.Equal(t, "2.1.0", versionWithoutDigest("2.1.0"), "a revision without a digest")
	assert.Empty(t, versionWithoutDigest(""), "a claim with no revision yet")
}
