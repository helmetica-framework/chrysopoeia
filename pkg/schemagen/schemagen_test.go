package schemagen_test

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
	"github.com/stretchr/testify/assert"
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

func Test_PreprocessYAMLHints(t *testing.T) {
	preprocessTestFiles := os.DirFS("testdata/preprocess")
	files, err := fs.Glob(preprocessTestFiles, "*.processed.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files, "No test files found in testdata/preprocess")

	for _, processedFile := range files {
		t.Run(strings.TrimSuffix(processedFile, ".processed.yaml"), func(t *testing.T) {
			originalFile := strings.TrimSuffix(processedFile, ".processed.yaml") + ".yaml"
			originalData, err := fs.ReadFile(preprocessTestFiles, originalFile)
			require.NoError(t, err)

			expectedProcessedData, err := fs.ReadFile(preprocessTestFiles, processedFile)
			require.NoError(t, err)

			actualProcessedData, err := schemagen.PreprocessYAMLHints(originalData)
			require.NoError(t, err)

			expectedJSON, err := kubeyaml.YAMLToJSONStrict(expectedProcessedData)
			require.NoError(t, err)
			actualJSON, err := kubeyaml.YAMLToJSONStrict(actualProcessedData)
			require.NoError(t, err)

			assert.JSONEq(t, string(expectedJSON), string(actualJSON), "Processed YAML does not match expected for file: %s", originalFile)
		})
	}
}

func Test_PreprocessYAMLHints_NothingToConfigure(t *testing.T) {
	// A chart shipping only CRDs has nothing to configure and so an empty or comment-only values.yaml.
	for name, values := range map[string]string{
		"empty":        "",
		"commentsOnly": "# The CRDs of the operator, this chart takes no values.\n",
	} {
		t.Run(name, func(t *testing.T) {
			processed, err := schemagen.PreprocessYAMLHints([]byte(values))
			require.NoError(t, err)
			assert.Equal(t, values, string(processed))
		})
	}
}

func TestGenerateCRD_ChartWithoutValues(t *testing.T) {
	chart := chartv2.Chart{
		Metadata: &chartv2.Metadata{Name: "mariadb-operator-crds", Version: "26.6.0"},
		Raw:      []*common.File{{Name: "values.yaml", Data: []byte("")}},
	}

	crd, err := schemagen.GenerateCRD(chart)
	require.NoError(t, err)

	values := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["values"]
	assert.Empty(t, values.Properties, "a chart without values has no values to set")
}
