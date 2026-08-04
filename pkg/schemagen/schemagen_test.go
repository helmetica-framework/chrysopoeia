package schemagen_test

import (
	"os"
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
	chartLoader, err := loader.Loader("./testdata/juiceshop-chart")
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

	if err := os.WriteFile("testdata/juiceshop-crd.yaml", yamlData, 0644); err != nil {
		t.Fatalf("Failed to write CRD to file: %v", err)
	}
}

func Test_PreprocessYAMLHints(t *testing.T) {
	yamlData := []byte(`
# This is a description for the root
root:
  # This is a description for child1
  child1: value1
  # This is a description for child2
  '#child2': {type: string}
  child2:
`)

	processedYAML, err := schemagen.PreprocessYAMLHints(yamlData)
	require.NoError(t, err)
	yamlData, err = kubeyaml.YAMLToJSONStrict(processedYAML)
	require.NoError(t, err)

	assert.JSONEq(t, `{
  "root": {
    "child1": "value1",
    "#child2": {
      "type": "string",
      "description": "This is a description for child2"
    },
    "child2": null,
    "#child1": {
      "description": "This is a description for child1"
    }
  },
  "#root": {
    "description": "This is a description for the root"
  }
}`, string(yamlData))
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
