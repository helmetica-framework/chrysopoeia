// Package celvalues evaluates the CEL expressions a wrapper chart puts into its values.yaml.
//
// A chart author writes an expression where a value would otherwise go:
//
//	ingressHostname: ""
//	podinfo:
//	  ingress:
//	    enabled: "cel: values.ingressHostname != ''"
//	    hosts:   "cel: [{'host': values.ingressHostname}]"
//
// Expressions see two roots: `values`, the claim's values merged over the chart's defaults, and
// `claim`, the metadata of the claim resource. They never see each other's results.
package celvalues

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen/parser"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"sigs.k8s.io/yaml"
)

// Claim is the metadata of the claim resource, exposed to expressions as `claim`.
type Claim struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
}

// Preprocessor holds the compiled expressions of one chart together with that chart's plain
// default values.
type Preprocessor struct {
	defaults map[string]any
	exprs    []expression
}

type expression struct {
	path []string
	src  string
	prog cel.Program
}

// New scans a chart's values.yaml for CEL expressions, compiles and type-checks each one.
// An error from New is a chart-author error and names the dotted path of the expression.
func New(valuesYaml []byte) (*Preprocessor, error) {
	prep := &Preprocessor{}

	var root any
	if err := yaml.Unmarshal(valuesYaml, &root); err != nil {
		return nil, fmt.Errorf("failed to parse values.yaml: %w", err)
	}
	values, ok := root.(map[string]any)
	if root != nil && !ok {
		// The unmarshaller's own message talks about JSON and Go types, which means nothing to
		// someone who wrote YAML and reads this back off a status condition.
		return nil, fmt.Errorf("values.yaml must be a mapping at its root, got %T", root)
	}
	if values == nil {
		values = map[string]any{}
	}

	env, err := cel.NewEnv(
		cel.Variable("values", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("claim", cel.MapType(cel.StringType, cel.DynType)),
		ext.Bindings(),
		ext.Strings(),
		ext.Lists(),
		ext.TwoVarComprehensions(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating cel environment: %w", err)
	}

	defaults := map[string]any{}

	if err := prep.scan(env, []string{}, values, defaults); err != nil {
		// No wrap: the error already names the path it came from, and a prefix in front of it only
		// pushes that path rightwards in the status condition an author reads.
		return nil, err
	}

	prep.defaults = defaults

	return prep, nil
}

// scan walks src in sorted key order, collecting expressions and copying every plain value into
// dst. The order matters: Go randomises map iteration, and a chart with two broken expressions
// would otherwise report a different one on every reconcile, churning the status condition and
// hiding the second error from the author.
func (p *Preprocessor) scan(env *cel.Env, path []string, src, dst map[string]any) error {
	for _, key := range slices.Sorted(maps.Keys(src)) {
		val := src[key]
		if strings.HasPrefix(key, "#") {
			// skipping hint blocks
			continue
		}
		keyPath := append(slices.Clone(path), key)

		if looksLikeExpression(key) {
			return fmt.Errorf("%s: a key looks like a cel: expression, expressions belong in the value", display(keyPath))
		}

		switch v := val.(type) {
		case string:
			expr, ok := strings.CutPrefix(v, parser.CelPrefix)
			// not an expression, add to default
			if !ok {
				if nearMiss(v) {
					return fmt.Errorf("%s: %q is not a cel: expression, the prefix is exactly %q", display(keyPath), v, parser.CelPrefix)
				}
				dst[key] = v
				continue
			}
			expr = strings.TrimSpace(expr)
			if expr == "" {
				return fmt.Errorf("%s: empty cel: expression", display(keyPath))
			}
			prog, err := compile(env, expr)
			if err != nil {
				return fmt.Errorf("%s: %w", display(keyPath), err)
			}
			p.exprs = append(p.exprs, expression{path: keyPath, src: expr, prog: prog})

		case map[string]any:
			sub := make(map[string]any, len(v))
			if err := p.scan(env, keyPath, v, sub); err != nil {
				return err
			}
			dst[key] = sub

		default:
			if err := rejectExpressions(keyPath, v); err != nil {
				return err
			}
			dst[key] = v
		}
	}
	return nil
}

// rejectExpressions fails on a cel: string anywhere below path. Lists have no stable path to write
// a result back to, so an expression inside one is a chart-author error rather than a silent
// literal string handed to Helm.
func rejectExpressions(path []string, val any) error {
	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, parser.CelPrefix) {
			return fmt.Errorf("%s: a cel: expression cannot appear inside a list", display(path))
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if strings.HasPrefix(key, "#") {
				// a hint block is metadata wherever it sits, including inside a list item
				continue
			}
			if err := rejectExpressions(append(slices.Clone(path), key), v[key]); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range v {
			if err := rejectExpressions(append(slices.Clone(path), fmt.Sprintf("[%d]", i)), item); err != nil {
				return err
			}
		}
	}
	return nil
}

// nearMissPrefix matches the spellings of the prefix that are not the prefix: a different case, a
// leading space, a space before the colon.
var nearMissPrefix = regexp.MustCompile(`(?i)^\s*cel\s*:`)

// looksLikeExpression reports whether s carries the prefix in any spelling. Used on keys, where
// even the canonical spelling is a mistake.
func looksLikeExpression(s string) bool {
	return nearMissPrefix.MatchString(s)
}

// nearMiss reports whether s looks like an attempt at the prefix without being it. Such a string
// would otherwise be handed to Helm verbatim and fail at deploy time as a nonsense config value,
// which is exactly the class of author mistake compiling at load time exists to catch.
func nearMiss(s string) bool {
	return !strings.HasPrefix(s, parser.CelPrefix) && nearMissPrefix.MatchString(s)
}

// display renders a path for a human: dots between keys, no dot before a list index.
func display(path []string) string {
	var b strings.Builder
	for i, segment := range path {
		if i > 0 && !strings.HasPrefix(segment, "[") {
			b.WriteByte('.')
		}
		b.WriteString(segment)
	}
	return b.String()
}

func compile(env *cel.Env, src string) (cel.Program, error) {
	ast, iss := env.Compile(src)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return env.Program(ast)
}

// NewFromChart is New over the raw values.yaml of a loaded chart.
// A chart that ships no values.yaml has no expressions and yields a Preprocessor that changes
// nothing.
func NewFromChart(chart *chartv2.Chart) (*Preprocessor, error) {
	if chart == nil {
		return New(nil)
	}
	for _, f := range chart.Raw {
		// The match is exact and load-bearing: chart.Raw carries every file of the archive, so a
		// subchart's values.yaml is in here as charts/<name>/values.yaml. Loosening this to a
		// suffix or basename match would start evaluating expressions the wrapper chart does not
		// own.
		if f.Name == "values.yaml" {
			return New(f.Data)
		}
	}
	return New(nil)
}
