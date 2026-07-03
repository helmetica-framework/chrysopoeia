package breakagedetection

import (
	"encoding/json"
	"slices"

	"gomodules.xyz/jsonpatch/v3"
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
	os, oerr := json.Marshal(original)
	if oerr != nil {
		errors = append(errors, "Failed to marshal original schema: "+oerr.Error())
	}
	us, uerr := json.Marshal(updated)
	if uerr != nil {
		errors = append(errors, "Failed to marshal updated schema: "+uerr.Error())
	}
	if len(errors) > 0 {
		return warnings, errors
	}

	jsonpatch, err := jsonpatch.CreatePatch(os, us)
	if err != nil {
		errors = append(errors, "Failed to create JSON patch: "+err.Error())
		return warnings, errors
	}

	for _, patch := range jsonpatch {
		switch patch.Operation {
		case "add":
		case "remove":
			errors = append(errors, "Schema has removed a field: "+patch.Path)
		case "replace":
			errors = append(errors, "Schema has replaced a field: "+patch.Path)
		default:
			errors = append(errors, "Schema has an unknown change: "+patch.Operation+" "+patch.Path)
		}
	}

	return warnings, errors
}
