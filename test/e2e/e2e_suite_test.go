//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/suite"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	chrysopoeiav1 "github.com/helmetica-framework/chrysopoeia/api/v1"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
)

type E2ESuite struct {
	suite.Suite

	client client.WithWatch

	projectDir string
	buildDir   string

	ociRegistryPort int
	ociRegistryCert []byte
}

func (suite *E2ESuite) SetupSuite() {
	{
		scheme := runtime.NewScheme()
		suite.Require().NoError(chrysopoeiav1.AddToScheme(scheme))
		suite.Require().NoError(clientgoscheme.AddToScheme(scheme))
		conf, err := ctrl.GetConfig()
		suite.Require().NoError(err)
		c, err := client.NewWithWatch(conf, client.Options{Scheme: scheme})
		suite.Require().NoError(err)
		suite.client = c
	}

	suite.projectDir = getProjectDir(suite.T())
	buildDir, err := os.MkdirTemp(suite.projectDir, "chrysopoeia-e2e-")
	suite.Require().NoError(err)
	suite.buildDir = buildDir
	suite.T().Logf("Setting up E2E suite... projectDir=%s, buildDir=%s", suite.projectDir, suite.buildDir)

	if !skipCertManagerInstall && !isCertManagerCRDsInstalled(suite.T()) {
		installCertManager(suite.T())
	}
	if !isPrometheusOperatorCRDsInstalled(suite.T()) {
		installPrometheusOperatorCRDs(suite.T())
	}

	suite.T().Log("Building and uploading manager image to kind cluster...")
	buildAndUploadManagerImage(suite.T(), suite.projectDir, suite.buildDir)

	suite.T().Log("Building and uploading test controller image to kind cluster...")
	buildAndUploadTestControllerImage(suite.T(), suite.projectDir, suite.buildDir)

	suite.T().Log("Installing OCI registry...")
	suite.ociRegistryPort, suite.ociRegistryCert = installOCIRegistry(suite.T(), suite.client, suite.projectDir)

	suite.T().Log("Installing controller manager...")
	installControllerManager(suite.T(), suite.projectDir, suite.buildDir)
	requireDeploymentAvailable(suite.T(), "chrysopoeia-system", "chrysopoeia-controller-manager")
}

func (suite *E2ESuite) TearDownSuite() {
	suite.T().Log("Tearing down E2E suite...")

	suite.T().Log("Removing build directory:", suite.buildDir)
	suite.Require().NoError(os.RemoveAll(suite.buildDir))
}

func (suite *E2ESuite) TearDownTest() {
	if suite.T().Failed() {
		suite.T().Log("Test failed, fetching controller manager logs...")
		runAndLogToT(suite.T(), exec.Command("kubectl", "logs", "deployment.apps/chrysopoeia-controller-manager",
			"--namespace", "chrysopoeia-system",
		))
	}
}

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purpose of being used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, installs CertManager,
// and Prometheus Operator CRDs.
func TestE2E(t *testing.T) {
	suite.Run(t, new(E2ESuite))
}
