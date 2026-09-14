package parser

import (
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

// CelPrefix marks a value in values.yaml as a CEL expression evaluated by pkg/celvalues.
// The detection helpers below live here rather than in celvalues so that the schema generator and
// the evaluator cannot disagree about what counts as an expression. This package is the one both
// can import without pulling cel-go into the schema generator.
const CelPrefix = "cel:"

// celSpelling matches the spellings of [CelPrefix] that are not [CelPrefix]: a different case, a
// leading space, a space before the colon.
var celSpelling = regexp.MustCompile(`(?i)^\s*cel\s*:`)

// IsCelExpression reports whether value carries [CelPrefix] exactly, and is therefore computed
// rather than set by a user.
func IsCelExpression(value string) bool {
	return strings.HasPrefix(value, CelPrefix)
}

// LooksLikeCelExpression reports whether value carries [CelPrefix] in any spelling. A value that
// looks like one without being one is a chart-author mistake: celvalues rejects it rather than
// handing the magic string to Helm as a literal.
func LooksLikeCelExpression(value string) bool {
	return celSpelling.MatchString(value)
}

// PreprocessYAML preprocesses the given YAML input, extracting the [Hint.Description] from the leading comments of each key
// and adding it as a hint for the corresponding key in the output YAML.
func PreprocessYAML(yamlData []byte) ([]byte, error) {
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
	processedNodes := preprocessNode(node.Content[0])

	modifiedYAML, err := yaml.Marshal(processedNodes)
	if err != nil {
		return nil, err
	}

	return modifiedYAML, nil
}

func preprocessNode(node *yaml.Node) *yaml.Node {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]

			hintKey := "#" + strings.TrimPrefix(keyNode.Value, "#")

			// Add description hint if present
			if keyNode.HeadComment != "" {
				hintDescription := stripComment(keyNode.HeadComment)
				hintNode := ensureHintMappingNode(hintKey, &node.Content)
				if hintNode.Kind == yaml.MappingNode {
					descriptionNode := ensureNodeAtKey(&hintNode.Content, "description", &yaml.Node{
						Kind: yaml.ScalarNode,
						Tag:  "!!str",
					})
					descriptionNode.Value = hintDescription
				}
			}

			// A cel: value is computed by the values pre-processor, never set by a user, so it must not
			// appear in the generated CRD. This overrules an explicit export hint: the field is not settable
			// no matter what the author asked for.
			if valueNode.Kind == yaml.ScalarNode && IsCelExpression(valueNode.Value) {
				hintNode := ensureHintMappingNode(hintKey, &node.Content)
				if hintNode.Kind == yaml.MappingNode {
					exportNode := ensureNodeAtKey(&hintNode.Content, "export", &yaml.Node{
						Kind: yaml.ScalarNode,
						Tag:  "!!bool",
					})
					// ensureNodeAtKey returns an existing node untouched, so the tag has to be set
					// here too and not only on the template above: an author who wrote `export: true`
					// leaves a node already tagged !!bool with the wrong value.
					exportNode.Tag = "!!bool"
					exportNode.Value = "false"
				}
			}

			preprocessNode(valueNode)
		}
	case yaml.SequenceNode:
		for _, itemNode := range node.Content {
			preprocessNode(itemNode)
		}
	}

	return node
}

// ensureHintMappingNode ensures that a mapping node for the given hint key exists in the content slice.
// If it exists, it returns the existing mapping node. If it does not exist, it creates a new mapping node,
// appends it to the content slice, and returns the newly created mapping node.
func ensureHintMappingNode(hintKey string, content *[]*yaml.Node) *yaml.Node {
	return ensureNodeAtKey(content, hintKey, &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{},
	})
}

// ensureNodeAtKey ensures that a node for the given key exists in the content slice.
// If it exists, it returns the existing node. If it does not exist, it appends the given value node
// to the content slice and returns it.
// Modifies the content slice in place.
func ensureNodeAtKey(content *[]*yaml.Node, key string, value *yaml.Node) *yaml.Node {
	for i := 0; i < len(*content); i += 2 {
		keyNode := (*content)[i]
		if keyNode.Kind == yaml.ScalarNode && keyNode.Value == key {
			return (*content)[i+1]
		}
	}
	// If the hint key does not exist, add it with an empty mapping node
	*content = append(*content, &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}, value)

	return value
}

func stripComment(s string) string {
	lines := strings.Split(s, "\n")
	strippedLines := make([]string, 0, len(lines))
	for _, line := range lines {
		strippedLine := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if strippedLine != "" {
			strippedLines = append(strippedLines, strippedLine)
		}
	}
	return strings.Join(strippedLines, "\n")
}
