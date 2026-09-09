// SPDX-License-Identifier: AGPL-3.0-only

package clickhouse

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// Config is the ClickHouse connection configuration. Names and defaults match
// clickhouse-migrate so one deployment convention covers both.
type Config struct {
	Host        string
	Port        int
	User        string
	Database    string
	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string
}

// ConfigFromEnv reads Config from the environment.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Host:        os.Getenv("CLICKHOUSE_HOST"),
		Port:        9440,
		User:        envOr("CLICKHOUSE_USER", "queryapi"),
		Database:    envOr("CLICKHOUSE_DATABASE", "o11y"),
		TLSCertFile: envOr("CLICKHOUSE_TLS_CERT_FILE", "/etc/clickhouse-client/certs/tls.crt"),
		TLSKeyFile:  envOr("CLICKHOUSE_TLS_KEY_FILE", "/etc/clickhouse-client/certs/tls.key"),
		TLSCAFile:   envOr("CLICKHOUSE_TLS_CA_FILE", "/etc/clickhouse-client/certs/ca.crt"),
	}
	if cfg.Host == "" {
		return Config{}, errors.New("clickhouse: CLICKHOUSE_HOST is required")
	}
	if raw := os.Getenv("CLICKHOUSE_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("clickhouse: invalid CLICKHOUSE_PORT %q: %w", raw, err)
		}
		cfg.Port = port
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// tlsConfig builds the client-certificate TLS config. Authentication is by
// certificate, not password, matching the ssl_auth.xml user on the server.
func (c Config) tlsConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: load client certificate: %w", err)
	}

	pool := x509.NewCertPool()
	ca, err := os.ReadFile(c.TLSCAFile)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read CA: %w", err)
	}
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("clickhouse: CA file contained no certificates")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
