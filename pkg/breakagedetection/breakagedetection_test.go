package breakagedetection_test

import (
	"os"
	"strings"
	"testing"

	"github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestCheck(t *testing.T) {
	originalCRDYAML, err := os.ReadFile("testdata/crd.orig.yaml")
	if err != nil {
		t.Fatalf("Failed to read original CRD YAML: %v", err)
	}
	updatedCRDYAML, err := os.ReadFile("testdata/crd.updated.yaml")
	if err != nil {
		t.Fatalf("Failed to read updated CRD YAML: %v", err)
	}

	var original, updated apiextv1.CustomResourceDefinition
	if err := yaml.Unmarshal(originalCRDYAML, &original); err != nil {
		t.Fatalf("Failed to unmarshal original CRD YAML: %v", err)
	}
	if err := yaml.Unmarshal(updatedCRDYAML, &updated); err != nil {
		t.Fatalf("Failed to unmarshal updated CRD YAML: %v", err)
	}

	t.Run("Original to Updated", func(t *testing.T) {
		warnings, errors := breakagedetection.Check(original, updated)
		if len(errors) > 0 {
			t.Errorf("Breakage detection errors: \n%s", strings.Join(errors, "\n"))
		}
		if len(warnings) > 0 {
			t.Errorf("Breakage detection warnings: \n%s", strings.Join(warnings, "\n"))
		}
	})

	t.Run("Updated to Original", func(t *testing.T) {
		warnings, errors := breakagedetection.Check(updated, original)
		if len(errors) > 0 {
			t.Errorf("Breakage detection errors: \n%s", strings.Join(errors, "\n"))
		}
		if len(warnings) > 0 {
			t.Errorf("Breakage detection warnings: \n%s", strings.Join(warnings, "\n"))
		}
	})
}
