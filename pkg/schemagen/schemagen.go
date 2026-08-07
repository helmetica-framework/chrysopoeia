package schemagen

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"

	"github.com/Masterminds/semver/v3"
	parser "github.com/helmetica-framework/chrysopoeia/pkg/schemagen/parser"
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
// The generated CRD has a single version named "bundle", which is served and stored.
//
// Untagged null values are skipped. Empty objects and arrays are skipped unless they have hints that specify their type.
// You can use hints in the values.yaml to specify the type of a value, or to mark it as exported or not exported.
// Hints are specified as a sibling key with a '#' prefix, e.g.:
//
//	foo:
//	'#foo':
//	  type: string
//
// Allowed types for hints are "boolean", "integer", "number", and "string".
//
// Empty objects and arrays can be annotated with hints to specify their type, e.g.:
//
//	stringArray: []
//	'#stringArray':
//	  items:
//	    '#':
//	      type: string
//	# Equal to [""]
//
//	objArray: []
//	'#objArray':
//	  items:
//	    '#name':
//	      type: string
//	# Equal to [{"name": ""}]
//
//	obj: {}
//	'#obj':
//	  properties:
//	    '#name':
//	      type: string
//	# Equal to {"name": ""}
//
//	stringmap: {}
//	'#stringmap':
//	  items:
//	    '#':
//	      type: string
//	# Equal to {"key": "value"}
//
//	objmap: {}
//	'#objmap':
//	  items:
//	    '#name':
//	      type: string
//	# Equal to {"key": {"name": ""}}
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

	valuesYaml, err := parser.PreprocessYAML(valuesYaml)
	if err != nil {
		return apiextv1.CustomResourceDefinition{}, fmt.Errorf("Failed to pre-process YAML: %w", err)
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

func valuesSchema(rawValues []byte) (apiextv1.JSONSchemaProps, error) {
	jsonData, err := kubeyaml.YAMLToJSONStrict(rawValues)
	if err != nil {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}
	var root any
	if err := json.Unmarshal(jsonData, &root, json.WithUnmarshalers(parser.HintsUnmarshaler())); err != nil {
		return apiextv1.JSONSchemaProps{}, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}
	// collects all hints from the root node and its children into a map of jsonpointer -> hint
	// Used to infer visibility of fields in the schema generation step.
	hints := make(map[string]parser.Hint)
	if err := collectHints(hints, []string{}, root); err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}

	schemaProps, err := convertJSONNodeToJSONSchema(root, parser.Hint{}, []string{}, hints)
	if err != nil {
		return apiextv1.JSONSchemaProps{}, err
	}
	return ptr.Deref(schemaProps, apiextv1.JSONSchemaProps{}), nil
}

func pop(path []string) (string, []string) {
	if len(path) == 0 {
		return "", path
	}
	return path[len(path)-1], path[:len(path)-1]
}

func convertJSONNodeToJSONSchema(node any, hint parser.Hint, path []string, hints map[string]parser.Hint) (*apiextv1.JSONSchemaProps, error) {
	switch typedNode := node.(type) {
	case map[string]any:
		panic("BUG: HintsUnmarshaler should have unmarshaled JSON objects into ObjWithHints, not map[string]any")
	case parser.ObjWithHints:
		if v, ok := typedNode[""]; len(typedNode) == 1 && ok {
			return convertJSONNodeToJSONSchema(nil, ptr.Deref(v.Hint, parser.Hint{}), append(path, "#"), hints)
		}

		if p := hint.Items; p != nil {
			lastPathElement, path := pop(path)
			rawPropsFoundAt := append(path, "#"+lastPathElement, "items")

			pj, err := convertJSONNodeToJSONSchema(p, parser.Hint{}, rawPropsFoundAt, hints)
			if err != nil {
				return nil, fmt.Errorf("error converting to schema at path %q: %w", jsonpointer(rawPropsFoundAt), err)
			}
			return &apiextv1.JSONSchemaProps{
				Description: hint.Description,
				Type:        "object",
				AdditionalProperties: &apiextv1.JSONSchemaPropsOrBool{
					Schema: pj,
				},
			}, nil
		} else if p := hint.Properties; p != nil {
			lastPathElement, path := pop(path)
			rawPropsFoundAt := append(path, "#"+lastPathElement, "properties")

			schema, err := convertJSONNodeToJSONSchema(p, parser.Hint{Description: hint.Description}, rawPropsFoundAt, hints)
			if err != nil {
				return nil, fmt.Errorf("error converting to schema at path %q: %w", jsonpointer(rawPropsFoundAt), err)
			}
			if schema.Type != "object" {
				return nil, fmt.Errorf("expected object type for properties at path %q, got %q", jsonpointer(rawPropsFoundAt), schema.Type)
			}
			return schema, nil
		}

		var props = make(map[string]apiextv1.JSONSchemaProps)
		for k, v := range typedNode {
			if strings.HasPrefix(k, "#") {
				continue
			}
			vs, err := convertJSONNodeToJSONSchema(v.Value, ptr.Deref(v.Hint, parser.Hint{}), append(path, k), hints)
			if err != nil {
				return nil, fmt.Errorf("error converting to schema at path %q: %w", jsonpointer(append(path, k)), err)
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
		if p := hint.Items; p != nil {
			lastPathElement, path := pop(path)
			rawItemsFoundAt = append(path, "#"+lastPathElement, "items")
			rawItems = p
		} else if len(typedNode) > 0 {
			rawItems = typedNode[0]
			rawItemsFoundAt = append(path, "0")
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping empty array with non-discoverable item type at %s.\n", jsonpointer(rawItemsFoundAt))
			return nil, nil
		}

		if itms, err := convertJSONNodeToJSONSchema(rawItems, parser.Hint{}, rawItemsFoundAt, hints); err != nil {
			return nil, fmt.Errorf("error converting to schema at path %q: %w", jsonpointer(rawItemsFoundAt), err)
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
	default:
		var typ string
		if hint.Type != "" {
			typ = hint.Type
		} else if t := guessTypeFromValue(typedNode); t != "" {
			typ = t
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: Skipping key with non-discoverable type at %s, use hints {'#KEY': {'type': 'your_type'}} to specify the type.\n", strings.Join(path, "/"))
			return nil, nil
		}

		if exported := exported(jsonpointer(path), hints, true); !exported {
			return nil, nil
		}

		enums := make([]apiextv1.JSON, len(hint.Enum))
		for i, e := range hint.Enum {
			ej, err := json.Marshal(e)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal enum value at path %q: %w", jsonpointer(append(path, fmt.Sprintf("enum[%d]", i))), err)
			}
			enums[i] = apiextv1.JSON{Raw: ej}
		}

		return &apiextv1.JSONSchemaProps{
			Description: hint.Description,
			Type:        typ,
			Enum:        enums,
		}, nil
	}
}

func guessTypeFromValue(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case float64:
		return "integer"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return ""
	}
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

// export checks if the given path is marked as exported in the hints map.
// It returns true if the path is exported, false otherwise.
// The export property is inherited from parent paths if not explicitly set on the current path.
func exported(path string, hints map[string]parser.Hint, defaultValue bool) bool {
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

func collectHints(hints map[string]parser.Hint, path []string, obj any) error {
	switch typedObj := obj.(type) {
	case []any:
		for i, v := range typedObj {
			if err := collectHints(hints, append(path, fmt.Sprintf("%d", i)), v); err != nil {
				return err
			}
		}
	case parser.ObjWithHints:
		for k, v := range typedObj {
			if err := collectHints(hints, append(path, k), v.Value); err != nil {
				return err
			}
			if h := v.Hint; h != nil {
				hints[jsonpointer(append(path, k))] = *h

				path := append(path, "#"+k)
				if h.Items != nil {
					path = append(path, "items")
					collectHints(hints, path, h.Items)
				}
				if h.Properties != nil {
					path = append(path, "properties")
					collectHints(hints, path, h.Properties)
				}
			}
		}
	}

	return nil
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
