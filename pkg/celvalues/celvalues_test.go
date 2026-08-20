package celvalues_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/common"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	k8sjson "k8s.io/apimachinery/pkg/util/json"

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

// theADRExample is the wrapper chart from improve-wrapper-chart-ux-draft-adr.adoc, rewritten with
// expressions. The user sets one field; the chart derives four.
const theADRExample = `
ingressHostname: ""

server:
  ingress:
    enabled: "cel: values.ingressHostname != ''"
    hosts: "cel: values.ingressHostname == '' ? [] : [{'host': values.ingressHostname}]"
    tls: |
      cel: cel.bind(h, values.ingressHostname,
        h == '' ? [] : [{'secretName': claim.name + '-tls', 'hosts': [h]}])
`

func TestApply_TheADRExample(t *testing.T) {
	p, err := celvalues.New([]byte(theADRExample))
	require.NoError(t, err)

	out, err := p.Apply(
		map[string]any{"ingressHostname": "example.com"},
		celvalues.Claim{Name: "prod", Namespace: "team-a"},
	)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"ingressHostname": "example.com",
		"server": map[string]any{
			"ingress": map[string]any{
				"enabled": true,
				"hosts":   []any{map[string]any{"host": "example.com"}},
				"tls": []any{map[string]any{
					"secretName": "prod-tls",
					"hosts":      []any{"example.com"},
				}},
			},
		},
	}, out)
}

func TestApply_ExpressionsSeeChartDefaults(t *testing.T) {
	// The user set nothing, so the expression reads the chart's own default.
	p, err := celvalues.New([]byte(theADRExample))
	require.NoError(t, err)

	out, err := p.Apply(nil, celvalues.Claim{Name: "prod", Namespace: "team-a"})
	require.NoError(t, err)

	ingress := out["server"].(map[string]any)["ingress"].(map[string]any)
	assert.Equal(t, false, ingress["enabled"])
	assert.Equal(t, []any{}, ingress["hosts"])
	assert.Equal(t, []any{}, ingress["tls"])
}

func TestApply_ClaimValuesWinOverChartDefaults(t *testing.T) {
	p, err := celvalues.New([]byte("replicas: 1\ncomputed: \"cel: values.replicas\"\n"))
	require.NoError(t, err)

	out, err := p.Apply(map[string]any{"replicas": int64(3)}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, out["computed"])
}

func TestApply_OutputCarriesClaimValuesAndComputedPathsOnly(t *testing.T) {
	// Chart defaults are Helm's job. Folding them in here would pin every default into the
	// HelmRelease and make its diff useless.
	p, err := celvalues.New([]byte(`
replicas: 1
untouchedDefault: "keep me out"
computed: "cel: values.replicas"
`))
	require.NoError(t, err)

	out, err := p.Apply(map[string]any{"replicas": int64(3)}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"replicas": int64(3), "computed": int64(3)}, out)
	assert.NotContains(t, out, "untouchedDefault")
}

func TestApply_DoesNotMutateItsInput(t *testing.T) {
	p, err := celvalues.New([]byte(`
replicas: 1
server:
  computed: "cel: values.replicas"
`))
	require.NoError(t, err)

	in := map[string]any{"replicas": int64(3), "server": map[string]any{"existing": "value"}}
	_, err = p.Apply(in, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{"replicas": int64(3), "server": map[string]any{"existing": "value"}}, in)
}

func TestApply_DoesNotLeakBetweenCalls(t *testing.T) {
	// The controller shares one Preprocessor per chart digest across every claim using that chart.
	// CoalesceTables mutates both of its arguments, so a call that let the claim's values reach
	// p.defaults would poison every later claim. The second call has to differ from the first:
	// repeating identical input would return the same poisoned answer and look stable.
	p, err := celvalues.New([]byte(theADRExample))
	require.NoError(t, err)

	withHost, err := p.Apply(map[string]any{"ingressHostname": "example.com"}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, true, withHost["server"].(map[string]any)["ingress"].(map[string]any)["enabled"])

	for range 5 {
		bare, err := p.Apply(nil, celvalues.Claim{Name: "other"})
		require.NoError(t, err)
		ingress := bare["server"].(map[string]any)["ingress"].(map[string]any)
		assert.Equal(t, false, ingress["enabled"], "the previous claim's hostname must not survive")
		assert.Equal(t, []any{}, ingress["hosts"])
		assert.Equal(t, []any{}, ingress["tls"])
	}
}

func TestApply_WritesAlongsideExistingClaimKeys(t *testing.T) {
	p, err := celvalues.New([]byte(`
host: ""
server:
  computed: "cel: values.host"
`))
	require.NoError(t, err)

	out, err := p.Apply(
		map[string]any{"host": "example.com", "server": map[string]any{"userSet": "kept"}},
		celvalues.Claim{Name: "prod"},
	)
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"host":   "example.com",
		"server": map[string]any{"userSet": "kept", "computed": "example.com"},
	}, out)
}

func TestApply_ClaimMetadataIsAvailable(t *testing.T) {
	p, err := celvalues.New([]byte(`
name: "cel: claim.name"
namespace: "cel: claim.namespace"
org: "cel: claim.labels['appuio.io/organization']"
missing: "cel: 'nope' in claim.annotations ? 'yes' : 'no'"
`))
	require.NoError(t, err)

	out, err := p.Apply(nil, celvalues.Claim{
		Name:      "prod",
		Namespace: "team-a",
		Labels:    map[string]string{"appuio.io/organization": "acme"},
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"name": "prod", "namespace": "team-a", "org": "acme", "missing": "no",
	}, out)
}

func TestApply_HintKeysNeverReachExpressions(t *testing.T) {
	// Hint blocks are schema metadata that happens to live in values.yaml. An expression must not
	// see them, otherwise a hint rename silently changes a computed value.
	p, err := celvalues.New([]byte(`
'#public':
  export: true
public: ""
keys: "cel: values.map(k, k)"
`))
	require.NoError(t, err)

	out, err := p.Apply(nil, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []any{"public"}, out["keys"])
}

func TestApply_ClaimValuesAreNeverEvaluated(t *testing.T) {
	// The trust boundary: user input is data. A claim value that looks like an expression is
	// passed through verbatim.
	p, err := celvalues.New([]byte("replicas: 1\n"))
	require.NoError(t, err)

	out, err := p.Apply(map[string]any{"note": "cel: 1 + 1"}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "cel: 1 + 1", out["note"])
}

func TestApply_ReportsEvaluationErrors(t *testing.T) {
	p, err := celvalues.New([]byte(`
divisor: 0
server:
  computed: "cel: 1 / int(values.divisor)"
`))
	require.NoError(t, err)

	_, err = p.Apply(nil, celvalues.Claim{Name: "prod"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "server.computed", "the error names the path of the expression")
	assert.ErrorContains(t, err, "division by zero")
}

func TestApply_ReportsAPathConflict(t *testing.T) {
	// The user set `server` to a scalar, so there is nowhere to write server.computed. Reporting
	// beats silently dropping one of the two.
	p, err := celvalues.New([]byte(`
host: ""
server:
  computed: "cel: values.host"
`))
	require.NoError(t, err)

	_, err = p.Apply(map[string]any{"server": "a scalar"}, celvalues.Claim{Name: "prod"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "server.computed")
}

func TestApply_ExpressionsDoNotSeeEachOther(t *testing.T) {
	// There is no chaining. A cel: slot is not part of `values` at all, so an expression reaching
	// for another one's path fails loudly instead of reading a stale or half-resolved value.
	p, err := celvalues.New([]byte(`
host: ""
server:
  enabled: "cel: values.host != ''"
  alsoEnabled: "cel: values.server.enabled"
`))
	require.NoError(t, err)

	_, err = p.Apply(map[string]any{"host": "example.com"}, celvalues.Claim{Name: "prod"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "server.alsoEnabled")
}

func TestApply_NoExpressionsIsAPassThrough(t *testing.T) {
	p, err := celvalues.New([]byte("replicas: 1\n"))
	require.NoError(t, err)

	in := map[string]any{"replicas": int64(3), "nested": map[string]any{"a": "b"}}
	out, err := p.Apply(in, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestApply_ReportsResultsThatAreNotRepresentableInJSON(t *testing.T) {
	for name, expr := range map[string]string{
		"celType":       "type(1)",
		"nonStringKeys": "dyn({1: 'a'})",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := celvalues.New([]byte("computed: \"cel: " + expr + "\"\n"))
			require.NoError(t, err)

			_, err = p.Apply(nil, celvalues.Claim{Name: "prod"})
			require.Error(t, err)
			assert.ErrorContains(t, err, "computed", "the error names the path of the expression")
			assert.ErrorContains(t, err, "not representable in JSON")
		})
	}
}

func TestApply_KeepsIntegersExact(t *testing.T) {
	// proto3's JSON mapping renders every number as a double and encodes an int64 outside the
	// double-safe range as a quoted string. A computed byte count has to stay a number.
	p, err := celvalues.New([]byte(`
n: 0
small: "cel: values.n"
big: "cel: 9007199254740993"
fractional: "cel: 1.5"
`))
	require.NoError(t, err)

	out, err := p.Apply(map[string]any{"n": int64(42)}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)

	assert.Equal(t, int64(42), out["small"], "integral results are int64, as unstructured data is")
	assert.Equal(t, int64(9007199254740993), out["big"], "beyond 2^53 too")
	assert.Equal(t, 1.5, out["fractional"])
}

func TestApply_SurvivesRoundTrippingThroughJSON(t *testing.T) {
	// The controller writes this into an InstanceRevision and later reads it back. If the two are
	// not equal, every reconcile sees a difference and writes again.
	p, err := celvalues.New([]byte("n: 0\ncomputed: \"cel: values.n * 2\"\n"))
	require.NoError(t, err)

	out, err := p.Apply(map[string]any{"n": int64(21)}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)

	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, k8sjson.Unmarshal(encoded, &decoded))

	assert.Equal(t, out, decoded)
}

func TestApply_RejectsClaimValuesThatAreNotUnstructuredData(t *testing.T) {
	// Exported API: a panic here would take the controller down instead of writing a condition.
	p, err := celvalues.New([]byte("replicas: 1\n"))
	require.NoError(t, err)

	for name, values := range map[string]map[string]any{
		"untypedInt": {"x": 3},
		"stringMap":  {"x": map[string]string{"a": "b"}},
		"nested":     {"x": map[string]any{"y": float32(1)}},
	} {
		t.Run(name, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := p.Apply(values, celvalues.Claim{Name: "prod"})
				require.Error(t, err)
				assert.ErrorContains(t, err, "not valid unstructured data")
			})
		})
	}
}

func TestApply_ExplainsAPathConflictInTheClaimsTerms(t *testing.T) {
	p, err := celvalues.New([]byte(`
host: ""
server:
  deep:
    computed: "cel: values.host"
`))
	require.NoError(t, err)

	_, err = p.Apply(
		map[string]any{"server": map[string]any{"deep": "a scalar"}},
		celvalues.Claim{Name: "prod"},
	)
	require.Error(t, err)
	assert.EqualError(t, err,
		"server.deep.computed: the claim sets server.deep to a string, so there is nowhere to write this value")
}

func TestApply_NullingAChartDefaultRemovesItFromTheContext(t *testing.T) {
	// Helm's coalesce deletes a defaulted key the claim sets to null, so the expression sees the
	// key as absent rather than as null. Inherited from Helm and surprising enough to pin.
	p, err := celvalues.New([]byte(`
host: "default.example.com"
computed: "cel: has(values.host) ? values.host : 'gone'"
`))
	require.NoError(t, err)

	kept, err := p.Apply(nil, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "default.example.com", kept["computed"])

	nulled, err := p.Apply(map[string]any{"host": nil}, celvalues.Claim{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "gone", nulled["computed"])
}
