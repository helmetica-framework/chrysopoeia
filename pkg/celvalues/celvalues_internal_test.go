package celvalues

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are in-package on purpose. defaults and exprs are what Apply consumes, and the
// external suite can only observe errors, so a scan that silently dropped every default or
// recorded the wrong path would still look green from outside.

func TestScan_SplitsDefaultsFromExpressions(t *testing.T) {
	p, err := New([]byte(`
ingressHostname: ""
replicas: 1
'#replicas':
  export: true
server:
  ingress:
    enabled: "cel: values.ingressHostname != ''"
    className: nginx
  '#ingress':
    description: how the service is exposed
`))
	require.NoError(t, err)

	assert.Equal(t, map[string]any{
		"ingressHostname": "",
		"replicas":        float64(1),
		"server": map[string]any{
			"ingress": map[string]any{"className": "nginx"},
		},
	}, p.defaults, "hint keys are dropped, the cel: slot is not a default, everything else survives")

	require.Len(t, p.exprs, 1)
	assert.Equal(t, []string{"server", "ingress", "enabled"}, p.exprs[0].path)
	assert.Equal(t, "values.ingressHostname != ''", p.exprs[0].src, "src is what was compiled, trimmed")
	assert.NotNil(t, p.exprs[0].prog)
}

func TestScan_RecordsOneDistinctPathPerExpression(t *testing.T) {
	// Guards against slice aliasing in the recursive walk: siblings must not overwrite each other.
	p, err := New([]byte(`
a:
  one: "cel: 1"
  two: "cel: 2"
  deep:
    three: "cel: 3"
b:
  four: "cel: 4"
`))
	require.NoError(t, err)

	paths := make([][]string, 0, len(p.exprs))
	for _, e := range p.exprs {
		paths = append(paths, e.path)
	}
	assert.Equal(t, [][]string{
		{"a", "deep", "three"},
		{"a", "one"},
		{"a", "two"},
		{"b", "four"},
	}, paths, "sorted by key at every level, so the order does not depend on map iteration")
}

func TestScan_IsDeterministic(t *testing.T) {
	// Go randomises map iteration. Without the sort, the collected order and the error a chart
	// author is shown both change between runs on identical input.
	const values = `
alpha: "cel: 1"
beta: "cel: 2"
gamma: "cel: 3"
delta: "cel: 4"
epsilon: "cel: 5"
`
	first, err := New([]byte(values))
	require.NoError(t, err)

	for range 50 {
		p, err := New([]byte(values))
		require.NoError(t, err)
		for i := range p.exprs {
			require.Equal(t, first.exprs[i].path, p.exprs[i].path)
		}
	}
}

func TestScan_ReportsTheSameErrorEveryRun(t *testing.T) {
	const values = `
alpha: "cel: nope.a"
beta: "cel: nope.b"
gamma: "cel: nope.c"
`
	_, first := New([]byte(values))
	require.Error(t, first)
	assert.ErrorContains(t, first, "alpha", "the first failing key in sorted order, not a random one")

	for range 50 {
		_, err := New([]byte(values))
		require.EqualError(t, err, first.Error())
	}
}

func TestScan_KeepsHintKeysOutOfDefaults(t *testing.T) {
	p, err := New([]byte(`
'#public':
  export: true
public: ""
'#':
  description: a hint key with an empty name
`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"public": ""}, p.defaults)
}

func TestNew_DefaultsIsNeverNil(t *testing.T) {
	// Task 2 merges chart defaults under the claim values; a nil map there is a landmine.
	for name, values := range map[string]string{
		"nil":          "",
		"commentsOnly": "# nothing to configure\n",
		"explicitNull": "null\n",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := New([]byte(values))
			require.NoError(t, err)
			assert.NotNil(t, p.defaults)
			assert.Empty(t, p.defaults)
		})
	}
}

func TestNew_RejectsANonMappingRoot(t *testing.T) {
	for name, values := range map[string]string{
		"list":   "- a\n- b\n",
		"scalar": "just a string\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New([]byte(values))
			require.Error(t, err)
			assert.ErrorContains(t, err, "must be a mapping at its root")
			assert.NotContains(t, err.Error(), "JSON", "the author wrote YAML, not JSON")
		})
	}
}

func TestNew_RejectsAnEmptyExpression(t *testing.T) {
	for name, values := range map[string]string{
		"empty":          `a: "cel:"`,
		"whitespaceOnly": `a: "cel:    "`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New([]byte(values))
			require.Error(t, err)
			assert.EqualError(t, err, "a: empty cel: expression",
				"a bare prefix must not dump the CEL parser's token set into a status condition")
		})
	}
}

func TestNew_RejectsNearMissPrefixes(t *testing.T) {
	// Each of these would otherwise be handed to Helm as a literal string and fail at deploy time
	// as a nonsense config value, instead of failing here with a name attached.
	for name, values := range map[string]string{
		"uppercase":    `a: "CEL: values.x"`,
		"mixedCase":    `a: "Cel: values.x"`,
		"leadingSpace": `a: " cel: values.x"`,
		"spaceBefore":  `a: "cel : values.x"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New([]byte(values))
			require.Error(t, err)
			assert.ErrorContains(t, err, "the prefix is exactly")
		})
	}
}

func TestNew_RejectsAnExpressionWrittenAsAKey(t *testing.T) {
	_, err := New([]byte(`"cel: values.a": 1`))
	require.Error(t, err)
	assert.ErrorContains(t, err, "expressions belong in the value")
}

func TestNew_KeepsStringsThatMerelyMentionCel(t *testing.T) {
	p, err := New([]byte(`
a: "celery"
b: "the cel: prefix is documented here"
c: "excel: spreadsheet"
`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"a": "celery",
		"b": "the cel: prefix is documented here",
		"c": "excel: spreadsheet",
	}, p.defaults)
	assert.Empty(t, p.exprs)
}

func TestRejectExpressions_SkipsHintKeysInsideLists(t *testing.T) {
	// pkg/schemagen/parser/testdata/preprocess/array.processed.yaml shows this shape is real.
	p, err := New([]byte(`
secrets:
  - '#name':
      type: string
    name: tls-secret
`))
	require.NoError(t, err, "a hint block is metadata wherever it sits")
	assert.Empty(t, p.exprs)
}

func TestDisplay(t *testing.T) {
	assert.Equal(t, "hosts[1].host", display([]string{"hosts", "[1]", "host"}))
	assert.Equal(t, "server.ingress.enabled", display([]string{"server", "ingress", "enabled"}))
	assert.Equal(t, "a", display([]string{"a"}))
	assert.Empty(t, display(nil))
}
