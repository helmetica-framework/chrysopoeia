package schemagen

import (
	"fmt"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/utils/ptr"
	kubeyaml "sigs.k8s.io/yaml"
)

const CRDKindAnnotation = "crd.bundle.appcat.io/kind"
const CRDListKindAnnotation = "crd.bundle.appcat.io/listKind"
const CRDSingularAnnotation = "crd.bundle.appcat.io/singular"
const CRDPluralAnnotation = "crd.bundle.appcat.io/plural"

var crdCategories = []string{"all", "claim", "helmetica"}

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

	valuesYaml, err := PreprocessYAMLHints(valuesYaml)
	if err != nil {
		return apiextv1.CustomResourceDefinition{}, fmt.Errorf("Failed to pre-process YAML: %w", err)
	}

	hints, err := collectHints(valuesYaml)
	if err != nil {
		return apiextv1.CustomResourceDefinition{}, err
	}

	schema, err := valuesSchema(valuesYaml, hints)
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
	crd.Spec.Names.Categories = append(crd.Spec.Names.Categories, crdCategories...)

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
			Subresources: &apiextv1.CustomResourceSubresources{
				Status: &apiextv1.CustomResourceSubresourceStatus{},
			},
			AdditionalPrinterColumns: []apiextv1.CustomResourceColumnDefinition{
				{
					Name:     "Instance Namespace",
					Type:     "string",
					JSONPath: ".status.instanceNamespace",
				},
				{
					Name:     "Status",
					Type:     "string",
					JSONPath: ".status.releaseStatus",
				},
				{
					Name:     "Drift",
					Type:     "boolean",
					JSONPath: ".status.driftDetected",
				},
				{
					Name:     "Age",
					Type:     "date",
					JSONPath: ".metadata.creationTimestamp",
				},
			},
			Schema: &apiextv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextv1.JSONSchemaProps{
						"spec": {
							Type:        "object",
							Description: "Configures the desired state of the service.",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"approval": {
									Type:        "object",
									Description: "Approval contains the approval strategy for the service.",
									Default:     &apiextv1.JSON{Raw: []byte(`{"strategy":"Automatic"}`)},
									Properties: map[string]apiextv1.JSONSchemaProps{
										"strategy": {
											Type:        "string",
											Description: "The approval strategy for the service. Can be either 'Automatic' or 'Manual'.",
											Enum:        []apiextv1.JSON{{Raw: []byte(`"Automatic"`)}, {Raw: []byte(`"Manual"`)}},
											Default:     &apiextv1.JSON{Raw: []byte(`"Automatic"`)},
										},
									},
								},
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
								"provides": dependencyReferences("The list of dependency groups whose CRDs this service ships."),
								"manages":  dependencyReferences("The dependency group whose CRDs the operator of this service manages."),
								"requires": dependencyReferences("The list of dependency groups that this service consumes."),
							},
						},
						"status": {
							Type:        "object",
							Description: "Status contains the observed state of the service.",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"latestRevision": {
									Type:        "string",
									Description: "The name of the revision that currently matches the spec.",
								},
								"appliedRevision": {
									Type:        "string",
									Description: "The name of the revision that is currently applied to the cluster.",
								},
								"releaseStatus": {
									Type:        "string",
									Description: "The current status of the service.",
								},
								"driftDetected": {
									Type:        "boolean",
									Description: "Whether a drift was detected.",
								},
								"instanceNamespace": {
									Type:        "string",
									Description: "The namespace where the service is deployed in.",
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

func valuesSchema(rawValues []byte, hints map[string]hint) (apiextv1.JSONSchemaProps, error) {
	var root map[string]any
	if err := kubeyaml.UnmarshalStrict(rawValues, &root); err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}

	schemaProps, err := convertJSONNodeTOJSONSchema(root, nil, []string{}, hints)
	if err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}
	return ptr.Deref(schemaProps, apiextv1.JSONSchemaProps{}), nil
}

func convertJSONNodeTOJSONSchema(node, parent any, path []string, hints map[string]hint) (*apiextv1.JSONSchemaProps, error) {
	hint := hints[jsonpointer(path)]
	var lastPathElement string
	if len(path) > 0 {
		lastPathElement = path[len(path)-1]
	}

	switch typedNode := node.(type) {
	case map[string]any:
		props := make(map[string]apiextv1.JSONSchemaProps)

		var rawProps map[string]any
		var rawPropsFoundAt []string
		if p := findPropertiesKeyForObject(parent, lastPathElement); p != nil {
			rawProps = p
			rawPropsFoundAt = append(path[:len(path)-1], "#"+lastPathElement, "properties")
		} else {
			rawProps = typedNode
			rawPropsFoundAt = path
		}

		for k, v := range rawProps {
			if strings.HasPrefix(k, "#") {
				continue
			}
			vs, err := convertJSONNodeTOJSONSchema(v, typedNode, append(rawPropsFoundAt, k), hints)
			if err != nil {
				return nil, fmt.Errorf("error converting to schema at path %q: %w", strings.Join(append(rawPropsFoundAt, k), "/"), err)
			}
			if vs != nil {
				props[k] = *vs
			}
		}

		if len(props) == 0 {
			return nil, nil
		} else {
			return &apiextv1.JSONSchemaProps{
				Description: hint.Description,
				Type:        "object",
				Properties:  props,
			}, nil
		}

	case []any:
		var items *apiextv1.JSONSchemaProps

		var rawItems any
		var rawItemsFoundAt []string
		if itms := findItemsKeyForArray(parent, lastPathElement); itms != nil {
			rawItems = itms
			rawItemsFoundAt = append(path[:len(path)-1], "#"+lastPathElement, "items")
		} else if len(typedNode) > 0 {
			rawItems = typedNode[0]
			rawItemsFoundAt = append(path, "0")
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping empty array with non-discoverable item type at %s.\n", strings.Join(path, "/"))
			return nil, nil
		}

		if itms, err := convertJSONNodeTOJSONSchema(rawItems, typedNode, rawItemsFoundAt, hints); err != nil {
			return nil, fmt.Errorf("error converting to schema at path %q: %w", strings.Join(rawItemsFoundAt, "/"), err)
		} else {
			items = itms
		}

		if items == nil {
			return nil, nil
		} else {
			return &apiextv1.JSONSchemaProps{
				Description: hint.Description,
				Type:        "array",
				Items: &apiextv1.JSONSchemaPropsOrArray{
					Schema: items,
				},
			}, nil
		}
	case string:
		if exported := exported(jsonpointer(path), hints, true); !exported {
			return nil, nil
		}

		return &apiextv1.JSONSchemaProps{
			Description: hint.Description,
			Type:        "string",
		}, nil
	case float64:
		if exported := exported(jsonpointer(path), hints, true); !exported {
			return nil, nil
		}

		return &apiextv1.JSONSchemaProps{
			Description: hint.Description,
			Type:        "integer",
		}, nil
	case bool:
		if exported := exported(jsonpointer(path), hints, true); !exported {
			return nil, nil
		}

		return &apiextv1.JSONSchemaProps{
			Description: hint.Description,
			Type:        "boolean",
		}, nil
	case nil:
		if exported := exported(jsonpointer(path), hints, true); !exported {
			return nil, nil
		}

		if hint.Type == "" {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping key with non-discoverable type at %s, use hints {'#KEY': {'type': 'your_type'}} to specify the type.\n", strings.Join(path, "/"))
			return nil, nil
		}

		return &apiextv1.JSONSchemaProps{
			Description: hint.Description,
			Type:        hint.Type,
		}, nil
	default:
		return nil, fmt.Errorf("unknown type: %T", node)
	}
}

// findItemsKeyForArray returns the value of the "items" key for an array, if it exists.
// It looks for a hint in the parent map with the key "#<lastPathElement>" and
// returns the value of its "items" key if found. If not found, it returns nil.
func findItemsKeyForArray(parent any, lastPathElement string) any {
	tp, ok := parent.(map[string]any)
	if !ok {
		return nil
	}
	hm, ok := tp["#"+lastPathElement]
	if !ok {
		return nil
	}
	if hmMap, ok := hm.(map[string]any); ok {
		if items, ok := hmMap["items"]; ok {
			return items
		}
	}
	return nil
}

// findPropertiesKeyForArray returns the value of the "properties" key for an object, if it exists.
// It looks for a hint in the parent map with the key "#<lastPathElement>" and
// returns the value of its "properties" key if found. If not found, it returns nil.
func findPropertiesKeyForObject(parent any, lastPathElement string) map[string]any {
	tp, ok := parent.(map[string]any)
	if !ok {
		return nil
	}
	hm, ok := tp["#"+lastPathElement]
	if !ok {
		return nil
	}
	if hmMap, ok := hm.(map[string]any); ok {
		if properties, ok := hmMap["properties"]; ok {
			if propsMap, ok := properties.(map[string]any); ok {
				return propsMap
			}
		}
	}
	return nil
}

// dependencyReferences is the schema of a list of references to a DependencyGroup. The scope the
// group is used under, and with it the label the harness proxy scopes the operator to, is the
// group's name or, if set, its alias.
func dependencyReferences(description string) apiextv1.JSONSchemaProps {
	return apiextv1.JSONSchemaProps{
		Type:        "array",
		Description: description,
		Items: &apiextv1.JSONSchemaPropsOrArray{
			Schema: &apiextv1.JSONSchemaProps{
				Type:     "object",
				Required: []string{"dependencyGroup"},
				Properties: map[string]apiextv1.JSONSchemaProps{
					"dependencyGroup": {
						Type:     "object",
						Required: []string{"name"},
						Properties: map[string]apiextv1.JSONSchemaProps{
							"name": {
								Type:        "string",
								Description: "The name of the DependencyGroup.",
							},
							"as": {
								Type:        "string",
								Description: "Overrides the name the scope label is built from, so that a second deployment of the same operator can serve a separate set of consumers.",
							},
						},
					},
				},
			},
		},
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
