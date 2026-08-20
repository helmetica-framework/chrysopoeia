package celvalues_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"

	"github.com/helmetica-framework/chrysopoeia/pkg/celvalues"
)

func TestNew_AcceptsAChartWithoutExpressions(t *testing.T) {
	p, err := celvalues.New([]byte("replicaCount: 1\nimage:\n  tag: latest\n"))
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestNew_AcceptsEmptyValues(t *testing.T) {
	// A chart shipping only CRDs has an empty or comment-only values.yaml.
	for name, values := range map[string]string{
		"empty":        "",
		"commentsOnly": "# The CRDs of the operator, this chart takes no values.\n",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := celvalues.New([]byte(values))
			require.NoError(t, err)
			assert.NotNil(t, p)
		})
	}
}

func TestNew_RejectsSyntaxError(t *testing.T) {
	_, err := celvalues.New([]byte(`enabled: "cel: values.host !="`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "enabled", "the error names the path of the offending expression")
}

func TestNew_RejectsUnknownRootVariable(t *testing.T) {
	// The typo that this whole compile stage exists to catch.
	_, err := celvalues.New([]byte(`
server:
  ingress:
    enabled: "cel: valeus.ingressHostname != ''"
`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "server.ingress.enabled")
	assert.ErrorContains(t, err, "valeus")
}

func TestNew_RejectsTypeError(t *testing.T) {
	_, err := celvalues.New([]byte(`enabled: "cel: 'a string' + 1"`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "enabled")
}

func TestNew_RejectsExpressionsInsideLists(t *testing.T) {
	// Lists have no stable path to write a result back to, so an expression in one is a
	// chart-author error rather than a silently ignored string.
	_, err := celvalues.New([]byte(`
hosts:
  - host: example.com
  - host: "cel: values.ingressHostname"
`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "hosts")
	assert.ErrorContains(t, err, "list")
}

func TestNew_RejectsUnparseableYAML(t *testing.T) {
	_, err := celvalues.New([]byte("a:\n\t- broken"))
	require.Error(t, err)
}

func TestNewFromChart_ChartWithoutValuesYAMLHasNoExpressions(t *testing.T) {
	// Loading a chart that ships no values.yaml must not fail: there is simply nothing to evaluate.
	p, err := celvalues.NewFromChart(&chartv2.Chart{Metadata: &chartv2.Metadata{Name: "crds-only"}})
	require.NoError(t, err)
	assert.NotNil(t, p)
}

func TestNewFromChart_ReadsRawValuesYAML(t *testing.T) {
	chart := &chartv2.Chart{
		Metadata: &chartv2.Metadata{Name: "wrapper"},
		Raw: []*common.File{
			{Name: "Chart.yaml", Data: []byte("name: wrapper\n")},
			{Name: "values.yaml", Data: []byte(`enabled: "cel: valeus.host != ''"`)},
		},
	}
	_, err := celvalues.NewFromChart(chart)
	require.Error(t, err, "the expression in values.yaml is compiled, so its typo is reported")
	assert.ErrorContains(t, err, "valeus")
}
