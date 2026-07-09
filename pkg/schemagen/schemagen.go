package schemagen

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"go.yaml.in/yaml/v4"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/registry"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const CRDKindAnnotation = "crd.bundle.appcat.io/kind"
const CRDListKindAnnotation = "crd.bundle.appcat.io/listKind"
const CRDSingularAnnotation = "crd.bundle.appcat.io/singular"
const CRDPluralAnnotation = "crd.bundle.appcat.io/plural"

// GenerateCRD generates a [apiextv1.CustomResourceDefinition] from a Helm chart.
// The CRD is generated based on the chart's values.yaml file and annotations.
//
// Use the following annotations in the chart's metadata to customize the CRD:
//
//   - [CRDKindAnnotation]: The kind of the CRD. Defaults to "Instance".
//   - [CRDListKindAnnotation]: The list kind of the CRD. Defaults to empty.
//   - [CRDSingularAnnotation]: The singular name of the CRD. Defaults to empty.
//   - [CRDPluralAnnotation]: The plural name of the CRD. Defaults to lowercase kind + "s".
//
// The generated CRD is namespace-scoped.
// The CRD's group is derived from the chart's version and name, in the format "v<major>.<chart-name>.bundles.appcat.io".
//
// Warning: Currently all untagged null values in the values.yaml file are assumed to be strings.
// This may lead to incorrect schema generation for fields that are actually of a different type.
func GenerateCRD(chart chartv2.Chart, opts ...GenerateOption) (apiextv1.CustomResourceDefinition, error) {
	o := &generateOptions{}
	for _, opt := range opts {
		opt(o)
	}

	var valuesYaml []byte
	for _, f := range chart.Raw {
		if f.Name == "values.yaml" {
			valuesYaml = f.Data
			break
		}
	}
	if valuesYaml == nil {
		return apiextv1.CustomResourceDefinition{}, fmt.Errorf("values.yaml not found in chart")
	}

	schema, err := valuesSchema(valuesYaml)
	if err != nil {
		return apiextv1.CustomResourceDefinition{}, err
	}

	var crd apiextv1.CustomResourceDefinition
	crd.SetGroupVersionKind(apiextv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))

	crd.Spec.Names = o.names
	if crd.Spec.Names.Kind == "" || crd.Spec.Names.Plural == "" {
		names, err := names(chart)
		if err != nil {
			return apiextv1.CustomResourceDefinition{}, err
		}
		crd.Spec.Names = names
	}

	crd.Spec.Group = o.group
	if crd.Spec.Group == "" {
		// https://github.com/helm/helm/blob/af25d22902ef9fdbf7c667f3a0744a8f5a9a8fc3/pkg/registry/client.go#L800
		semver, err := semver.StrictNewVersion(strings.ReplaceAll(chart.Metadata.Version, "_", "+"))
		if err != nil {
			return apiextv1.CustomResourceDefinition{}, fmt.Errorf("invalid strict chart version: %s", chart.Metadata.Version)
		}
		crd.Spec.Group = fmt.Sprintf("v%d.%s.bundles.appcat.io", semver.Major(), chart.Name())
	}

	crd.Name = fmt.Sprintf("%s.%s", crd.Spec.Names.Plural, crd.Spec.Group)
	crd.Spec.Scope = apiextv1.NamespaceScoped
	crd.Spec.Versions = []apiextv1.CustomResourceDefinitionVersion{
		{
			Name:    "bundle",
			Served:  true,
			Storage: true,
			Schema: &apiextv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextv1.JSONSchemaProps{
						"spec": {
							Type:        "object",
							Description: "Configures the desired state of the service.",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"version": {
									Type:        "string",
									Description: "The version of the service. Every change to this field together with the `.spec.values` field creates a new revision of the service.",
								},
								"values": {
									Type:        "object",
									Description: "This field together with the `.spec.version` field defines the configuration of the service. Every change to either of these two fields creates a new revision of the service.",
									Properties:  schema.Properties,
								},
								"ociUrl": {
									Type:        "string",
									Description: "The OCI repository where the service bundle is stored.",
								},
							},
						},
					},
				},
			},
		},
	}

	return crd, nil
}

func names(chart chartv2.Chart) (apiextv1.CustomResourceDefinitionNames, error) {
	kind := chart.Metadata.Annotations[CRDKindAnnotation]
	if kind == "" {
		kind = "Instance"
	}
	plural := chart.Metadata.Annotations[CRDPluralAnnotation]
	if plural == "" {
		plural = strings.ToLower(kind) + "s"
	}

	listKind := chart.Metadata.Annotations[CRDListKindAnnotation]
	singular := chart.Metadata.Annotations[CRDSingularAnnotation]

	return apiextv1.CustomResourceDefinitionNames{
		Kind:     kind,
		ListKind: listKind,
		Plural:   plural,
		Singular: singular,
	}, nil
}

func valuesSchema(rawValues []byte) (apiextv1.JSONSchemaProps, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(rawValues, &node); err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}

	if len(node.Content) == 0 {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("empty YAML document")
	}
	if len(node.Content) > 1 {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("multiple YAML documents found")
	}
	top := node.Content[0] // Unwrap the document node
	if top.Kind != yaml.MappingNode {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("top-level YAML node is not a mapping")
	}

	schemaProps, err := convertYAMLNodeToJSONSchema(top, "")
	if err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}
	if schemaProps == nil {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("top-level YAML node returned nil schema")
	}
	return *schemaProps, nil
}

// convertYAMLNodeToJSONSchema converts a YAML node to a JSON schema.
// The path parameter is used for error reporting and should be the path to the current node in the YAML document.
// The function handles mapping nodes, sequence nodes, and scalar nodes. It also handles alias nodes by resolving them to their target node.
// For mapping nodes, it recursively converts each key-value pair to a JSON schema property.
// For sequence nodes, it converts the first element to a JSON schema item and assumes that all elements are of the same type.
// For scalar nodes, it determines the JSON schema type based on the YAML tag and returns a JSON schema with that type.
// The function can return a nil schema without an error if the input node is nil or the node type can't be determined.
// In this case, the caller should handle the nil schema appropriately.
func convertYAMLNodeToJSONSchema(node *yaml.Node, path string) (*apiextv1.JSONSchemaProps, error) {
	if node == nil {
		return nil, nil
	}

	switch node.Kind {
	case yaml.AliasNode:
		return convertYAMLNodeToJSONSchema(node.Alias, path)

	case yaml.MappingNode:
		props := make(map[string]apiextv1.JSONSchemaProps)
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			if keyNode == nil {
				continue
			}
			valueSchema, err := convertYAMLNodeToJSONSchema(valueNode, path+"."+keyNode.Value)
			if err != nil {
				return nil, fmt.Errorf("at %s: %s", path, err)
			}
			if valueSchema != nil {
				valueSchema.Description = stripComment(keyNode.HeadComment)
				props[keyNode.Value] = *valueSchema
			}
		}

		return &apiextv1.JSONSchemaProps{
			Type:       "object",
			Properties: props,
		}, nil

	case yaml.SequenceNode:
		var items *apiextv1.JSONSchemaProps
		if len(node.Content) > 0 {
			var err error
			items, err = convertYAMLNodeToJSONSchema(node.Content[0], path+"[0]")
			if err != nil {
				return nil, fmt.Errorf("at %s: %s", path, err)
			}
		}

		if items == nil {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping array with non-discoverable item type at %s.\n", path)
			return nil, nil
		}

		return &apiextv1.JSONSchemaProps{
			Type: "array",
			Items: &apiextv1.JSONSchemaPropsOrArray{
				Schema: items,
			},
		}, nil

	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping key with non-discoverable type at %s, use yaml tags (`key: !!type`) to specify the type.\n", path)
			return nil, nil
		}

		var schemaType string
		switch node.Tag {
		case "!!bool":
			schemaType = "boolean"
		case "!!int":
			schemaType = "integer"
		case "!!float":
			schemaType = "number"
		case "!!str":
			schemaType = "string"
		default:
			return nil, fmt.Errorf("unsupported YAML scalar tag: %s", node.Tag)
		}

		return &apiextv1.JSONSchemaProps{
			Type: schemaType,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported YAML node kind: %v", node.Kind)
	}
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

func downloadChart(chartRef string) (string, error) {
	c, err := registry.NewClient()
	if err != nil {
		return "", err
	}

	res, err := c.Pull(chartRef,
		registry.PullOptWithChart(true),
	)
	if err != nil {
		return "", err
	}

	cacheDir := filepath.Join(".", "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	sha := fmt.Sprintf("%x", sha256.Sum256([]byte(chartRef)))
	filePath := filepath.Join(cacheDir, sha+".tgz")
	if err := os.WriteFile(filePath, res.Chart.Data, 0644); err != nil {
		return "", err
	}

	return filePath, nil
}

type generateOptions struct {
	group string
	names apiextv1.CustomResourceDefinitionNames
}

// GenerateOption is a function that modifies the options for generating a CRD.
type GenerateOption func(*generateOptions)

// WithGroup sets the group for the generated CRD.
func WithGroup(group string) GenerateOption {
	return func(o *generateOptions) {
		o.group = group
	}
}

// WithNames sets the names for the generated CRD.
func WithNames(names apiextv1.CustomResourceDefinitionNames) GenerateOption {
	return func(o *generateOptions) {
		o.names = names
	}
}
