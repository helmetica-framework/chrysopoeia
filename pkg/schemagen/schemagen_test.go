package schemagen_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
	"github.com/helmetica-framework/chrysopoeia/testutil"
)

func TestGenerateCRD_Golden(t *testing.T) {
	scheme, restCfg := testutil.SetupEnvtestEnv(t)
	cli, err := client.New(restCfg, client.Options{Scheme: scheme})
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
			})
		})
	}
}
