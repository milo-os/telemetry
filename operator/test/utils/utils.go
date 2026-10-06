/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive,staticcheck
)

const (
	prometheusOperatorVersion = "v0.77.1"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	certmanagerVersion = "v1.16.3"
	certmanagerURLTmpl = "https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml"
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "create", "-f", url)
	_, err := Run(cmd)
	return err
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// certManagerLeaderLeases are cert-manager's leader-election leases in kube-system,
// which its manifest does not remove on uninstall. A stale lease can block the
// cainjector from acquiring leadership and injecting the CA bundle, leaving the
// webhook untrusted by the API server.
var certManagerLeaderLeases = []string{
	"cert-manager-cainjector-leader-election",
	"cert-manager-controller",
	"cert-manager-webhook",
}

// UninstallCertManager uninstalls cert-manager, including the leader-election leases
// in kube-system that the manifest deletion leaves behind.
func UninstallCertManager() {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}

	for _, lease := range certManagerLeaderLeases {
		cmd = exec.Command("kubectl", "delete", "lease", lease, "-n", "kube-system", "--ignore-not-found")
		if _, err := Run(cmd); err != nil {
			warnError(err)
		}
	}
}

// InstallCertManager installs cert-manager and waits until its webhook is trusted by
// the API server. Deployment availability alone is not enough: the cainjector must
// first inject the CA bundle into the webhook configuration, otherwise the API server
// rejects the webhook with "unknown authority" and fails the resource apply.
func InstallCertManager() error {
	url := fmt.Sprintf(certmanagerURLTmpl, certmanagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}
	// Wait for cert-manager-webhook to be ready, which can take time if cert-manager
	// was re-installed after uninstalling on a cluster.
	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)
	if _, err := Run(cmd); err != nil {
		return err
	}

	// Wait until the CA bundle is injected into the webhook configuration so the API
	// server trusts the webhook, not just until the deployment is running.
	caBundleReady := func() bool {
		out, err := exec.Command("kubectl", "get", "validatingwebhookconfiguration", "cert-manager-webhook",
			"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}").CombinedOutput()
		if err != nil {
			return false
		}
		return len(bytes.TrimSpace(out)) > 0
	}

	deadline := time.Now().Add(5 * time.Minute)
	for !caBundleReady() {
		if time.Now().After(deadline) {
			return fmt.Errorf("cert-manager webhook CA bundle was never injected into " +
				"validatingwebhookconfiguration/cert-manager-webhook: the cainjector may be " +
				"blocked by a stale leader-election lease")
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// kindCluster is the Kind cluster the suite runs against: KIND_CLUSTER, or
// "kind", the name `kind create cluster` uses.
func kindCluster() string {
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		return v
	}
	return "kind"
}

// IsolateKubeconfig points every later kubectl, task and kind command in this
// process at a private kubeconfig for the Kind cluster, and returns a func that
// removes it.
//
// The suite runs bare kubectl, which reads the current context from the
// kubeconfig. The shared ~/.kube/config is not the suite's to rely on: any
// other process can change its current context mid-run, and on 2026-10-06 one
// did, so the teardown ran against a production cluster (#201). A private file
// named by KUBECONFIG cannot be changed by anything else. Children inherit it
// from the environment, which Run and every bare exec.Command pass on.
//
// It refuses unless the kubeconfig's API server is on loopback, where Kind
// serves it, so a misnamed cluster or a remote Docker host fails here rather
// than at teardown.
func IsolateKubeconfig() (func(), error) {
	cluster := kindCluster()
	raw, err := exec.Command("kind", "get", "kubeconfig", "--name", cluster).Output()
	if err != nil {
		return nil, fmt.Errorf("getting the kubeconfig for Kind cluster %q: %w", cluster, err)
	}
	f, err := os.CreateTemp("", "e2e-kubeconfig-*")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		cleanup()
		return nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, err
	}

	// Read the server from the file itself, not the environment's kubeconfig.
	cmd := exec.Command("kubectl", "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+path)
	server, err := cmd.Output()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("reading the API server from the Kind kubeconfig: %w", err)
	}
	if err := RequireLoopbackServer(strings.TrimSpace(string(server))); err != nil {
		cleanup()
		return nil, err
	}

	if err := os.Setenv("KUBECONFIG", path); err != nil {
		cleanup()
		return nil, err
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "using private kubeconfig %s for Kind cluster %q (%s)\n",
		path, cluster, strings.TrimSpace(string(server)))
	return cleanup, nil
}

// RequireLoopbackServer returns an error unless server is an API server URL on
// a loopback address.
func RequireLoopbackServer(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("refusing to run: cannot read an API server host from %q", server)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("refusing to run: the e2e suite creates and deletes cluster-wide resources, "+
		"and its API server %s is not on loopback, so it is not a local Kind cluster", server)
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := kindCluster()
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err := Run(cmd)
	return err
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %s to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		_, err := out.WriteString(strings.TrimPrefix(scanner.Text(), prefix))
		if err != nil {
			return err
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err := out.WriteString("\n"); err != nil {
			return err
		}
	}

	_, err = out.Write(content[idx+len(target):])
	if err != nil {
		return err
	}
	// false positive
	// nolint:gosec
	return os.WriteFile(filename, out.Bytes(), 0644)
}
