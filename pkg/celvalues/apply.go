package celvalues

import (
	"fmt"
	"math"
	"reflect"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	helmutil "helm.sh/helm/v4/pkg/chart/common/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Apply returns claimValues with the result of every expression written at its path.
//
// claimValues is not mutated, and the returned map shares no structure with it. Chart defaults are
// not folded into the result: merging defaults is Helm's job, and folding them in here would pin
// every default into the HelmRelease and make its diff useless.
//
// Expressions see claimValues merged over the chart defaults as `values`, with claim values
// winning, and c as `claim`. They never see each other's results.
//
// claimValues must hold only the types Kubernetes uses for unstructured data: string, int64,
// bool, float64, nil, map[string]any and []any. Anything else is reported as an error rather than
// evaluated.
func (p *Preprocessor) Apply(claimValues map[string]any, c Claim) (out map[string]any, err error) {
	// runtime.DeepCopyJSON panics on anything that is not a Kubernetes unstructured type.
	// So we catch them here to not kill the whole controller.
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("the claim values are not valid unstructured data: %v", r)
		}
	}()

	out = runtime.DeepCopyJSON(claimValues)
	if out == nil {
		out = map[string]any{}
	}
	if len(p.exprs) == 0 {
		return out, nil
	}

	// CoalesceTables mutates both of its arguments, so each side is its own deep copy: `out` must
	// not pick up the chart defaults, and p.defaults has to survive for the next call.
	vars := map[string]any{
		"values": helmutil.CoalesceTables(runtime.DeepCopyJSON(claimValues), runtime.DeepCopyJSON(p.defaults)),
		"claim":  c.asMap(),
	}

	for _, expr := range p.exprs {
		ev, _, err := expr.prog.Eval(vars)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", display(expr.path), err)
		}

		val, err := jsonValue(ev)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", display(expr.path), err)
		}

		if conflict, at, ok := pathConflict(out, expr.path); ok {
			return nil, fmt.Errorf("%s: the claim sets %s to %s, so there is nowhere to write this value",
				display(expr.path), display(at), conflict)
		}
		if err := unstructured.SetNestedField(out, val, expr.path...); err != nil {
			return nil, fmt.Errorf("%s: cannot write the result: %w", display(expr.path), err)
		}
	}

	return out, nil
}

// asMap renders the claim for CEL. The label and annotation maps are passed through as
// map[string]string; cel-go's reflection adapter handles them against the map(string, dyn)
// declaration of the `claim` root, so widening them buys nothing.
func (c Claim) asMap() map[string]any {
	return map[string]any{
		"annotations": c.Annotations,
		"labels":      c.Labels,
		"name":        c.Name,
		"namespace":   c.Namespace,
	}
}

// jsonValue converts a CEL result to the plain JSON types a CRD can hold, in the shapes
// Kubernetes uses for unstructured data: integral numbers as int64, everything else as string,
// bool, float64, nil, map[string]any or []any.
func jsonValue(val ref.Val) (any, error) {
	switch v := val.(type) {
	case types.Bool:
		return bool(v), nil
	case types.String:
		return string(v), nil
	case types.Int:
		return int64(v), nil
	case types.Uint:
		if uint64(v) > math.MaxInt64 {
			return nil, fmt.Errorf("the result %d does not fit in an int64", uint64(v))
		}
		return int64(v), nil
	case types.Double:
		return float64(v), nil
	case types.Null:
		return nil, nil
	case traits.Lister:
		return jsonList(v)
	case traits.Mapper:
		return jsonMap(v)
	}

	native, err := val.ConvertToNative(reflect.TypeOf(""))
	if err != nil {
		return nil, fmt.Errorf("the result is not representable in JSON: %w", err)
	}
	return native, nil
}

func jsonList(list traits.Lister) ([]any, error) {
	size, ok := list.Size().(types.Int)
	if !ok {
		return nil, fmt.Errorf("the result is not representable in JSON: a list of unknown size")
	}
	out := make([]any, 0, int(size))
	for i := range int64(size) {
		item, err := jsonValue(list.Get(types.Int(i)))
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func jsonMap(m traits.Mapper) (map[string]any, error) {
	out := map[string]any{}
	for it := m.Iterator(); it.HasNext() == types.True; {
		key := it.Next()
		name, ok := key.(types.String)
		if !ok {
			return nil, fmt.Errorf("the result is not representable in JSON: a map keyed by %s, only string keys are allowed", key.Type().TypeName())
		}
		value, err := jsonValue(m.Get(key))
		if err != nil {
			return nil, err
		}
		out[string(name)] = value
	}
	return out, nil
}

// pathConflict reports whether something other than a map already sits on the way to path, so that
// the result cannot be written there. It returns a description of what is in the way and the path
// it sits at. unstructured.SetNestedField refuses the write on its own, but its message names Go
// types and a second path notation, and this one is read off a status condition by whoever wrote
// the claim.
func pathConflict(values map[string]any, path []string) (string, []string, bool) {
	node := any(values)
	for i, segment := range path[:len(path)-1] {
		m, ok := node.(map[string]any)
		if !ok {
			return describe(node), path[:i], true
		}
		next, present := m[segment]
		if !present {
			// SetNestedField creates the missing maps itself, so there is nothing in the way.
			return "", nil, false
		}
		node = next
	}
	// The loop checks what it stepped through, not what it landed on.
	if _, ok := node.(map[string]any); !ok {
		return describe(node), path[:len(path)-1], true
	}
	return "", nil, false
}

// describe names a value the way someone who wrote YAML would recognise it.
func describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case int64, float64:
		return "a number"
	case []any:
		return "a list"
	}
	return fmt.Sprintf("%T", v)
}
