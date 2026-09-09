package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "")
	t.Setenv("CLICKHOUSE_USER", "")
	t.Setenv("CLICKHOUSE_DATABASE", "")

	_, err := configFromEnv()
	require.Error(t, err)
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "clickhouse.example")
	t.Setenv("CLICKHOUSE_USER", "clickhouse-migrations-client")
	t.Setenv("CLICKHOUSE_DATABASE", "o11y")

	cfg, err := configFromEnv()
	require.NoError(t, err)
	require.Equal(t, "clickhouse.example", cfg.host)
	require.Equal(t, "9440", cfg.port)
	require.Equal(t, "/migrations", cfg.migrationsDir)
	require.Equal(t, "/etc/clickhouse-client/certs/tls.crt", cfg.tlsCertFile)
	require.Equal(t, "queryapi", cfg.queryapiUser)
}

func TestConfigFromEnv_RejectsMaliciousQueryapiUser(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "clickhouse.example")
	t.Setenv("CLICKHOUSE_USER", "clickhouse-migrations-client")
	t.Setenv("CLICKHOUSE_DATABASE", "o11y")

	for name, value := range map[string]string{
		"backtick":  "queryapi`; DROP TABLE x --",
		"semicolon": "queryapi; DROP TABLE x",
		"space":     "query api",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLICKHOUSE_QUERYAPI_USER", value)
			_, err := configFromEnv()
			require.Error(t, err)
		})
	}
}

func TestConfigFromEnv_AcceptsRealQueryapiUsers(t *testing.T) {
	t.Setenv("CLICKHOUSE_HOST", "clickhouse.example")
	t.Setenv("CLICKHOUSE_USER", "clickhouse-migrations-client")
	t.Setenv("CLICKHOUSE_DATABASE", "o11y")

	for _, value := range []string{"queryapi", "queryapi-clickhouse-client", "queryapi_user"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CLICKHOUSE_QUERYAPI_USER", value)
			cfg, err := configFromEnv()
			require.NoError(t, err)
			require.Equal(t, value, cfg.queryapiUser)
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	require.Equal(t, "`o11y`", quoteIdentifier("o11y"))
	require.Equal(t, "`o11y``; DROP TABLE x`", quoteIdentifier("o11y`; DROP TABLE x"))
}

func TestLoadClientTLSConfig(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := writeTestCert(t, dir)

	tlsConfig, err := loadClientTLSConfig(certFile, keyFile, caFile)
	require.NoError(t, err)
	require.Len(t, tlsConfig.Certificates, 1)
	require.NotNil(t, tlsConfig.RootCAs)
}

func TestLoadClientTLSConfig_MissingFiles(t *testing.T) {
	_, err := loadClientTLSConfig("/nonexistent/tls.crt", "/nonexistent/tls.key", "/nonexistent/ca.crt")
	require.Error(t, err)
}

// writeTestCert generates a minimal self-signed cert/key pair and writes it
// (plus its own PEM as the "CA") to dir, returning the three file paths.
func writeTestCert(t *testing.T, dir string) (certFile, keyFile, caFile string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	caFile = filepath.Join(dir, "ca.crt")

	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	return certFile, keyFile, caFile
}
