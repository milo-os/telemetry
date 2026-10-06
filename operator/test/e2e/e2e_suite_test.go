/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use
this file except in compliance with the License. You may obtain a copy of the
License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed
under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.datum.net/o11y/operator/test/utils"
)

var (
	// Optional Environment Variables: - CERT_MANAGER_INSTALL_SKIP=true: Skips
	// CertManager installation during test setup. These variables are useful if
	// CertManager is already installed, avoiding re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be
	// found on the cluster
	isCertManagerAlreadyInstalled = false

	// - PROMETHEUS_OPERATOR_INSTALL_SKIP=true: Skips Prometheus Operator
	// installation during test setup. These variables are useful if Prometheus
	// Operator is already installed, avoiding re-installation and conflicts.
	skipPrometheusOperatorInstall = os.Getenv("PROMETHEUS_OPERATOR_INSTALL_SKIP") == "true"
	// isPrometheusOperatorAlreadyInstalled will be set true when Prometheus
	// Operator CRDs be found on the cluster
	isPrometheusOperatorAlreadyInstalled = false

	// projectImage is the name of the image which will be build and loaded with
	// the code source changes to be tested.
	projectImage = "example.com/telemetry-services-operator:v0.0.1"
	// cleanupKubeconfig removes the suite's private kubeconfig. It is nil until
	// IsolateKubeconfig succeeds, and teardown runs only once it has: without
	// the private kubeconfig, kubectl would reach whatever cluster the shared
	// kubeconfig names (#201).
	cleanupKubeconfig func()
)

// taskImageVars splits "repo:tag" into the IMAGE_NAME/IMAGE_TAG task
// variables the Taskfile's docker-build/deploy tasks expect (unlike the
// old Makefile's single combined IMG variable).
func taskImageVars(image string) []string {
	name, tag, _ := strings.Cut(image, ":")
	return []string{
		fmt.Sprintf("IMAGE_NAME=%s", name),
		fmt.Sprintf("IMAGE_TAG=%s", tag),
	}
}

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests
// execute in an isolated, temporary environment to validate project changes
// with the the purposed to be used in CI jobs. The default setup requires Kind,
// builds/loads the Manager Docker image locally, and installs CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting telemetry-services-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// First, before anything runs kubectl.
	By("isolating kubectl to the Kind cluster")
	cleanup, err := utils.IsolateKubeconfig()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to isolate kubectl to the Kind cluster")
	cleanupKubeconfig = cleanup

	By("building the manager(Operator) image")
	cmd := exec.Command("task", append([]string{"docker-build"}, taskImageVars(projectImage)...)...)
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// TODO(user): If you want to change the e2e test vendor from Kind, ensure the
	// image is built and available before running the tests. Also, remove the
	// following block.
	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	// The tests-e2e are intended to run on a temporary cluster that is created
	// and destroyed for testing. To prevent errors when tests run in environments
	// with CertManager already installed, we check for its presence before
	// execution. Setup CertManager before the suite if not skipped and if not
	// already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}

	// The tests-e2e are intended to run on a temporary cluster that is created
	// and destroyed for testing. To prevent errors when tests run in environments
	// with PrometheusOperator already installed, we check for its presence before
	// execution. Setup PrometheusOperator before the suite if not skipped and if
	// not already installed
	if !skipPrometheusOperatorInstall {
		By("checking if prometheus operator is installed already")
		isPrometheusOperatorAlreadyInstalled = utils.IsPrometheusCRDsInstalled()
		if !isPrometheusOperatorAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing PrometheusOperator...\n")
			Expect(utils.InstallPrometheusOperator()).To(Succeed(), "Failed to install PrometheusOperator")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: PrometheusOperator is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	if cleanupKubeconfig == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Skipping teardown: kubectl was never isolated to the Kind cluster\n")
		return
	}
	defer cleanupKubeconfig()

	// Teardown CertManager after the suite if not skipped and if it was not
	// already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}

	// Teardown PrometheusOperator after the suite if not skipped and if it was
	// not already installed
	if !skipPrometheusOperatorInstall && !isPrometheusOperatorAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling PrometheusOperator...\n")
		utils.UninstallPrometheusOperator()
	}
})
