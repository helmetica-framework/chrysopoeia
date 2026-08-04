package schemagen_test

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
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
	chartLoader, err := loader.Loader("./testdata/charts/juiceshop-chart")
	if err != nil {
		t.Fatalf("Creating chart loader failed: %v", err)
	}
	chart, err := chartLoader.Load()
	if err != nil {
		t.Fatalf("Loading chart failed: %v", err)
	}

	crd, err := schemagen.GenerateCRD(*chart)
	if err != nil {
		t.Fatalf("GenerateCRD failed: %v", err)
	}

	yamlData, err := kubeyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("Failed to marshal CRD to YAML: %v", err)
	}

	if err := os.WriteFile("testdata/charts/juiceshop-crd.yaml", yamlData, 0644); err != nil {
		t.Fatalf("Failed to write CRD to file: %v", err)
	}
}

//go:embed testdata/preprocess/*
var preprocessTestFiles embed.FS

func Test_PreprocessYAMLHints(t *testing.T) {
	files, err := fs.Glob(preprocessTestFiles, "testdata/preprocess/*.processed.yaml")
	require.NoError(t, err)

	for _, processedFile := range files {
		t.Run(strings.TrimSuffix(filepath.Base(processedFile), ".processed.yaml"), func(t *testing.T) {
			originalFile := strings.TrimSuffix(processedFile, ".processed.yaml") + ".yaml"
			originalData, err := preprocessTestFiles.ReadFile(originalFile)
			require.NoError(t, err)

			expectedProcessedData, err := preprocessTestFiles.ReadFile(processedFile)
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
