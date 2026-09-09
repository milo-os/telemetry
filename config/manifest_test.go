// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestServerAuthCertsHaveDNSNames guards against a cert-manager CSI volume
// requesting server auth with no SAN names: cert-manager issues the
// certificate anyway, but Go's TLS client verifier requires a SAN match and
// ignores the deprecated CN field entirely, so every such client rejects the
// handshake. See milo-os/telemetry#148.
func TestServerAuthCertsHaveDNSNames(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var doc map[string]any
			if decErr := dec.Decode(&doc); decErr != nil {
				break
			}
			checkCSIVolumes(t, path, doc)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk config/: %v", err)
	}
}

// TestNATSSubjectsArePoPScoped guards the edge collectors' publish subjects
// against the hub's per-PoP leaf grant, which allows only
// o11y.<signal>.<cluster>.* -- a leaf publish outside it is dropped by the
// edge with no error on either side, so the whole path looks healthy while
// nothing crosses. See milo-os/telemetry#146 for the grant.
func TestNATSSubjectsArePoPScoped(t *testing.T) {
	collectors := map[string]bool{
		"collectors/node-agent-collector.yaml": true,
		"collectors/gateway-collector.yaml":    true,
	}
	for path := range collectors {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		var doc struct {
			Spec struct {
				Config struct {
					Processors map[string]struct {
						LogStatements []struct {
							Statements []string `yaml:"statements"`
						} `yaml:"log_statements"`
					} `yaml:"processors"`
					Exporters struct {
						NATS struct {
							Logs struct {
								Subject string `yaml:"subject"`
							} `yaml:"logs"`
						} `yaml:"nats"`
					} `yaml:"exporters"`
				} `yaml:"config"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		// The attribute the nats exporter routes on must carry the cluster
		// token between the signal and the project.
		var subjectStmt string
		for _, proc := range doc.Spec.Config.Processors {
			for _, block := range proc.LogStatements {
				for _, stmt := range block.Statements {
					if strings.Contains(stmt, `"telemetry.subject"`) {
						subjectStmt = stmt
					}
				}
			}
		}
		if subjectStmt == "" {
			t.Errorf("%s: no statement sets telemetry.subject", path)
			continue
		}
		if !strings.Contains(subjectStmt, `resource.attributes["k8s.cluster.name"]`) {
			t.Errorf("%s: telemetry.subject omits the cluster token, so the hub's per-PoP grant silently drops it: %s",
				path, subjectStmt)
		}

		// The static fallback has to satisfy the same grant.
		if got := doc.Spec.Config.Exporters.NATS.Logs.Subject; !strings.Contains(got, "${cluster}") {
			t.Errorf("%s: nats exporter fallback subject %q omits the cluster token", path, got)
		}
	}
}

func checkCSIVolumes(t *testing.T, path string, node any) {
	switch v := node.(type) {
	case map[string]any:
		if driver, _ := v["driver"].(string); driver == "csi.cert-manager.io" {
			attrs, _ := v["volumeAttributes"].(map[string]any)
			usages, _ := attrs["csi.cert-manager.io/key-usages"].(string)
			if strings.Contains(usages, "server auth") {
				dnsNames, _ := attrs["csi.cert-manager.io/dns-names"].(string)
				if strings.TrimSpace(dnsNames) == "" {
					t.Errorf("%s: CSI cert-manager volume (common-name %v) requests server auth with no dns-names -- cert-manager issues it with no SAN, which every Go TLS client rejects",
						path, attrs["csi.cert-manager.io/common-name"])
				}
			}
		}
		for _, child := range v {
			checkCSIVolumes(t, path, child)
		}
	case []any:
		for _, child := range v {
			checkCSIVolumes(t, path, child)
		}
	}
}

func TestSinkLogsTableIsUnqualified(t *testing.T) {
	const path = "collectors/sink-collector.yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	var doc struct {
		Spec struct {
			Config struct {
				Exporters struct {
					ClickHouse struct {
						Database      string `yaml:"database"`
						LogsTableName string `yaml:"logs_table_name"`
						Username      string `yaml:"username"`
					} `yaml:"clickhouse"`
				} `yaml:"exporters"`
			} `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	ch := doc.Spec.Config.Exporters.ClickHouse

	// The exporter qualifies the table with `database` itself, so a dotted
	// name here becomes o11y.`o11y.logs` and every INSERT fails with code 60.
	if strings.Contains(ch.LogsTableName, ".") {
		t.Errorf("%s: logs_table_name %q is qualified; the exporter prepends database %q, yielding %s.`%s`",
			path, ch.LogsTableName, ch.Database, ch.Database, ch.LogsTableName)
	}

	// A certificate does not select the ClickHouse identity: unset, the
	// driver sends "default" and authentication fails.
	if ch.Username == "" {
		t.Errorf("%s: clickhouse exporter names no username", path)
	}
}

// observedTimeAttribute is the name the sink's transform and the schema's
// ObservedTimestamp expression must agree on.
const observedTimeAttribute = "telemetry.observed_time_unix_nano"

// TestObservedTimeAttributeIsPlumbedEndToEnd guards the chain that populates
// ObservedTimestamp. Broken, nothing errors -- rows just stop matching any
// time window, since it is the partition/order/query key.
func TestObservedTimeAttributeIsPlumbedEndToEnd(t *testing.T) {
	const (
		sinkPath    = "collectors/sink-collector.yaml"
		gatewayPath = "collectors/gateway-collector.yaml"
		schemaPath  = "clickhouse-migrations/migrations/000001_init.up.sql"
	)

	// The sink hands the value to the exporter as an attribute...
	sink := readFile(t, sinkPath)
	if !strings.Contains(sink, observedTimeAttribute) {
		t.Errorf("%s: does not promote %s; ObservedTimestamp will fall back to insert time",
			sinkPath, observedTimeAttribute)
	}
	if !strings.Contains(sink, "transform/observed_time") {
		t.Errorf("%s: transform/observed_time is not in the logs pipeline", sinkPath)
	}

	// ...the schema reads that same attribute back out...
	if schema := readFile(t, schemaPath); !strings.Contains(schema, observedTimeAttribute) {
		t.Errorf("%s: does not materialize ObservedTimestamp from %s", schemaPath, observedTimeAttribute)
	}

	// ...and the gateway stamps it, since its otlp receiver leaves it at 0.
	if gw := readFile(t, gatewayPath); !strings.Contains(gw, "observed_time_unix_nano") {
		t.Errorf("%s: does not stamp observed_time_unix_nano on receipt", gatewayPath)
	}
}

// strippedByHub lists the collectors/ resources the hub's o11y-sink-app
// Kustomization deletes by kind and name. The bundle is applied to both the
// hub and the edges, so anything outside this set reaches the hub too.
var strippedByHub = map[string]bool{
	"Deployment/nats":                             true,
	"Service/nats":                                true,
	"ConfigMap/nats-config":                       true,
	"OpenTelemetryCollector/gateway-collector":    true,
	"OpenTelemetryCollector/node-agent-collector": true,
}

// TestEdgeOnlyResourcesCannotCollideOnTheHub guards a cross-repo contract the
// compiler cannot see. collectors/ ships to the hub as well as the edges, and
// the hub strips edge resources by name -- so a new resource named for a thing
// the hub already runs silently takes ownership of the hub's copy. That is how
// PodMonitor/nats replaced the NATS chart's own monitor and blinded hub
// JetStream metrics: same name, same namespace, selector matching nothing.
//
// An edge- prefix makes the collision impossible rather than relying on the
// strip list in datum-cloud/infra staying in step with this file.
func TestEdgeOnlyResourcesCannotCollideOnTheHub(t *testing.T) {
	const path = "collectors/nats.yaml"

	dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "" || doc.Metadata.Name == "" {
			continue
		}

		id := doc.Kind + "/" + doc.Metadata.Name
		if strippedByHub[id] || strings.HasPrefix(doc.Metadata.Name, "edge-") {
			continue
		}
		t.Errorf("%s: %s is neither stripped by the hub nor named edge-*; it will be applied to the hub, where it may take over a resource of the same name",
			path, id)
	}
}

// TestEdgeNATSExporterIsScraped guards the edge broker's metrics path. vmagent
// discovers targets from PodMonitors, not from prometheus.io annotations, so
// without one the exporter serves :7777 to nobody and every edge NATS panel
// reads empty -- indistinguishable from an idle broker. A port name that does
// not resolve to a container port fails the same silent way.
func TestEdgeNATSExporterIsScraped(t *testing.T) {
	const path = "collectors/nats.yaml"

	var (
		monitorPorts []string
		podLabels    map[string]string
		portNames    = map[string]bool{}
		selector     map[string]string
	)

	dec := yaml.NewDecoder(strings.NewReader(readFile(t, path)))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				// PodMonitor
				Selector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"selector"`
				PodMetricsEndpoints []struct {
					Port string `yaml:"port"`
				} `yaml:"podMetricsEndpoints"`
				// Deployment
				Template struct {
					Metadata struct {
						Labels map[string]string `yaml:"labels"`
					} `yaml:"metadata"`
					Spec struct {
						Containers []struct {
							Ports []struct {
								Name string `yaml:"name"`
							} `yaml:"ports"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}

		switch doc.Kind {
		case "PodMonitor":
			selector = doc.Spec.Selector.MatchLabels
			for _, ep := range doc.Spec.PodMetricsEndpoints {
				monitorPorts = append(monitorPorts, ep.Port)
			}
		case "Deployment":
			podLabels = doc.Spec.Template.Metadata.Labels
			for _, c := range doc.Spec.Template.Spec.Containers {
				for _, p := range c.Ports {
					portNames[p.Name] = true
				}
			}
		}
	}

	if len(monitorPorts) == 0 {
		t.Fatalf("%s: no PodMonitor; the exporter sidecar is never scraped", path)
	}

	if len(selector) == 0 {
		t.Errorf("%s: PodMonitor selects on no labels", path)
	}
	for k, v := range selector {
		if podLabels[k] != v {
			t.Errorf("%s: PodMonitor selects %s=%q, but the nats pod template has %s=%q",
				path, k, v, k, podLabels[k])
		}
	}

	for _, port := range monitorPorts {
		if !portNames[port] {
			t.Errorf("%s: PodMonitor scrapes port %q, which no container declares", path, port)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(raw)
}
