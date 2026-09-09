// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse_test

import (
	"testing"

	"go.datum.net/o11y/queryapi/internal/storage/clickhouse"
)

func TestConfigFromEnvRequiresHost(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "")
	if _, err := clickhouse.ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv with no host succeeded, want error")
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "clickhouse.example")
	cfg, err := clickhouse.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Port != 9440 {
		t.Errorf("Port = %d, want 9440", cfg.Port)
	}
	if cfg.User != "queryapi" {
		t.Errorf("User = %q, want queryapi", cfg.User)
	}
	if cfg.Database != "o11y" {
		t.Errorf("Database = %q, want o11y", cfg.Database)
	}
	if cfg.TLSCertFile != "/etc/clickhouse-client/certs/tls.crt" {
		t.Errorf("TLSCertFile = %q", cfg.TLSCertFile)
	}
}

func TestConfigFromEnvInvalidPort(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "clickhouse.example")
	t.Setenv("CLICKHOUSE_PORT", "not-a-port")
	if _, err := clickhouse.ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv with a bad port succeeded, want error")
	}
}

// TestNewRequiresLoadableCertificates pins the fail-closed contract: a missing
// client certificate must be an error, never a silent downgrade to plaintext.
func TestNewRequiresLoadableCertificates(t *testing.T) {
	_, err := clickhouse.New(clickhouse.Config{
		Host: "clickhouse.example", Port: 9440, User: "queryapi", Database: "o11y",
		TLSCertFile: "/nonexistent/tls.crt",
		TLSKeyFile:  "/nonexistent/tls.key",
		TLSCAFile:   "/nonexistent/ca.crt",
	})
	if err == nil {
		t.Fatal("New with unreadable certificates succeeded, want error")
	}
}
