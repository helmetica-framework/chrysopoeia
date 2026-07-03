package breakagedetection_test

import (
	"path/filepath"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/v2/loader"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection"
	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
)

func TestCheck(t *testing.T) {
	original := crdFromChart(t, "juiceshop-original")
	updated := crdFromChart(t, "juiceshop-updated")

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
