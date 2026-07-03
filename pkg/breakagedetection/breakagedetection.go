package breakagedetection

import (
	"fmt"
	"slices"
	"strings"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Check compares two [apiextv1.CustomResourceDefinition]s and returns a list of warnings and errors.
// Warnings indicate potential issues that may arise from the update, but do not prevent the update from being applied.
// Errors indicate breaking changes that will prevent the update from being applied.
func Check(original, updated apiextv1.CustomResourceDefinition) (warnings []string, errors []string) {
	if original.Spec.Group != updated.Spec.Group {
		errors = append(errors, "Group has changed")
	}
	if original.Spec.Names.Kind != updated.Spec.Names.Kind {
		errors = append(errors, "Kind has changed")
	}
	if original.Spec.Names.ListKind != updated.Spec.Names.ListKind {
		errors = append(errors, "ListKind has changed")
	}
	if original.Spec.Names.Plural != updated.Spec.Names.Plural {
		errors = append(errors, "Plural has changed")
	}
	if original.Spec.Names.Singular != updated.Spec.Names.Singular {
		errors = append(errors, "Singular has changed")
	}
	if original.Spec.Scope != updated.Spec.Scope {
		errors = append(errors, "Scope has changed")
	}
	if !slices.EqualFunc(original.Spec.Versions, updated.Spec.Versions, func(a, b apiextv1.CustomResourceDefinitionVersion) bool {
		return a.Name == b.Name && a.Storage == b.Storage && a.Served == b.Served
	}) {
		errors = append(errors, "Versions have changed")
	}
	// Checkpoint: We can't compare the schema because it might have changed in a non-defined way, return early
	if len(errors) > 0 {
		return warnings, errors
	}

	for i, originalVersion := range original.Spec.Versions {
		updatedVersion := updated.Spec.Versions[i]
		scw, sce := checkSchema(originalVersion.Schema, updatedVersion.Schema)
		warnings = append(warnings, scw...)
		errors = append(errors, sce...)
	}

	return warnings, errors
}

func checkSchema(original, updated *apiextv1.CustomResourceValidation) (warnings []string, errors []string) {
	if original == nil && updated == nil {
		return warnings, errors
	}
	if original == nil && updated != nil {
		return warnings, errors
	}
	if original != nil && updated == nil {
		errors = append(errors, "Schema has been removed")
		return warnings, errors
	}

	return checkSchemaProperties([]string{}, *original.OpenAPIV3Schema, *updated.OpenAPIV3Schema)
}

func checkSchemaProperties(path []string, original, updated apiextv1.JSONSchemaProps) (warnings []string, errors []string) {
	if original.Type != updated.Type {
		errors = append(errors, fmt.Sprintf("%s: Type has changed from %s to %s", joinPath(path), original.Type, updated.Type))
		return warnings, errors
	}
	for propName, prop := range original.Properties {
		updatedProp, ok := updated.Properties[propName]
		if !ok {
			errors = append(errors, fmt.Sprintf("%s: Property has been removed", joinPath(append(path, propName))))
			continue
		}
		scw, sce := checkSchemaProperties(append(path, propName), prop, updatedProp)
		warnings = append(warnings, scw...)
		errors = append(errors, sce...)
	}
	var origItems []apiextv1.JSONSchemaProps
	if original.Items != nil {
		if original.Items.Schema != nil {
			origItems = append(origItems, *original.Items.Schema)
		}
		origItems = append(origItems, original.Items.JSONSchemas...)
	}
	var updatedItems []apiextv1.JSONSchemaProps
	if updated.Items != nil {
		if updated.Items.Schema != nil {
			updatedItems = append(updatedItems, *updated.Items.Schema)
		}
		updatedItems = append(updatedItems, updated.Items.JSONSchemas...)
	}
	if len(origItems) != len(updatedItems) {
		errors = append(errors, fmt.Sprintf("%s: Number of items has changed from %d to %d", joinPath(path), len(origItems), len(updatedItems)))
		return warnings, errors
	}

	for i, item := range origItems {
		updatedItem := updatedItems[i]
		scw, sce := checkSchemaProperties(append(path, fmt.Sprintf("[%d]", i)), item, updatedItem)
		warnings = append(warnings, scw...)
		errors = append(errors, sce...)
	}

	return warnings, errors
}

func joinPath(path []string) string {
	return strings.Join(path, ".")
}
