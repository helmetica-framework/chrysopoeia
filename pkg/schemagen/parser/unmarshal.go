package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// Hint represents a hint for a value in the schema.
// It can be used to provide additional information about the value, such as its description, type, and whether it should be exported.
type Hint struct {
	Description string `json:"description,omitempty"`

	Type string `json:"type"`

	Enum []any `json:"enum,omitempty"`

	Items      any `json:"items,omitempty"`
	Properties any `json:"properties,omitempty"`

	Export *bool `json:"export,omitempty"`
}

var validTypes = sets.New("boolean", "integer", "number", "string")

// UnmarshalJSONFrom unmarshals a Hint from a jsontext.Decoder.
// It expects the JSON to be an object, and will return an error if it is not.
// It applies some validations.
func (h *Hint) UnmarshalJSONFrom(d *jsontext.Decoder) error {
	type noValidation Hint
	var th noValidation
	if err := json.UnmarshalDecode(d, &th); err != nil {
		return fmt.Errorf("failed to unmarshal hint: %w", err)
	}
	if th.Type != "" && !validTypes.Has(th.Type) {
		return fmt.Errorf("invalid hint type: %s", th.Type)
	}
	if len(th.Enum) > 0 && (th.Items != nil || th.Properties != nil) {
		return fmt.Errorf("hint cannot have both enum and items/properties")
	}
	*h = Hint(th)
	return nil
}

// ObjWithHints is a map of string keys to [ValueWithHint] values, which can contain hints for each value.
// When using the [HintsUnmarshaler], every `map[string]any` in the JSON will be unmarshaled into an ObjWithHints.
type ObjWithHints map[string]ValueWithHint

// ValueWithHint is a struct that contains a value and an optional hint for that value.
type ValueWithHint struct {
	Hint  *Hint
	Value any
}

// UnmarshalJSONFrom unmarshals an [ObjWithHints] from a [jsontext.Decoder].
// It expects the JSON to be an object, and will return an error if it is not.
// Keys that start with '#' are treated as hints for the corresponding key without the '#' prefix.
func (v *ObjWithHints) UnmarshalJSONFrom(d *jsontext.Decoder) error {
	if d.PeekKind() != jsontext.KindBeginObject {
		return fmt.Errorf("expected object, got %s", d.PeekKind())
	}

	if _, err := d.ReadToken(); err != nil { // consume '{'
		return fmt.Errorf("failed to read opening token: %w", err)
	}

	for d.PeekKind() != jsontext.KindEndObject {
		rawKey, err := d.ReadValue()
		if err != nil {
			return fmt.Errorf("failed to read key: %w", err)
		}
		var key string
		if err := json.Unmarshal(rawKey, &key); err != nil {
			return fmt.Errorf("failed to unmarshal key: %w", err)
		}
		if strings.HasPrefix(key, "#") {
			key = strings.TrimPrefix(key, "#")
			var h Hint
			if err := json.UnmarshalDecode(d, &h); err != nil {
				return fmt.Errorf("failed to unmarshal hint for key %s: %w", key, err)
			}
			if *v == nil {
				*v = make(ObjWithHints)
			}
			(*v)[key] = ValueWithHint{
				Hint:  &h,
				Value: (*v)[key].Value,
			}
			continue
		}

		var value any
		if err := json.UnmarshalDecode(d, &value); err != nil {
			return fmt.Errorf("failed to unmarshal value for key %s: %w", key, err)
		}
		if *v == nil {
			*v = make(ObjWithHints)
		}
		(*v)[key] = ValueWithHint{
			Hint:  (*v)[key].Hint,
			Value: value,
		}
	}
	if _, err := d.ReadToken(); err != nil { // consume '}'
		return fmt.Errorf("failed to read closing token: %w", err)
	}

	return nil
}

// HintsUnmarshaler returns a json.Unmarshalers that can unmarshal JSON into an [ObjWithHints].
// It replaces the default unmarshaler for JSON objects with a custom unmarshaler that can handle hints.
// If the JSON is not an object, it uses the default unmarshaler for the value.
func HintsUnmarshaler() *json.Unmarshalers {
	return json.UnmarshalFromFunc(func(d *jsontext.Decoder, a *any) error {
		if d.PeekKind() == jsontext.KindBeginObject {
			var obj ObjWithHints
			if err := obj.UnmarshalJSONFrom(d); err != nil {
				return fmt.Errorf("failed to unmarshal object: %w", err)
			}
			*a = obj
			return nil
		}

		type val any
		var value val
		if err := json.UnmarshalDecode(d, &value); err != nil {
			return fmt.Errorf("failed to unmarshal value: %w", err)
		}
		*a = value
		return nil
	})
}
