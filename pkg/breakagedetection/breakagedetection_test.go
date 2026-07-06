package breakagedetection_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection"
	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
)

func TestCheck(t *testing.T) {
	original := crdFromChart(t, "juiceshop-original")
	updated := crdFromChart(t, "juiceshop-updated")

	debugPrintCRD(t, original, updated)

	t.Run("Original to Updated", func(t *testing.T) {
		warnings, errors := breakagedetection.Check(original, updated)
		assert.ElementsMatch(t, []string{
			"spec.values.config.server.port: Type has changed from integer to string",
			"spec.values.inArray.[0].value: Type has changed from integer to string",
			"spec.values.removedKey: Property has been removed",
		}, errors)
		assert.Empty(t, warnings)
	})

	t.Run("Updated to Original", func(t *testing.T) {
		warnings, errors := breakagedetection.Check(updated, original)
		assert.Equal(t, []string{
			"spec.values.config.server.port: Type has changed from string to integer",
			"spec.values.emptyArrayWithAddedType: Property has been removed",
			"spec.values.inArray.[0].value: Type has changed from string to integer",
		}, errors)
		assert.Empty(t, warnings)
	})
}

func crdFromChart(t *testing.T, name string) apiextv1.CustomResourceDefinition {
	loader, err := loader.Loader(filepath.Join(".", "testdata", name))
	if err != nil {
		t.Fatalf("Failed to create loader for chart %s: %v", name, err)
	}
	chart, err := loader.Load()
	if err != nil {
		t.Fatalf("Failed to load chart %s: %v", name, err)
	}
	crd, err := schemagen.GenerateCRD(*chart)
	if err != nil {
		t.Fatalf("Failed to generate CRD from chart %s: %v", name, err)
	}
	return crd
}

func debugPrintCRD(t *testing.T, orig, updated apiextv1.CustomResourceDefinition) {
	debugPrint, err := json.Marshal(map[string]apiextv1.CustomResourceDefinition{
		"original": orig,
		"updated":  updated,
	})
	if err != nil {
		t.Fatalf("Failed to marshal debug print: %v", err)
	}
	t.Logf("CRD Debug print: %s", debugPrint)
}
