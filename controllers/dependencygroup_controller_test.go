package controllers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

func TestConflictingClaims(t *testing.T) {
	mariadb := dependencyGroup(t, "mariadb", "2026-08-03T10:00:00Z",
		"backups.k8s.mariadb.com", "databases.k8s.mariadb.com")
	// Claims a CRD of the older mariadb group, plus one of its own.
	mariadbFork := dependencyGroup(t, "mariadb-fork", "2026-08-03T11:00:00Z",
		"databases.k8s.mariadb.com", "grants.k8s.mariadb.com")
	strimzi := dependencyGroup(t, "strimzi", "2026-08-03T12:00:00Z",
		"kafkas.kafka.strimzi.io")

	groups := []chrysopoeiav1.DependencyGroup{mariadb, mariadbFork, strimzi}

	assert.Empty(t, conflictingClaims(mariadb, groups),
		"the older group keeps the claim on the disputed CRD")
	assert.Empty(t, conflictingClaims(strimzi, groups),
		"a group claiming only its own CRDs is undisputed")
	assert.Equal(t,
		[]string{`CRD "databases.k8s.mariadb.com" is already claimed by DependencyGroup "mariadb"`},
		conflictingClaims(mariadbFork, groups),
		"the younger group loses only the disputed CRD")
}

func TestConflictingClaims_TieBreak(t *testing.T) {
	// Groups created in the same instant must not both be rejected, nor both accepted.
	same := "2026-08-03T10:00:00Z"
	alpha := dependencyGroup(t, "alpha", same, "widgets.example.com")
	omega := dependencyGroup(t, "omega", same, "widgets.example.com")

	groups := []chrysopoeiav1.DependencyGroup{omega, alpha}

	assert.Empty(t, conflictingClaims(alpha, groups), "the name breaks the tie")
	assert.Len(t, conflictingClaims(omega, groups), 1)
}

func TestConflictingClaims_IgnoresDeletedGroups(t *testing.T) {
	mariadb := dependencyGroup(t, "mariadb", "2026-08-03T10:00:00Z", "databases.k8s.mariadb.com")
	deleting := metav1.NewTime(mustParseTime(t, "2026-08-03T12:00:00Z"))
	mariadb.DeletionTimestamp = &deleting

	successor := dependencyGroup(t, "mariadb-v2", "2026-08-03T11:00:00Z", "databases.k8s.mariadb.com")

	assert.Empty(t, conflictingClaims(successor, []chrysopoeiav1.DependencyGroup{mariadb, successor}),
		"a group on its way out must not keep blocking its successor")
}

func dependencyGroup(t *testing.T, name, created string, crds ...string) chrysopoeiav1.DependencyGroup {
	t.Helper()

	claimed := make([]chrysopoeiav1.DependencyGroupCRD, len(crds))
	for i, crd := range crds {
		claimed[i] = chrysopoeiav1.DependencyGroupCRD{Name: crd}
	}

	return chrysopoeiav1.DependencyGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(mustParseTime(t, created)),
		},
		Spec: chrysopoeiav1.DependencyGroupSpec{CRDs: claimed},
	}
}

func mustParseTime(t *testing.T, ts string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, ts)
	require.NoError(t, err)
	return parsed
}
