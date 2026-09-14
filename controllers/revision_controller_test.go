package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
