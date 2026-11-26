// Syncs the flux Go module versions with the flux kustomize deployments.
// Authoritative are the Go module versions.
package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Printf("usage: %s kustomize.yaml", os.Args[0])
	}
	kustomizefile := os.Args[1]
	raw, err := os.ReadFile(kustomizefile)
	if err != nil {
		panic(err)
	}

	mf, err := loadModuleFile()
	if err != nil {
		panic(err)
	}

	var m map[string]any
	if err := yaml.UnmarshalStrict(raw, &m); err != nil {
		panic(err)
	}
	ress, _, err := unstructured.NestedStringSlice(m, "resources")
	if err != nil {
		panic(err)
	}
	for i, res := range ress {
		u, err := url.Parse(res)
		if err != nil {
			panic(err)
		}
		if u.Host == "github.com" && strings.HasPrefix(u.Path, "/fluxcd") {
			p, _, _ := strings.Cut(u.Path, "//")
			mod := path.Join(u.Host, p, "api")
			v, ok := versionForModule(mf, mod)
			if !ok {
				panic("module " + mod + " not found in go.mod")
			}
			q := u.Query()
			q.Set("ref", v.Version)
			u.RawQuery = q.Encode()
			if ress[i] != u.String() {
				fmt.Printf("synced: %s -> %s\n", ress[i], u.String())
			}
			ress[i] = u.String()
		}
	}
	if err := unstructured.SetNestedStringSlice(m, ress, "resources"); err != nil {
		panic(err)
	}
	um, err := yaml.Marshal(m)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(kustomizefile, um, 0644); err != nil {
		panic(err)
	}
}

func versionForModule(mf *modfile.File, path string) (module.Version, bool) {
	for _, req := range mf.Require {
		if req.Mod.Path == path {
			return req.Mod, true
		}
	}
	return module.Version{}, false
}

func loadModuleFile() (*modfile.File, error) {
	mfn, err := getGoEnv("GOMOD")
	if err != nil {
		return nil, fmt.Errorf("failed to get go env GOMOD: %w", err)
	}
	if mfn == "" {
		return nil, fmt.Errorf("empty GOMOD")
	}
	rmf, err := os.ReadFile(mfn)
	if err != nil {
		return nil, fmt.Errorf("failed to read mod file: %w", err)
	}
	mf, err := modfile.Parse(mfn, rmf, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read mod file: %w", err)
	}
	return mf, nil
}

func getGoEnv(key string) (string, error) {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
