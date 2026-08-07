package parser_test

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kubeyaml "sigs.k8s.io/yaml"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen/parser"
)

func ExamplePreprocessYAML() {
	raw := []byte("# This is a hint for foo\nfoo: bar")
	processed, _ := parser.PreprocessYAML(raw)
	fmt.Println(string(processed))
	// Output:
	// # This is a hint for foo
	// foo: bar
	// '#foo':
	//     description: This is a hint for foo
}

func Test_PreprocessYAML(t *testing.T) {
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

			actualProcessedData, err := parser.PreprocessYAML(originalData)
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
			processed, err := parser.PreprocessYAML([]byte(values))
			require.NoError(t, err)
			assert.Equal(t, values, string(processed))
		})
	}
}
