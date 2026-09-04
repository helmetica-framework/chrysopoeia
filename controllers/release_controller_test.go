package controllers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

func releaseControllerFor(group, version, kind string) *ReleaseController {
	return &ReleaseController{GVK: schema.GroupVersionKind{Group: group, Version: version, Kind: kind}}
}

// The instance namespace name is the identity of a deployed release: changing the scheme strands
// every namespace already in a cluster. These values are pinned so that has to be a deliberate edit
// here rather than a side effect of touching the hash inputs.
func TestInstanceNamespaceName(t *testing.T) {
	nsn := types.NamespacedName{Namespace: "default", Name: "foo"}

	// A short kind leaves the identity digest at its full 32 characters.
	assert.Equal(t, "helx-mariadb-978f7249-170427d918635ae5444ee085c2d84f60",
		releaseControllerFor("helmetica.io", "v1", "MariaDB").instanceNamespaceName(nsn))
	// A long one takes the room from the digest instead, up to the 63 character limit.
	assert.Equal(t, "helx-postgresqlinstanceclaim-cf6572d6-1fd3b1526b81f91060b8efe55",
		releaseControllerFor("helmetica.io", "v1", "PostgreSQLInstanceClaim").instanceNamespaceName(nsn))
}

func TestInstanceNamespaceName_Stable(t *testing.T) {
	r := releaseControllerFor("helmetica.io", "v1", "MariaDB")
	nsn := types.NamespacedName{Namespace: "default", Name: "foo"}

	assert.Equal(t, r.instanceNamespaceName(nsn), r.instanceNamespaceName(nsn))
	assert.Equal(t, r.instanceNamespaceName(nsn),
		releaseControllerFor("helmetica.io", "v1", "MariaDB").instanceNamespaceName(nsn))
}

// Every field is joined with a space before hashing because a space cannot occur in any of them. A
// separator that can, or none at all, lets two distinct claims share an instance namespace, where
// their releases overwrite each other and deleting one takes down the other.
func TestInstanceNamespaceName_SeparatesHashedFields(t *testing.T) {
	base := releaseControllerFor("helmetica.io", "v1", "MariaDB")
	baseNSN := types.NamespacedName{Namespace: "team", Name: "prod-db"}

	for _, tc := range []struct {
		desc string
		r    *ReleaseController
		nsn  types.NamespacedName
	}{
		{
			"the namespace/name boundary moved",
			base,
			types.NamespacedName{Namespace: "team-prod", Name: "db"},
		},
		{
			"the namespace/name boundary moved the other way",
			base,
			types.NamespacedName{Namespace: "team-prod-db", Name: ""},
		},
		{
			"the group/version boundary moved",
			releaseControllerFor("helmetica.i", "ov1", "MariaDB"), baseNSN,
		},
		{
			"the version/kind boundary moved",
			releaseControllerFor("helmetica.io", "v", "1MariaDB"), baseNSN,
		},
		{
			"a different namespace",
			base,
			types.NamespacedName{Namespace: "other", Name: "prod-db"},
		},
		{
			"a different name",
			base,
			types.NamespacedName{Namespace: "team", Name: "other"},
		},
		{
			"a different group",
			releaseControllerFor("example.com", "v1", "MariaDB"), baseNSN,
		},
		{
			"a different version",
			releaseControllerFor("helmetica.io", "v2", "MariaDB"), baseNSN,
		},
		{
			"a different kind",
			releaseControllerFor("helmetica.io", "v1", "OpenBao"), baseNSN,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			assert.NotEqual(t, base.instanceNamespaceName(baseNSN), tc.r.instanceNamespaceName(tc.nsn))
		})
	}
}

// The kind and the GVK digest are there to be read: they let a reader recognise an instance and let
// the namespaces of one GVK be listed by a prefix computed from the GVK alone.
func TestInstanceNamespaceName_SharesAPrefixPerGVK(t *testing.T) {
	r := releaseControllerFor("helmetica.io", "v1", "MariaDB")

	first := r.instanceNamespaceName(types.NamespacedName{Namespace: "default", Name: "foo"})
	second := r.instanceNamespaceName(types.NamespacedName{Namespace: "other", Name: "bar"})

	prefix := strings.Join(strings.Split(first, "-")[:3], "-")
	assert.True(t, strings.HasPrefix(prefix, "helx-mariadb-"), "the kind is readable in %q", first)
	assert.True(t, strings.HasPrefix(second, prefix+"-"), "%q and %q share the GVK prefix", first, second)
	assert.NotEqual(t, first, second, "but the identity digest still separates them")
}

// A namespace name is a DNS-1123 label, and it is also used as the value of the
// chrysopoeia.io/instance-namespace label on an OperatorHarness.
func TestInstanceNamespaceName_FitsKubernetesLimits(t *testing.T) {
	for _, kindLen := range []int{1, 7, 17, 18, 23, 32, 33, 34, 40, 63} {
		kind := "K" + strings.Repeat("a", kindLen-1)

		t.Run(fmt.Sprintf("kind of %d characters", kindLen), func(t *testing.T) {
			name := releaseControllerFor("helmetica.io", "v1", kind).
				instanceNamespaceName(types.NamespacedName{Namespace: "default", Name: "foo"})

			assert.Empty(t, validation.IsDNS1123Label(name), "%q is not a usable namespace name", name)
			assert.Empty(t, validation.IsValidLabelValue(name), "%q is not a usable label value", name)

			digest := name[strings.LastIndex(name, "-")+1:]
			assert.GreaterOrEqual(t, len(digest), 16,
				"%q keeps at least 64 bits of identity even when the kind crowds it out", name)
		})
	}
}

// Past 32 characters the kind is cut, so two kinds can share the readable part. The identity digest
// takes the full kind, so they still get separate namespaces.
func TestInstanceNamespaceName_SeparatesKindsThatShareATruncation(t *testing.T) {
	prefix := strings.Repeat("A", 32)
	nsn := types.NamespacedName{Namespace: "default", Name: "foo"}

	first := releaseControllerFor("helmetica.io", "v1", prefix+"One").instanceNamespaceName(nsn)
	second := releaseControllerFor("helmetica.io", "v1", prefix+"Two").instanceNamespaceName(nsn)

	assert.NotEqual(t, first, second)
}

func TestExtractDependencies(t *testing.T) {
	instance := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"requires": []any{
				map[string]any{"dependencyGroup": map[string]any{"name": "mariadb"}},
				map[string]any{"dependencyGroup": map[string]any{"name": "strimzi", "as": "strimzi-test"}},
			},
			"provides": []any{
				map[string]any{"dependencyGroup": map[string]any{"name": "mariadb"}},
			},
		},
	}}

	requires := extractRequires(instance)
	assert.Equal(t, []chrysopoeiav1.DependencyGroupReference{
		{Name: "mariadb"},
		{Name: "strimzi", As: "strimzi-test"},
	}, requires)

	assert.Equal(t, "mariadb", requires[0].ScopeName(), "without an alias the group name names the scope")
	assert.Equal(t, "strimzi-test", requires[1].ScopeName(), "the alias names the scope")

	assert.Equal(t, []chrysopoeiav1.DependencyGroupReference{{Name: "mariadb"}}, extractProvides(instance))
	assert.Empty(t, extractManages(instance), "this instance does not deploy an operator")
}

func TestExtractDependencies_Absent(t *testing.T) {
	assert.Nil(t, extractRequires(unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}))
	assert.Nil(t, extractRequires(unstructured.Unstructured{Object: map[string]any{}}))
}

func TestExtractDependencies_SkipsUnusableEntries(t *testing.T) {
	// The instance schema allows only dependencyGroup references today, but the field is read from
	// unstructured data: anything unusable has to be skipped rather than turned into an empty scope.
	instance := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"requires": []any{
				"mariadbs.k8s.mariadb.com",
				map[string]any{"name": "mariadbs.k8s.mariadb.com"},
				map[string]any{"dependencyGroup": map[string]any{"name": ""}},
				map[string]any{"dependencyGroup": map[string]any{}},
				map[string]any{"someFutureRef": map[string]any{"name": "mariadb"}},
				map[string]any{"dependencyGroup": map[string]any{"name": "mariadb"}},
			},
		},
	}}

	assert.Equal(t, []chrysopoeiav1.DependencyGroupReference{{Name: "mariadb"}}, extractRequires(instance))
}
