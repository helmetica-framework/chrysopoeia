package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

func TestConflictingProviders(t *testing.T) {
	// The CRDs of the mariadb group ship in a chart of their own, the operator only manages them.
	crds := crdSource(t, "v26.mariadb-crds", "2026-08-03T10:00:00Z", "mariadb")
	operator := crdSource(t, "v26.mariadb-operator", "2026-08-03T11:00:00Z")
	operator.Spec.Manages = []chrysopoeiav1.DependencyReference{
		{DependencyGroup: &chrysopoeiav1.DependencyGroupReference{Name: "mariadb"}},
	}
	// A second chart claiming to ship the same CRDs would install them over the first one.
	duplicate := crdSource(t, "v27.mariadb-crds", "2026-08-03T12:00:00Z", "mariadb")

	sources := []chrysopoeiav1.CustomResourceDefinitionSource{crds, operator, duplicate}

	assert.Empty(t, conflictingProviders(crds, sources),
		"the older source keeps the group")
	assert.Empty(t, conflictingProviders(operator, sources),
		"managing a group is not providing it")
	assert.Equal(t,
		[]string{`DependencyGroup "mariadb" is already provided by CustomResourceDefinitionSource "v26.mariadb-crds"`},
		conflictingProviders(duplicate, sources))
}

func TestConflictingProviders_TieBreak(t *testing.T) {
	// Sources created in the same instant must not both be rejected, nor both accepted.
	same := "2026-08-03T10:00:00Z"
	alpha := crdSource(t, "alpha", same, "widgets")
	omega := crdSource(t, "omega", same, "widgets")

	sources := []chrysopoeiav1.CustomResourceDefinitionSource{omega, alpha}

	assert.Empty(t, conflictingProviders(alpha, sources), "the name breaks the tie")
	assert.Len(t, conflictingProviders(omega, sources), 1)
}

func TestConflictingProviders_IgnoresDeletedSources(t *testing.T) {
	old := crdSource(t, "v26.mariadb-crds", "2026-08-03T10:00:00Z", "mariadb")
	deleting := metav1.NewTime(mustParseTime(t, "2026-08-03T12:00:00Z"))
	old.DeletionTimestamp = &deleting

	successor := crdSource(t, "v27.mariadb-crds", "2026-08-03T11:00:00Z", "mariadb")

	assert.Empty(t, conflictingProviders(successor, []chrysopoeiav1.CustomResourceDefinitionSource{old, successor}),
		"a source on its way out must not keep blocking its successor")
}

func crdSource(t *testing.T, name, created string, provides ...string) chrysopoeiav1.CustomResourceDefinitionSource {
	t.Helper()

	provided := make([]chrysopoeiav1.DependencyReference, len(provides))
	for i, group := range provides {
		provided[i] = chrysopoeiav1.DependencyReference{
			DependencyGroup: &chrysopoeiav1.DependencyGroupReference{Name: group},
		}
	}

	return chrysopoeiav1.CustomResourceDefinitionSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, created)),
		},
		Spec: chrysopoeiav1.CustomResourceDefinitionSourceSpec{Provides: provided},
	}
}
