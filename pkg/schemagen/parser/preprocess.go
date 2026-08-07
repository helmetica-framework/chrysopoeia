package parser

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

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
