package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

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
