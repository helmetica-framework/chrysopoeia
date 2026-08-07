package schemagen_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
	"github.com/stretchr/testify/require"
)

func TestGenerateCRD_Golden(t *testing.T) {
	charts, err := fs.Glob(os.DirFS("testdata/charts"), "*/values.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, charts, "No test charts found in testdata/charts")
	for i, chartPath := range charts {
		charts[i] = strings.TrimSuffix(chartPath, "/values.yaml")
	}
	t.Log("Found test charts:", charts)

	for _, chartPath := range charts {
		t.Run(chartPath, func(t *testing.T) {
			chartLoader, err := loader.Loader("./testdata/charts/" + chartPath)
			require.NoError(t, err, "Creating chart loader failed for chart: %s", chartPath)
			chart, err := chartLoader.Load()
			require.NoError(t, err, "Loading chart failed for chart: %s", chartPath)

			crd, err := schemagen.GenerateCRD(*chart)
			require.NoError(t, err, "GenerateCRD failed for chart: %s", chartPath)

			yamlData, err := kubeyaml.Marshal(crd)
			require.NoError(t, err, "Failed to marshal CRD to YAML for chart: %s", chartPath)

			require.NoError(t, os.WriteFile("./testdata/charts/"+chartPath+".golden.yaml", yamlData, 0644))
		})
	}
}
