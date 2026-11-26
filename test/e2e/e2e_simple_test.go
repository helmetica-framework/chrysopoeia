//go:build e2e
// +build e2e

package e2e

import (
	"os/exec"
	"path"
)

func (suite *E2ESuite) TestInstallPodinfo() {
	suite.T().Log("Registering ClaimDefinition...")
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"apply",
			"-n", "chrysopoeia-system",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claimdefinition.yaml"),
		),
	)

	suite.T().Log("Waiting for CRD to become ready...")
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"wait",
			"crd/podinfos.v0.podinfo.helmetica-bundles.io",
			"--for=create",
			"--for=condition=Established",
			"--timeout", "1m",
		),
	)

	suite.T().Log("Applying claim1...")
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"apply",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claim1.yaml"),
		),
	)
	suite.T().Log("Waiting for claim1 to become ready...")
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"wait",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claim1.yaml"),
			"--for=jsonpath={.status.releaseStatus}=Ready",
			"--for=jsonpath={.status.instanceNamespace}",
			"--timeout", "1m",
			"--output=jsonpath={.status.instanceNamespace}",
		),
	)

	claim1Ns := requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"get",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claim1.yaml"),
			"--output=jsonpath={.status.instanceNamespace}",
		),
	)
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"wait",
			"-n", claim1Ns,
			"helmrelease", "claim1",
			"--for=condition=Ready",
			"--timeout", "1m",
		),
	)

	suite.T().Log("Deleting claim1...")
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"delete",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claim1.yaml"),
		),
	)
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"wait",
			"namespace", claim1Ns,
			"--for=delete",
			"--timeout", "1m",
		),
	)
	requireRun(suite.T(),
		exec.Command(
			"kubectl",
			"wait",
			"-f", path.Join(suite.projectDir, "test", "e2e", "testdata", "podinfo", "claim1.yaml"),
			"--for=delete",
			"--timeout", "1m",
		),
	)
}
