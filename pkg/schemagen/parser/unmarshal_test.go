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
