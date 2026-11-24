//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	homunculusv1ac "github.com/helmetica-framework/chrysopoeia/test/e2e/testdata/testcontroller/api/v1/applyconfiguration/api/v1"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/krusty"
	kustomizeapi "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
	"sigs.k8s.io/yaml"

	chrysopoeiav1ac "github.com/helmetica-framework/chrysopoeia/api/v1/applyconfiguration/api/v1"
)

func (suite *E2ESuite) TestInstallOperatorBasedService() {
	suite.T().Log("Building test chart...")
	cmd := exec.Command(
		"helm",
		"package",
		path.Join(suite.projectDir, "test/e2e/testdata/proxychart"),
	)
	cmd.Dir = suite.buildDir
	requireRun(suite.T(), cmd)

	suite.T().Log("Uploading test chart...")
	caFile := path.Join(suite.buildDir, "oci-registry-ca.crt")
	require.NoError(suite.T(), os.WriteFile(caFile, suite.ociRegistryCert, 0644))
	requireRun(suite.T(), exec.Command(
		"helm",
		"push",
		fmt.Sprintf("%s/proxychart-0.1.0.tgz", suite.buildDir),
		fmt.Sprintf("oci://localhost:%d/charts", suite.ociRegistryPort),
		"--ca-file", caFile,
	))

	suite.T().Log("Registering ClaimDefinition...")
	suite.requireApply(
		corev1ac.Secret("oci-registry-ca", "chrysopoeia-system").
			WithStringData(map[string]string{
				"ca.crt": string(suite.ociRegistryCert),
			}),
		client.FieldOwner("e2e-test"))

	fluxConf := `
---
apiVersion: image.toolkit.fluxcd.io/v1
kind: ImageRepository
metadata:
  name: proxychart
spec:
  exclusionList:
  - ^.*\.sig$
  - ^sha256-.+$
  image: registry.oci-registry-system.svc:5000/charts/proxychart
  interval: 12h
  provider: generic
  certSecretRef:
    name: oci-registry-ca
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: proxychart
spec:
  interval: 12h
  provider: generic
  ref:
    semver: 0.1.0
  timeout: 60s
  url: oci://registry.oci-registry-system.svc:5000/charts/proxychart
  certSecretRef:
    name: oci-registry-ca
`
	requireRun(suite.T(),
		cmdWithStdin(suite.T(), strings.NewReader(fluxConf),
			"kubectl", "apply", "-f", "-", "-n", "chrysopoeia-system"),
	)

	for _, role := range []string{"provides", "manages", "requires"} {
		spec := chrysopoeiav1ac.ClaimDefinitionSpec().
			WithCRDNames(
				apiextv1.CustomResourceDefinitionNames{
					Kind:   "XChart",
					Plural: "xcharts",
				},
			).
			WithReference(
				chrysopoeiav1ac.SourceReference().
					WithAPIVersion("source.toolkit.fluxcd.io/v1").
					WithKind("OCIRepository").
					WithName("proxychart"),
			).
			WithVersionDiscovery(
				chrysopoeiav1ac.VersionDiscovery().
					WithReference(
						chrysopoeiav1ac.DiscoveryReference().
							WithAPIVersion("image.toolkit.fluxcd.io/v1").
							WithKind("ImageRepository").
							WithName("proxychart")),
			)

		const groupName = "group1"
		switch role {
		case "provides":
			spec = spec.WithProvides(
				chrysopoeiav1ac.DependencyReference().
					WithDependencyGroup(
						chrysopoeiav1ac.DependencyGroupReference().
							WithName(groupName)),
			)
		case "manages":
			spec = spec.WithManages(
				chrysopoeiav1ac.DependencyReference().
					WithDependencyGroup(
						chrysopoeiav1ac.DependencyGroupReference().
							WithName(groupName)),
			)
		case "requires":
			spec = spec.WithRequires(
				chrysopoeiav1ac.DependencyReference().
					WithDependencyGroup(
						chrysopoeiav1ac.DependencyGroupReference().
							WithName(groupName)),
			)
		}

		suite.requireApply(
			chrysopoeiav1ac.
				ClaimDefinition(fmt.Sprintf("%s-x", role), "chrysopoeia-system").
				WithSpec(spec),
			client.FieldOwner("e2e-test"))

		suite.T().Log("Waiting for CRD to become ready...")
		requireRun(suite.T(),
			exec.Command(
				"kubectl",
				"wait",
				fmt.Sprintf("crd/xcharts.%s-x.helmetica-bundles.io", role),
				"--for=create",
				"--for=condition=Established",
				"--timeout", "1m",
			),
		)
	}

	suite.T().Log("Install provider...")
	crds, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), path.Join(suite.projectDir, "test/e2e/testdata/testcontroller/config/crd"))
	suite.Require().NoError(err)

	providedCRDNames := make([]string, 0)
	for _, res := range crds.Resources() {
		raw, err := res.AsYAML()
		suite.Require().NoError(err)
		var crd apiextv1.CustomResourceDefinition
		suite.Require().NoError(yaml.UnmarshalStrict(raw, &crd))
		providedCRDNames = append(providedCRDNames, crd.GetName())
	}
	crdRefs := make([]*chrysopoeiav1ac.DependencyGroupCRDApplyConfiguration, len(providedCRDNames))
	for i, name := range providedCRDNames {
		crdRefs[i] = chrysopoeiav1ac.DependencyGroupCRD().WithName(name)
	}
	suite.requireApply(
		chrysopoeiav1ac.DependencyGroup("group1").WithSpec(chrysopoeiav1ac.DependencyGroupSpec().WithCRDs(crdRefs...)),
		client.FieldOwner("e2e-test"))

	providerClaim := new(unstructured.Unstructured)
	providerClaim.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "provides-x.helmetica-bundles.io",
		Version: "bundle",
		Kind:    "XChart",
	})
	providerClaim.SetNamespace("default")
	providerClaim.SetName("group1provider")
	crdYaml, err := crds.AsYaml()
	suite.Require().NoError(err)
	suite.Require().NoError(unstructured.SetNestedField(providerClaim.Object, string(crdYaml), "spec", "values", "raw"))
	suite.requireApply(client.ApplyConfigurationFromUnstructured(providerClaim), client.FieldOwner("e2e-test"))

	suite.T().Log("Waiting for provided CRD to establish...")
	for _, crd := range providedCRDNames {
		requireRun(suite.T(),
			exec.Command(
				"kubectl",
				"wait",
				"crd",
				crd,
				"--for=create",
				"--for=condition=Established",
				"--timeout", "1m",
			),
		)
	}

	suite.T().Log("Install manager...")
	tmpDir, err := os.MkdirTemp(suite.projectDir, "manager-")
	suite.Require().NoError(err)
	suite.T().Cleanup(func() {
		suite.Require().NoError(os.RemoveAll(tmpDir))
	})
	mk := kustomizeapi.Kustomization{
		Resources: []string{
			requireFilepathRel(suite.T(), tmpDir, path.Join(suite.projectDir, "test/e2e/testdata/testcontroller/config/default")),
		},
		Patches: []kustomizeapi.Patch{{
			Target: &kustomizeapi.Selector{
				ResId: resid.NewResIdKindOnly("Deployment", ""),
			},
			Patch: `[{"op":"test","path":"/spec/template/spec/containers/0/name","value":"manager"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--controller-id=group1"}]`,
		}},
	}
	mkj, err := json.Marshal(mk, jsonv1.DefaultOptionsV1())
	suite.Require().NoError(err)
	suite.T().Log("Generated kustomization:", string(mkj))
	suite.Require().NoError(os.WriteFile(path.Join(tmpDir, "kustomization.yaml"), mkj, 0644))

	managerManifests, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), tmpDir)
	suite.Require().NoError(err)

	managerClaim := new(unstructured.Unstructured)
	managerClaim.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "manages-x.helmetica-bundles.io",
		Version: "bundle",
		Kind:    "XChart",
	})
	managerClaim.SetName("group1manager")
	managerClaim.SetNamespace("default")
	managerYaml, err := managerManifests.AsYaml()
	suite.Require().NoError(err)
	suite.Require().NoError(unstructured.SetNestedField(managerClaim.Object, string(managerYaml), "spec", "values", "raw"))

	suite.requireApply(client.ApplyConfigurationFromUnstructured(managerClaim), client.FieldOwner("e2e-test"))

	requireRun(suite.T(),
		cmdWithJSONStdin(suite.T(), managerClaim,
			"kubectl", "wait", "-f", "-", "--for=jsonpath={.status.releaseStatus}=Ready", "--timeout", "1m"),
	)

	suite.T().Log("Install dependent (requires) claim...")
	tmj, err := json.Marshal(homunculusv1ac.Test("test", ""))
	suite.Require().NoError(err)
	dependentClaim := new(unstructured.Unstructured)
	dependentClaim.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "requires-x.helmetica-bundles.io",
		Version: "bundle",
		Kind:    "XChart",
	})
	dependentClaim.SetName("group1dependent")
	dependentClaim.SetNamespace("default")
	suite.Require().NoError(err)
	suite.Require().NoError(unstructured.SetNestedField(dependentClaim.Object, string(tmj), "spec", "values", "raw"))
	suite.requireApply(client.ApplyConfigurationFromUnstructured(dependentClaim), client.FieldOwner("e2e-test"))

	dep1ns := requireRun(suite.T(),
		cmdWithJSONStdin(suite.T(), dependentClaim,
			"kubectl", "wait", "-f", "-", "--for=jsonpath={.status.releaseStatus}=Ready", "--timeout", "1m", "-o=jsonpath={.status.instanceNamespace}"),
	)
	requireRun(suite.T(),
		exec.CommandContext(suite.T().Context(),
			"kubectl", "wait", "-n", dep1ns, "tests.homunculus.helmetica.io", "test", "--for=jsonpath={.status.controllerId}=group1", "--timeout", "1m"),
	)
}

func (suite *E2ESuite) requireApply(obj runtime.ApplyConfiguration, opts ...client.ApplyOption) {
	suite.T().Helper()

	id := "unknown"
	if co, ok := obj.(client.Object); ok {
		id = fmt.Sprintf("%s:%s/%s", co.GetObjectKind().GroupVersionKind(), co.GetNamespace(), co.GetName())
	}

	suite.T().Log("Applying", id)

	suite.Require().NoError(suite.client.Apply(suite.T().Context(), obj, opts...))
}

// cmdWithJSONStdin serializes what's in jsn with default options and adds it to the Cmds stdin.
func cmdWithJSONStdin(t *testing.T, jsn any, name string, arg ...string) *exec.Cmd {
	t.Helper()

	buf := new(bytes.Buffer)
	require.NoError(t, json.MarshalWrite(buf, jsn))
	return cmdWithStdin(t, buf, name, arg...)
}

func cmdWithStdin(t *testing.T, stdin io.Reader, name string, arg ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), name, arg...)
	cmd.Stdin = stdin
	return cmd
}
