package schemagen_test

import (
	"os"
	"testing"

	"github.com/helmetica-framework/chrysopoeia/pkg/schemagen"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	kubeyaml "sigs.k8s.io/yaml"
)

func TestGenerateCRD_Golden(t *testing.T) {
	chartLoader, err := loader.Loader("./testdata/juiceshop-chart")
	if err != nil {
		t.Fatalf("Creating chart loader failed: %v", err)
	}
	chart, err := chartLoader.Load()
	if err != nil {
		t.Fatalf("Loading chart failed: %v", err)
	}

	crd, err := schemagen.GenerateCRD(*chart)
	if err != nil {
		t.Fatalf("GenerateCRD failed: %v", err)
	}

	yamlData, err := kubeyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("Failed to marshal CRD to YAML: %v", err)
	}

	if err := os.WriteFile("testdata/juiceshop-crd.yaml", yamlData, 0644); err != nil {
		t.Fatalf("Failed to write CRD to file: %v", err)
	}
}
