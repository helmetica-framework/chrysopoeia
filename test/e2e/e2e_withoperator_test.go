//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"path"
)

func (suite *E2ESuite) TestInstallOperatorBasedService() {
	suite.T().Log("Building test chart...")
	cmd := exec.Command(
		"helm",
		"package",
		path.Join(suite.projectDir, "test/e2e/testdata/testchart"),
	)
	cmd.Dir = suite.buildDir
	requireRun(suite.T(), cmd)

	suite.T().Log("Uploading test chart...")
	cmd = exec.Command(
		"helm",
		"push",
		fmt.Sprintf("%s/testchart-0.1.0.tgz", suite.buildDir),
		fmt.Sprintf("oci://localhost:%d/testchart", suite.ociRegistryPort),
		"--insecure-skip-tls-verify",
	)
	requireRun(suite.T(), cmd)
}
