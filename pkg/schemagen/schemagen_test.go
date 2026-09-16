package schemagen_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
	"github.com/helmetica-framework/chrysopoeia/testutil"
)

func TestGenerateCRD_Golden(t *testing.T) {
	scheme, restCfg := testutil.SetupEnvtestEnv(t)
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
	cli = client.WithFieldValidation(cli, client.FieldValidation("Strict"))
	require.NoError(t, err, "Creating controller-runtime client failed")

	charts, err := fs.Glob(os.DirFS("testdata/charts"), "*/values.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, charts, "No test charts found in testdata/charts")
	for i, chartPath := range charts {
		charts[i] = strings.TrimSuffix(chartPath, "/values.yaml")
	}
	t.Log("Found test charts:", charts)

	for _, chartPath := range charts {
		t.Run(chartPath, func(t *testing.T) {
			chartOut := "./testdata/charts/" + chartPath + ".golden.yaml"
			t.Run("Generate CRD", func(t *testing.T) {
				chartLoader, err := loader.Loader("./testdata/charts/" + chartPath)
				require.NoError(t, err, "Creating chart loader failed for chart: %s", chartPath)
				chart, err := chartLoader.Load()
				require.NoError(t, err, "Loading chart failed for chart: %s", chartPath)

				crd, err := schemagen.GenerateCRD(*chart)
				require.NoError(t, err, "GenerateCRD failed for chart: %s", chartPath)

				yamlData, err := kubeyaml.Marshal(crd)
				require.NoError(t, err, "Failed to marshal CRD to YAML for chart: %s", chartPath)

				require.NoError(t, os.WriteFile(chartOut, yamlData, 0644))
			})
			t.Run("Apply CRD to Cluster", func(t *testing.T) {
				var crd apiextv1.CustomResourceDefinition
				yamlData, err := os.ReadFile(chartOut)
				require.NoError(t, err, "Failed to read golden CRD file for chart: %s", chartPath)
				require.NoError(t, kubeyaml.UnmarshalStrict(yamlData, &crd), "Failed to unmarshal golden CRD YAML for chart: %s", chartPath)

				require.NoError(t, cli.Create(t.Context(), &crd), "Failed to apply golden CRD for chart: %s", chartPath)
				defer func() {
					require.NoError(t, cli.Delete(t.Context(), &crd), "Failed to delete golden CRD for chart: %s", chartPath)
				}()
				require.Eventually(t, func() bool {
					var tmpCRD apiextv1.CustomResourceDefinition
					if err := cli.Get(t.Context(), client.ObjectKey{Name: crd.Name}, &tmpCRD); err != nil {
						t.Logf("Failed to get CRD %s: %s", crd.Name, err.Error())
						return false
					}
					if tmpCRD.Status.AcceptedNames.ListKind == "" || len(tmpCRD.Status.StoredVersions) == 0 {
						return false
					}
					var tryList unstructured.UnstructuredList
					tryList.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   crd.Spec.Group,
						Version: tmpCRD.Status.StoredVersions[0],
						Kind:    tmpCRD.Status.AcceptedNames.ListKind,
					})
					if err := cli.List(t.Context(), &tryList); err != nil {
						t.Logf("Failed to list CRD %s: %s", crd.Name, err.Error())
						return false
					}
					return true
				}, 3*time.Second, 10*time.Millisecond)

				applytests, err := fs.Glob(os.DirFS(filepath.Join("testdata/charts", chartPath)), "examples/*.yaml")
				require.NoError(t, err)
				for _, applytest := range applytests {
					t.Run("Test examples "+applytest, func(t *testing.T) {
						applytestPath := filepath.Join("testdata/charts", chartPath, applytest)
						yamlData, err := os.ReadFile(applytestPath)
						require.NoError(t, err, "Failed to read apply test YAML for chart: %s", chartPath)

						var obj unstructured.Unstructured
						require.NoError(t, kubeyaml.UnmarshalStrict(yamlData, &obj), "Failed to unmarshal apply test YAML for chart: %s", chartPath)
						obj.SetNamespace("default")
						matchError := obj.GetAnnotations()["match-error"]
						createErr := cli.Create(t.Context(), &obj)

						if matchError != "" {
							require.ErrorContains(t, createErr, matchError)
						} else {
							require.NoError(t, createErr)
						}
					})
				}
			})
		})
	}
}

func TestGenerateCRD_StatusConditions(t *testing.T) {
	chartLoader, err := loader.Loader("./testdata/charts/celwrapper")
	require.NoError(t, err)
	chart, err := chartLoader.Load()
	require.NoError(t, err)

	crd, err := schemagen.GenerateCRD(*chart)
	require.NoError(t, err)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	values := schema.Properties["spec"].Properties["values"]
	assert.NotContains(t, values.Properties, "podinfo",
		"every field under podinfo is computed, so the key is not settable")
	assert.Contains(t, values.Properties, "ingressHostname")
	assert.Contains(t, values.Properties, "replicas")

	conditions, ok := schema.Properties["status"].Properties["conditions"]
	require.True(t, ok, "the claim status carries conditions")
	assert.Equal(t, "array", conditions.Type)
	require.NotNil(t, conditions.XListType, "a condition type appears at most once, which the API server enforces")
	assert.Equal(t, "map", *conditions.XListType)
	assert.Equal(t, []string{"type"}, conditions.XListMapKeys)

	require.NotNil(t, conditions.Items)
	require.NotNil(t, conditions.Items.Schema)
	item := *conditions.Items.Schema
	assert.Equal(t, "object", item.Type)
	for _, field := range []string{"type", "status", "reason", "message", "lastTransitionTime", "observedGeneration"} {
		assert.Contains(t, item.Properties, field, "metav1.Condition has a %s field", field)
	}
	assert.Equal(t, "date-time", item.Properties["lastTransitionTime"].Format)
	assert.Equal(t, "integer", item.Properties["observedGeneration"].Type)
	assert.Equal(t, "int64", item.Properties["observedGeneration"].Format)
	assert.ElementsMatch(t,
		[]string{"type", "status", "reason", "message", "lastTransitionTime"}, item.Required,
		"everything metav1.Condition marks required, and only that")
}

// TestGeneratedCRD_KeepsAVersionWrittenToStatus covers what the golden files
// cannot: a property missing from the schema is pruned by the API server on
// write, with no error for the writer to see. The client here is deliberately
// not the strict one used above, so this reproduces what a controller writing
// the field actually experiences.
func TestGeneratedCRD_KeepsAVersionWrittenToStatus(t *testing.T) {
	scheme, restCfg := testutil.SetupEnvtestEnv(t)
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
	require.NoError(t, err, "Creating controller-runtime client failed")

	crd := generatedCRD(t, "empty")
	require.NoError(t, cli.Create(t.Context(), &crd))
	t.Cleanup(func() {
		require.NoError(t, cli.Delete(context.Background(), &crd))
	})

	gvk := establishedGVK(t, cli, crd)

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(gvk)
	claim.SetNamespace("default")
	claim.SetName("version-write")
	require.NoError(t, cli.Create(t.Context(), claim))

	require.NoError(t, unstructured.SetNestedField(claim.Object, "1.2.3", "status", "version"))
	require.NoError(t, cli.Status().Update(t.Context(), claim))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	require.NoError(t, cli.Get(t.Context(),
		client.ObjectKey{Namespace: "default", Name: "version-write"}, got))

	version, found, err := unstructured.NestedString(got.Object, "status", "version")
	require.NoError(t, err)
	require.True(t, found, "the API server pruned status.version: it is not in the generated schema")
	require.Equal(t, "1.2.3", version)
}

// generatedCRD generates the CRD for one chart under testdata.
func generatedCRD(t *testing.T, chartPath string) apiextv1.CustomResourceDefinition {
	t.Helper()

	chartLoader, err := loader.Loader("./testdata/charts/" + chartPath)
	require.NoError(t, err, "Creating chart loader failed for chart: %s", chartPath)
	chart, err := chartLoader.Load()
	require.NoError(t, err, "Loading chart failed for chart: %s", chartPath)

	crd, err := schemagen.GenerateCRD(*chart)
	require.NoError(t, err, "GenerateCRD failed for chart: %s", chartPath)
	return crd
}

// establishedGVK waits for the API server to serve the CRD's kind and returns
// it. Creating an instance before that is a race.
func establishedGVK(t *testing.T, cli client.Client, crd apiextv1.CustomResourceDefinition) schema.GroupVersionKind {
	t.Helper()

	var gvk schema.GroupVersionKind
	require.Eventually(t, func() bool {
		var got apiextv1.CustomResourceDefinition
		if err := cli.Get(t.Context(), client.ObjectKey{Name: crd.Name}, &got); err != nil {
			return false
		}
		if got.Status.AcceptedNames.Kind == "" || len(got.Status.StoredVersions) == 0 {
			return false
		}
		gvk = schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: got.Status.StoredVersions[0],
			Kind:    got.Status.AcceptedNames.Kind,
		}
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(got.Status.AcceptedNames.ListKind))
		return cli.List(t.Context(), &list) == nil
	}, 3*time.Second, 10*time.Millisecond)

	return gvk
}
