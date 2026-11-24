//go:build e2e
// +build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/shlex"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kustomizeapi "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

const (
	certmanagerVersion = "v1.21.3"

	defaultKindBinary  = "kind"
	defaultKindCluster = "kind"

	prometheusOperatorVersion = "v0.86.2"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/releases/download/%s/stripped-down-crds.yaml"

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectRepository = "local.dev/chrysopoeia"
	projectTag        = "latest"
	projectImage      = projectRepository + ":" + projectTag
)

// requireRun executes the provided command within this context
func requireRun(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()

	t.Log("Executing command:", fmt.Sprintf("(PWD=%s)", cmd.Dir), strings.Join(cmd.Args, " "))
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%q failed with error %q", strings.Join(cmd.Args, " "), string(output))
	return string(output)
}

func installPrometheusOperatorCRDs(t *testing.T) {
	t.Helper()

	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	requireRun(t, exec.Command("kubectl", "apply", "-f", url))
}

// installCertManager installs the cert manager bundle.
func installCertManager(t *testing.T) {
	t.Helper()

	requireRun(t, exec.Command("helm", "upgrade", "--install", "cert-manager", "oci://quay.io/jetstack/charts/cert-manager", "--namespace", "cert-manager", "--create-namespace", "--version", certmanagerVersion, "--set", "crds.enabled=true"))

	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	requireDeploymentAvailable(t, "cert-manager", "cert-manager-webhook")
}

// requireDeploymentAvailable waits until the specified deployment is available
func requireDeploymentAvailable(t *testing.T, namespace, name string) {
	t.Helper()

	cmd := exec.Command("kubectl", "wait", "deployment.apps", name,
		"--for", "condition=Available",
		"--namespace", namespace,
		"--timeout", "3m",
	)
	t.Log("Running command:", cmd)
	if err := cmd.Run(); err != nil {
		t.Logf("failed to wait for deployment: %s", err.Error())
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			runAndLogToT(t, exec.Command("kubectl", "describe", "deployment.apps", name,
				"--namespace", namespace,
			))
			runAndLogToT(t, exec.Command("kubectl", "describe", "pods",
				"--namespace", namespace,
			))
			runAndLogToT(t, exec.Command("kubectl", "logs", "deployment.apps/"+name,
				"--namespace", namespace,
			))
		}
		t.FailNow()
	}
}

func runAndLogToT(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()

	cmd.Stdout = t.Output()
	cmd.Stderr = t.Output()
	t.Log("Executing command:", fmt.Sprintf("(PWD=%s)", cmd.Dir), strings.Join(cmd.Args, " "))
	return cmd.Run()
}

func isAnyCRDInstalled(t *testing.T, crds ...string) bool {
	t.Helper()

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output := requireRun(t, cmd)

	// Check if any of the given CRDs are present in the output
	for crd := range strings.Lines(output) {
		for _, line := range crds {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// isCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
// If any of the CRDs are found, it returns true.
func isCertManagerCRDsInstalled(t *testing.T) bool {
	t.Helper()

	return isAnyCRDInstalled(t,
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	)
}

// isPrometheusOperatorCRDsInstalled checks if any Prometheus Operator CRDs are installed
// by verifying the existence of key CRDs related to Prometheus Operator.
// If any of the CRDs are found, it returns true.
func isPrometheusOperatorCRDsInstalled(t *testing.T) bool {
	t.Helper()

	return isAnyCRDInstalled(t,
		"podmonitors.monitoring.coreos.com",
		"probes.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"scrapeconfigs.monitoring.coreos.com",
		"servicemonitors.monitoring.coreos.com",
		"thanosrulers.monitoring.coreos.com",
	)
}

// requireLoadImageToKindClusterWithName loads a local docker image to the kind cluster
func requireLoadImageToKindClusterWithName(t *testing.T, name string) {
	t.Helper()

	cluster := defaultKindCluster
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}
	// We use go tool to run kind, so we need to split the command properly
	cs, err := shlex.Split(kindBinary)
	require.NoError(t, err)

	cmd := exec.Command(cs[0], append(cs[1:], kindOptions...)...)
	cmd.Dir = getProjectDir(t)
	requireRun(t, cmd)
}

// getProjectDir will return the directory where the project is
func getProjectDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)

	return strings.ReplaceAll(wd, "/test/e2e", "")
}

// tmpNamespace creates a temporary namespace and returns its name
func tmpNamespace(t *testing.T) string {
	t.Helper()

	cmd := exec.Command("kubectl", "create", "-f", "-", "-ojsonpath={.metadata.name}")
	cmd.Stdin = strings.NewReader(`
apiVersion: v1
kind: Namespace
metadata:
  generateName: chrysopoeia-e2e-
  annotations:
    environment: e2e-test
`)
	return strings.TrimSpace(requireRun(t, cmd))
}

func buildAndUploadManagerImage(t *testing.T, projectDir, buildDir string) {
	t.Helper()

	buildCmd := exec.Command("go", "build", "-o", path.Join(buildDir, "chrysopoeia"))
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	buildCmd.Dir = projectDir
	requireRun(t, buildCmd)
	imageBuildCmd := exec.Command("docker", "build", "-f", path.Join(projectDir, "Dockerfile"), "-t", projectImage, buildDir)
	requireRun(t, imageBuildCmd)

	requireLoadImageToKindClusterWithName(t, projectImage)
}

func buildAndUploadTestControllerImage(t *testing.T, projectDir, buildDir string) {
	t.Helper()

	const testControllerPath = "./test/e2e/testdata/testcontroller"
	const testControllerImage = "local.dev/testcontroller:latest"

	makeCmd := exec.Command("make")
	makeCmd.Dir = path.Join(projectDir, testControllerPath)
	requireRun(t, makeCmd)

	buildCmd := exec.Command("go", "build", "-o", path.Join(buildDir, "testcontroller"), "./cmd")
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	buildCmd.Dir = path.Join(projectDir, testControllerPath)
	requireRun(t, buildCmd)
	imageBuildCmd := exec.Command("docker", "build", "-f", path.Join(projectDir, testControllerPath, "Dockerfile"), "-t", testControllerImage, buildDir)
	requireRun(t, imageBuildCmd)

	requireLoadImageToKindClusterWithName(t, testControllerImage)
}

func installControllerManager(t *testing.T, projectDir, tmpDir string) {
	t.Helper()

	kf := kustomizeapi.Kustomization{
		Resources: []string{
			requireFilepathRel(t, tmpDir, path.Join(projectDir, "config", "crd")),
			requireFilepathRel(t, tmpDir, path.Join(projectDir, "config", "default")),
			requireFilepathRel(t, tmpDir, path.Join(projectDir, "config", "proxy")),
		},
		// Override the manager image to use the locally built one
		Images: []kustomizeapi.Image{
			{
				Name:    "ghcr.io/helmetica-framework/chrysopoeia",
				NewName: projectRepository,
				NewTag:  projectTag,
			},
		},
		// Replace imagePullPolicy to Never in all managed deployments to use the locally loaded image in kind
		Replacements: []kustomizeapi.ReplacementField{{
			SourceValue: ptr.To("Never"),
			Targets: []*kustomizeapi.TargetSelector{{
				Select: &kustomizeapi.Selector{
					ResId: resid.NewResIdKindOnly("Deployment", ""),
				},
				Options: &kustomizeapi.FieldOptions{Create: true},
				FieldPaths: []string{
					"spec.template.spec.containers.*.imagePullPolicy",
				},
			}},
		}},
		Patches: []kustomizeapi.Patch{{
			Target: &kustomizeapi.Selector{
				ResId: resid.NewResIdWithNamespace(resid.FromKind("Deployment"), "chrysopoeia-controller-manager", "chrysopoeia-system"),
			},
			Patch: `[{"op":"test","path":"/spec/template/spec/containers/0/name","value":"manager"},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--image-reflector-controller-hostname=image-reflector-controller-tags.chrysopoeia-flux-system.svc:8090"}]`,
		}},
	}

	enc, err := json.Marshal(kf)
	require.NoError(t, err)
	t.Log("Generated kustomization:", string(enc))

	require.NoError(t, os.WriteFile(path.Join(tmpDir, "kustomization.yaml"), enc, 0644))

	cmd := exec.Command("kubectl", "apply", "-k", tmpDir)
	requireRun(t, cmd)

	t.Log("Installing flux...")

	requireRun(t, exec.Command("kubectl", "apply", "-k", path.Join(projectDir, "config", "flux")))
}

func requireFilepathRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	require.NoError(t, err)
	return rel
}

func installOCIRegistry(t *testing.T, c client.Reader, projectDir string) (extPort int, certCA []byte) {
	t.Helper()

	requireRun(t, exec.Command("kubectl", "apply", "-f", path.Join(projectDir, "test/e2e/testdata/oci-registry.yaml")))
	requireDeploymentAvailable(t, "oci-registry-system", "registry")

	var certSecret corev1.Secret
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "tls-server-certificate", Namespace: "oci-registry-system"}, &certSecret))
	certCA, ok := certSecret.Data["ca.crt"]
	if !ok {
		t.Fatalf("CA certificate not found in secret %+v", certSecret)
	}

	fwdPort, err := freePort()
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		cmd := exec.CommandContext(t.Context(), "kubectl", "port-forward", "-n", "oci-registry-system", "svc/registry", fmt.Sprintf("%d:5000", fwdPort))
		t.Log("Executing command:", fmt.Sprintf("(PWD=%s)", cmd.Dir), strings.Join(cmd.Args, " "))

		// Run the command and log any errors, but don't fail the test if it is killed due to test finished.
		if err := cmd.Run(); err != nil && t.Context().Err() == nil {
			t.Errorf("Error occurred while running command %v: %v", cmd.Args, err)
		}
	})
	t.Cleanup(wg.Wait)

	return fwdPort, certCA
}

// freePort returns a free port on the host.
func freePort() (int, error) {
	a, err := net.ResolveTCPAddr("tcp", ":0")
	if err != nil {
		return 0, err
	}
	l, err := net.ListenTCP("tcp", a)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
