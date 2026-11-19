package parser_test

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/helmetica-framework/chrysopoeia/pkg/schemagen/parser"
)

func ExampleHintsUnmarshaler() {
	raw := []byte(`{
		"foo": "bar",
		"#foo": {
			"description": "This is a hint for foo"
		}
	}`)
	var v any
	_ = json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
	fmt.Printf("%T\n", v)
	tv := v.(ObjWithHints)
	fmt.Println("foo:", tv["foo"].Value, "#", tv["foo"].Hint.Description)
	// Output:
	// parser.ObjWithHints
	// foo: bar # This is a hint for foo
}

func Test_Unmarshal(t *testing.T) {
	raw, err := os.ReadFile("testdata/in.json")
	require.NoError(t, err)

	var v any
	err = json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
	require.NoError(t, err)
	assert.IsType(t, ObjWithHints{}, v)
}

func Test_Unmarshal_InvalidHint(t *testing.T) {
	raw := []byte(`{"#foo": "bar"}`)

	var v any
	err := json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
	require.Error(t, err)
}

func Test_Unmarshal_ValidationHint(t *testing.T) {
	raw := []byte(`{
		"#foo": {
			"validation": [
				{
					"rule": "!oldSelf.hasValue() || self == oldSelf.value()",
					"message": "immutable",
					"message_expression": "'got ' + self",
					"reason": "FieldValueForbidden",
					"field_path": ".bar",
					"optional_old_self": true
				}
			]
		}
	}`)

	var v any
	require.NoError(t, json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler())))

	obj, ok := v.(ObjWithHints)
	require.True(t, ok)
	require.NotNil(t, obj["foo"].Hint)
	require.Len(t, obj["foo"].Hint.Validation, 1)

	rule := obj["foo"].Hint.Validation[0]
	assert.Equal(t, "!oldSelf.hasValue() || self == oldSelf.value()", rule.Rule)
	assert.Equal(t, "immutable", rule.Message)
	assert.Equal(t, "'got ' + self", rule.MessageExpression)
	assert.Equal(t, "FieldValueForbidden", rule.Reason)
	assert.Equal(t, ".bar", rule.FieldPath)
	require.NotNil(t, rule.OptionalOldSelf)
	assert.True(t, *rule.OptionalOldSelf)
}

func Test_Unmarshal_ValidationHint_RequiresRule(t *testing.T) {
	raw := []byte(`{"#foo": {"validation": [{"message": "no rule"}]}}`)

	var v any
	err := json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
	require.ErrorContains(t, err, "validation[0]: rule must not be empty")
}

func Test_Unmarshal_ValidationHint_AcceptsKnownReasons(t *testing.T) {
	for _, reason := range []string{
		"FieldValueInvalid",
		"FieldValueForbidden",
		"FieldValueRequired",
		"FieldValueDuplicate",
	} {
		t.Run(reason, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"#foo": {"validation": [{"rule": "self > 0", "reason": %q}]}}`, reason))

			var v any
			require.NoError(t, json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler())))
		})
	}
}

func Test_Unmarshal_ValidationHint_RejectsUnknownReason(t *testing.T) {
	raw := []byte(`{"#foo": {"validation": [{"rule": "self > 0", "reason": "Bogus"}]}}`)

	var v any
	err := json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
	require.ErrorContains(t, err, `validation[0]: invalid reason "Bogus"`)
}

func Test_Unmarshal_ValidationHint_ReasonIsOptional(t *testing.T) {
	raw := []byte(`{"#foo": {"validation": [{"rule": "self > 0"}]}}`)

	var v any
	require.NoError(t, json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler())))
}

func Test_Unmarshal_BoundsHints(t *testing.T) {
	raw := []byte(`{
		"#foo": {
			"max_length": 63,
			"max_items": 5,
			"max_properties": 10
		}
	}`)

	var v any
	require.NoError(t, json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler())))

	obj, ok := v.(ObjWithHints)
	require.True(t, ok)
	require.NotNil(t, obj["foo"].Hint)

	require.NotNil(t, obj["foo"].Hint.MaxLength)
	assert.Equal(t, int64(63), *obj["foo"].Hint.MaxLength)
	require.NotNil(t, obj["foo"].Hint.MaxItems)
	assert.Equal(t, int64(5), *obj["foo"].Hint.MaxItems)
	require.NotNil(t, obj["foo"].Hint.MaxProperties)
	assert.Equal(t, int64(10), *obj["foo"].Hint.MaxProperties)
}

func Test_Unmarshal_BoundsHints_RejectNegative(t *testing.T) {
	for name, raw := range map[string][]byte{
		"max_length":     []byte(`{"#foo": {"max_length": -1}}`),
		"max_items":      []byte(`{"#foo": {"max_items": -1}}`),
		"max_properties": []byte(`{"#foo": {"max_properties": -1}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var v any
			err := json.Unmarshal(raw, &v, json.WithUnmarshalers(HintsUnmarshaler()))
			require.ErrorContains(t, err, name+" must not be negative")
		})
	}
}
