package schemagen_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
					return len(tmpCRD.Status.AcceptedNames.Plural) > 0
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
