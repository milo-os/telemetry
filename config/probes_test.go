// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"bytes"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSinkCollectorHasHealthCheck guards the sink's probes. The OpenTelemetry
// operator generates readiness, liveness and startup probes only when the
// health_check extension is configured and enabled under service.extensions;
// without both, the pod runs with no probes at all. See milo-os/telemetry#268.
func TestSinkCollectorHasHealthCheck(t *testing.T) {
	const path = "collectors/sink-collector.yaml"

	var doc struct {
		Spec struct {
			StartupProbe struct {
				PeriodSeconds    int `yaml:"periodSeconds"`
				FailureThreshold int `yaml:"failureThreshold"`
			} `yaml:"startupProbe"`
			Config struct {
				Extensions struct {
					HealthCheck struct {
						Endpoint string `yaml:"endpoint"`
					} `yaml:"health_check"`
				} `yaml:"extensions"`
				Service struct {
					Extensions []string `yaml:"extensions"`
				} `yaml:"service"`
			} `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	if got := doc.Spec.Config.Extensions.HealthCheck.Endpoint; got != "0.0.0.0:13133" {
		t.Errorf("%s: health_check endpoint %q, want 0.0.0.0:13133 so the kubelet can reach it", path, got)
	}
	if !slices.Contains(doc.Spec.Config.Service.Extensions, "health_check") {
		t.Errorf("%s: health_check is not in service.extensions; the operator generates no probes", path)
	}

	// The clickhouse exporter blocks Start for about 30s on schema detection
	// while ClickHouse is unreachable, and health_check reports unavailable
	// until Start returns. A short startup window would turn a ClickHouse
	// outage into a crash loop.
	sp := doc.Spec.StartupProbe
	if sp.PeriodSeconds*sp.FailureThreshold < 90 {
		t.Errorf("%s: startupProbe allows %ds, want at least 90s to outlast a ClickHouse outage at startup",
			path, sp.PeriodSeconds*sp.FailureThreshold)
	}
}

// TestVectorProbesReachItsAPI guards the Vector exporter's probes. Vector
// serves GET /health only when its API is enabled, and binds 127.0.0.1 unless
// told otherwise, where the kubelet cannot reach it. The same port also serves
// the unauthenticated gRPC tap API, so nothing else may admit it.
func TestVectorProbesReachItsAPI(t *testing.T) {
	const (
		configPath     = "operator/vector/base-vector-config.yaml"
		deploymentPath = "operator/vector/deployment.yaml"
		policyPath     = "operator/vector/network-policy.yaml"
		servicePath    = "operator/vector/service.yaml"
	)

	var cfg struct {
		API struct {
			Enabled bool   `yaml:"enabled"`
			Address string `yaml:"address"`
		} `yaml:"api"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, configPath)), &cfg); err != nil {
		t.Fatalf("%s: %v", configPath, err)
	}
	if !cfg.API.Enabled {
		t.Errorf("%s: api is disabled; /health is not served", configPath)
	}
	if cfg.API.Address != "0.0.0.0:8686" {
		t.Errorf("%s: api address %q, want 0.0.0.0:8686 so the kubelet can reach it", configPath, cfg.API.Address)
	}

	type probe struct {
		HTTPGet struct {
			Path string `yaml:"path"`
			Port string `yaml:"port"`
		} `yaml:"httpGet"`
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name  string `yaml:"name"`
						Ports []struct {
							Name          string `yaml:"name"`
							ContainerPort int    `yaml:"containerPort"`
						} `yaml:"ports"`
						ReadinessProbe *probe `yaml:"readinessProbe"`
						LivenessProbe  *probe `yaml:"livenessProbe"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, deploymentPath)), &dep); err != nil {
		t.Fatalf("%s: %v", deploymentPath, err)
	}
	found := false
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "vector" {
			continue
		}
		found = true
		ports := map[string]int{}
		for _, p := range c.Ports {
			ports[p.Name] = p.ContainerPort
		}
		for kind, p := range map[string]*probe{"readinessProbe": c.ReadinessProbe, "livenessProbe": c.LivenessProbe} {
			if p == nil {
				t.Errorf("%s: vector container has no %s", deploymentPath, kind)
				continue
			}
			if p.HTTPGet.Path != "/health" {
				t.Errorf("%s: %s path %q, want /health", deploymentPath, kind, p.HTTPGet.Path)
			}
			if ports[p.HTTPGet.Port] != 8686 {
				t.Errorf("%s: %s port %q does not name container port 8686", deploymentPath, kind, p.HTTPGet.Port)
			}
		}
	}
	if !found {
		t.Fatalf("%s: no vector container", deploymentPath)
	}

	var policy struct {
		Spec struct {
			PolicyTypes []string `yaml:"policyTypes"`
			Ingress     []struct {
				Ports []struct {
					Port int `yaml:"port"`
				} `yaml:"ports"`
			} `yaml:"ingress"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, policyPath)), &policy); err != nil {
		t.Fatalf("%s: %v", policyPath, err)
	}
	if !slices.Contains(policy.Spec.PolicyTypes, "Ingress") {
		t.Errorf("%s: policy does not restrict ingress", policyPath)
	}
	if len(policy.Spec.Ingress) == 0 {
		t.Errorf("%s: no ingress rules; the metrics port would be cut off too", policyPath)
	}
	for _, rule := range policy.Spec.Ingress {
		if len(rule.Ports) == 0 {
			t.Errorf("%s: an ingress rule admits every port, including the 8686 API", policyPath)
		}
		for _, p := range rule.Ports {
			if p.Port != 9598 {
				t.Errorf("%s: ingress admits port %d; only the 9598 metrics port may be admitted", policyPath, p.Port)
			}
		}
	}

	if strings.Contains(readFile(t, servicePath), "8686") {
		t.Errorf("%s: exposes the 8686 API port", servicePath)
	}
}

func kustomizeBuild(t *testing.T, dir string) []byte {
	t.Helper()
	cmds := [][]string{{"kustomize", "build"}, {"kubectl", "kustomize"}}
	for _, c := range cmds {
		bin, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, append(c[1:], dir)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", c[0], strings.Join(c[1:], " "), err, out)
		}
		return out
	}
	t.Fatalf("neither kustomize nor kubectl is on PATH; cannot render %s", dir)
	return nil
}

// TestVectorNetworkPolicySelectsRenderedPods guards the NetworkPolicy that
// keeps the 8686 gRPC tap closed. The policy's selector is rewritten by the
// kustomization's label transformer, so only the rendered output shows whether
// it still selects the Deployment's pods. A mismatch leaves the pods open.
func TestVectorNetworkPolicySelectsRenderedPods(t *testing.T) {
	rendered := kustomizeBuild(t, "operator/vector")

	var podLabels, selector map[string]string
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var obj struct {
			Kind string `yaml:"kind"`
			Spec struct {
				PodSelector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"podSelector"`
				Template struct {
					Metadata struct {
						Labels map[string]string `yaml:"labels"`
					} `yaml:"metadata"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("operator/vector: decoding rendered output: %v", err)
		}
		switch obj.Kind {
		case "Deployment":
			podLabels = obj.Spec.Template.Metadata.Labels
		case "NetworkPolicy":
			selector = obj.Spec.PodSelector.MatchLabels
		}
	}

	if len(podLabels) == 0 {
		t.Fatal("operator/vector: rendered Deployment has no pod template labels")
	}
	if len(selector) == 0 {
		t.Fatal("operator/vector: rendered NetworkPolicy has an empty podSelector, which selects every pod in the namespace")
	}
	for k, v := range selector {
		if podLabels[k] != v {
			t.Errorf("operator/vector: NetworkPolicy selects %s=%q but the Deployment's pods carry %s=%q; the policy protects nothing",
				k, v, k, podLabels[k])
		}
	}
}

// TestSinkCollectorLivenessProbe guards the sink's liveness settings. Six
// failures at a 10s period restart a wedged collector after about a minute
// without restarting it for a brief stall.
func TestSinkCollectorLivenessProbe(t *testing.T) {
	const path = "collectors/sink-collector.yaml"

	var doc struct {
		Spec struct {
			LivenessProbe struct {
				PeriodSeconds    int `yaml:"periodSeconds"`
				FailureThreshold int `yaml:"failureThreshold"`
			} `yaml:"livenessProbe"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	lp := doc.Spec.LivenessProbe
	if lp.PeriodSeconds != 10 {
		t.Errorf("%s: livenessProbe periodSeconds %d, want 10", path, lp.PeriodSeconds)
	}
	if lp.FailureThreshold != 6 {
		t.Errorf("%s: livenessProbe failureThreshold %d, want 6", path, lp.FailureThreshold)
	}
}
