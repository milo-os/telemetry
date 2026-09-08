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

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(raw)
}
