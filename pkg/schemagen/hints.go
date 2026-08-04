package schemagen

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
	"k8s.io/apimachinery/pkg/util/sets"
	kubeyaml "sigs.k8s.io/yaml"
)

type hints struct {
	Children map[string]hints `json:"children"`
	Hints    map[string]hint  `json:"hint,omitempty"`
}

// PreprocessYAMLWithHints preprocesses the given YAML input, extracting the [hint.Description] from the comment on the node and add them to the hint.
func PreprocessYAMLHints(yamlData []byte) ([]byte, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(yamlData, &node); err != nil {
		return nil, err
	}

	// A chart with nothing to configure, like a chart shipping only CRDs, has an empty or comment-only
	// values.yaml. There are no hints to extract from it, and valuesSchema turns it into an empty
	// schema, so it is passed through rather than rejected.
	if len(node.Content) == 0 {
		return yamlData, nil
	}
	if node.Kind != yaml.DocumentNode {
		return nil, fmt.Errorf("invalid YAML document")
	}
	processedNodes := processNode(node.Content[0])

	modifiedYAML, err := yaml.Marshal(processedNodes)
	if err != nil {
		return nil, err
	}

	return modifiedYAML, nil
}

func processNode(node *yaml.Node) *yaml.Node {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]

			if keyNode.HeadComment != "" {
				hintKey := strings.TrimSpace(strings.TrimPrefix(keyNode.Value, "#"))
				hintDescription := stripComment(keyNode.HeadComment)

				hintNodeIdx := slices.IndexFunc(node.Content, func(n *yaml.Node) bool {
					return n.Kind == yaml.ScalarNode && n.Value == "#"+hintKey
				})

				if hintNodeIdx != -1 {
					// If a hint node already exists, update its description
					hintNode := node.Content[hintNodeIdx+1]
					if hintNode.Kind == yaml.MappingNode {
						descriptionNodeIdx := slices.IndexFunc(hintNode.Content, func(n *yaml.Node) bool {
							return n.Kind == yaml.ScalarNode && n.Value == "description"
						})
						if descriptionNodeIdx != -1 {
							hintNode.Content[descriptionNodeIdx+1].Value = hintDescription
						} else {
							// Add a new description node if it doesn't exist
							hintNode.Content = append(hintNode.Content, &yaml.Node{
								Kind:  yaml.ScalarNode,
								Tag:   "!!str",
								Value: "description",
							}, &yaml.Node{
								Kind:  yaml.ScalarNode,
								Tag:   "!!str",
								Value: hintDescription,
							})
						}
					}
				} else {
					// Create a new hint node and add it to the mapping
					hintNode := &yaml.Node{
						Kind:  yaml.MappingNode,
						Tag:   "!!map",
						Value: "",
						Content: []*yaml.Node{
							{
								Kind:  yaml.ScalarNode,
								Tag:   "!!str",
								Value: "description",
							},
							{
								Kind:  yaml.ScalarNode,
								Tag:   "!!str",
								Value: hintDescription,
							},
						},
					}

					// Add the hint node to the mapping with the hint key
					node.Content = append(node.Content, &yaml.Node{
						Kind:  yaml.ScalarNode,
						Tag:   "!!str",
						Value: "#" + hintKey,
					}, hintNode)
				}
			}

			// Recursively process the value node
			processNode(valueNode)
		}
	case yaml.SequenceNode:
		for _, itemNode := range node.Content {
			processNode(itemNode)
		}
	}

	return node
}

type hint struct {
	Description string `json:"description,omitempty"`

	Type string `json:"type"`

	Export *bool `json:"export,omitempty"`
}

var validTypes = sets.New("boolean", "integer", "number", "string")

func (h *hint) UnmarshalJSONFrom(d *jsontext.Decoder) error {
	type noValidation hint
	var th noValidation
	if err := json.UnmarshalDecode(d, &th); err != nil {
		return fmt.Errorf("failed to unmarshal hint: %w", err)
	}
	if th.Type != "" && !validTypes.Has(th.Type) {
		return fmt.Errorf("invalid hint type: %s", th.Type)
	}
	*h = hint(th)
	return nil
}

func (t *hints) UnmarshalJSONFrom(d *jsontext.Decoder) error {
	switch d.PeekKind() {
	case jsontext.KindBeginObject:
		if _, err := d.ReadToken(); err != nil { // consume '{'
			return fmt.Errorf("failed to read opening token: %w", err)
		}
		t.Children = make(map[string]hints)
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
				var h hint
				if err := json.UnmarshalDecode(d, &h); err != nil {
					return fmt.Errorf("failed to unmarshal hint for key %s: %w", key, err)
				}
				if t.Hints == nil {
					t.Hints = make(map[string]hint)
				}
				t.Hints[strings.TrimPrefix(key, "#")] = h
				continue
			}
			var child hints
			if err := child.UnmarshalJSONFrom(d); err != nil {
				return fmt.Errorf("failed to unmarshal child for key %s: %w", key, err)
			}
			t.Children[key] = child
		}
		if _, err := d.ReadToken(); err != nil { // consume '}'
			return fmt.Errorf("failed to read closing token: %w", err)
		}
		return nil
	case jsontext.KindBeginArray:
		if _, err := d.ReadToken(); err != nil { // consume '['
			return fmt.Errorf("failed to read opening token: %w", err)
		}
		for i := 0; d.PeekKind() != jsontext.KindEndArray; i++ {
			var child hints
			if err := child.UnmarshalJSONFrom(d); err != nil {
				return fmt.Errorf("failed to unmarshal child in array: %w", err)
			}
			if t.Children == nil {
				t.Children = make(map[string]hints)
			}
			t.Children[fmt.Sprintf("%d", i)] = child
		}
		if _, err := d.ReadToken(); err != nil { // consume ']'
			return fmt.Errorf("failed to read closing token: %w", err)
		}
		return nil
	}

	return d.SkipValue()
}

func (t *hints) walk(path []string, f func(path []string, h *hint)) {
	for key, h := range t.Hints {
		f(append(path, key), &h)
	}
	for key, child := range t.Children {
		child.walk(append(path, key), f)
	}
}

// export checks if the given path is marked as exported in the hints map.
// It returns true if the path is exported, false otherwise.
// The export property is inherited from parent paths if not explicitly set on the current path.
func exported(path string, hints map[string]hint, defaultValue bool) bool {
	if h, ok := hints[path]; ok && h.Export != nil {
		return *h.Export
	}
	// If the current path is not explicitly marked, check parent paths.
	for {
		lastSlash := strings.LastIndex(path, "/")
		if lastSlash == -1 {
			break
		}
		path = path[:lastSlash]
		if h, ok := hints[path]; ok && h.Export != nil {
			return *h.Export
		}
	}

	return defaultValue
}

func collectHints(rawValues []byte) (map[string]hint, error) {
	jsonData, err := kubeyaml.YAMLToJSONStrict(rawValues)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}

	var root hints
	if err := json.Unmarshal(jsonData, &root, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	paths := make(map[string]hint)
	root.walk([]string{}, func(path []string, h *hint) {
		paths[jsonpointer(path)] = *h
	})

	return paths, nil
}

func jsonpointer(path []string) string {
	escaped := make([]string, len(path))
	for i, p := range path {
		escaped[i] = escapeJSONPointer(p)
	}
	return "/" + strings.Join(escaped, "/")
}

func escapeJSONPointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
