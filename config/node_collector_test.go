// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Provider components depend on these shared IDs. Infra validates the complete
// composition with the pinned collector before publishing a deployment bundle.
func TestComputeNodeCollectorContract(t *testing.T) {
	raw, err := os.ReadFile("node-collector/collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Spec struct {
			Image  string `yaml:"image"`
			Config struct {
				Receivers  map[string]any `yaml:"receivers"`
				Processors map[string]any `yaml:"processors"`
				Exporters  map[string]any `yaml:"exporters"`
				Extensions map[string]any `yaml:"extensions"`
				Service    struct {
					Pipelines map[string]struct {
						Receivers  []string `yaml:"receivers"`
						Processors []string `yaml:"processors"`
						Exporters  []string `yaml:"exporters"`
					} `yaml:"pipelines"`
				} `yaml:"service"`
			} `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile("node-collector/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ContractVersion  int      `json:"contractVersion"`
		CollectorVersion string   `json:"collectorVersion"`
		Pipelines        []string `json:"pipelines"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ContractVersion != 1 || manifest.CollectorVersion == "" ||
		!strings.HasSuffix(document.Spec.Image, ":"+manifest.CollectorVersion) {
		t.Fatal("manifest and collector image must agree on the version")
	}
	c := document.Spec.Config
	if len(c.Receivers) != 1 || c.Receivers["filelog/kubernetes"] == nil {
		t.Fatal("base must own exactly one Kubernetes file reader")
	}
	if c.Extensions["file_storage"] == nil || c.Processors["memory_limiter"] == nil ||
		c.Exporters["otlp_grpc/project"] == nil || c.Exporters["prometheusremotewrite/compute"] == nil {
		t.Fatal("missing shared component")
	}
	if len(manifest.Pipelines) != 1 || manifest.Pipelines[0] != "logs/platform" || len(c.Service.Pipelines) != 1 {
		t.Fatal("base must declare only the platform pipeline")
	}
	pipeline := c.Service.Pipelines["logs/platform"]
	if len(pipeline.Receivers) != 1 || pipeline.Receivers[0] != "filelog/kubernetes" ||
		len(pipeline.Processors) == 0 || pipeline.Processors[0] != "memory_limiter" ||
		len(pipeline.Exporters) != 1 || pipeline.Exporters[0] != "otlp_grpc/project" {
		t.Fatal("platform pipeline must use shared input, memory limiter, and durable export")
	}
	filter := c.Processors["filter/platform"].(map[string]any)["logs"].(map[string]any)["log_record"].([]any)
	conditions := make(map[string]bool)
	for _, condition := range filter {
		conditions[condition.(string)] = true
	}
	if !conditions[`resource.attributes["k8s.node.name"] == nil`] ||
		!conditions[`resource.attributes["telemetry.native_logs"] == "true"`] {
		t.Fatal("platform must exclude unknown ownership and service-owned logs")
	}
}
