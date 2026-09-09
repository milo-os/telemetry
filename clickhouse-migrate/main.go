// Command clickhouse-migrate applies golang-migrate SQL migrations to a
// ClickHouse database, authenticating via a client TLS certificate rather
// than a password (see users.d/ssl_auth.xml on the ClickHouse side).
package main

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	clickhousego "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-migrate/migrate/v4"
	chmigrate "github.com/golang-migrate/migrate/v4/database/clickhouse"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

type config struct {
	host            string
	port            string
	username        string
	database        string
	migrationsDir   string
	tlsCertFile     string
	tlsKeyFile      string
	tlsCAFile       string
	migrationsTable string
	queryapiUser    string
}

// safeIdentifierRe restricts CLICKHOUSE_QUERYAPI_USER to characters that
// cannot break out of the backtick-quoted identifier it's spliced into by
// renderMigrations. It permits the deployed value
// "queryapi-clickhouse-client" and the default "queryapi".
var safeIdentifierRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

func configFromEnv() (config, error) {
	c := config{
		host:            os.Getenv("CLICKHOUSE_HOST"),
		port:            envOr("CLICKHOUSE_PORT", "9440"),
		username:        os.Getenv("CLICKHOUSE_USER"),
		database:        os.Getenv("CLICKHOUSE_DATABASE"),
		migrationsDir:   envOr("MIGRATIONS_DIR", "/migrations"),
		tlsCertFile:     envOr("CLICKHOUSE_TLS_CERT_FILE", "/etc/clickhouse-client/certs/tls.crt"),
		tlsKeyFile:      envOr("CLICKHOUSE_TLS_KEY_FILE", "/etc/clickhouse-client/certs/tls.key"),
		tlsCAFile:       envOr("CLICKHOUSE_TLS_CA_FILE", "/etc/clickhouse-client/certs/ca.crt"),
		migrationsTable: envOr("MIGRATIONS_TABLE", chmigrate.DefaultMigrationsTable),
		queryapiUser:    envOr("CLICKHOUSE_QUERYAPI_USER", "queryapi"),
	}

	var missing []string
	for name, v := range map[string]string{
		"CLICKHOUSE_HOST":     c.host,
		"CLICKHOUSE_USER":     c.username,
		"CLICKHOUSE_DATABASE": c.database,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required env vars: %v", missing)
	}

	if !safeIdentifierRe.MatchString(c.queryapiUser) {
		return config{}, fmt.Errorf("invalid CLICKHOUSE_QUERYAPI_USER %q: must match %s", c.queryapiUser, safeIdentifierRe.String())
	}

	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadClientTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client cert/key: %w", err)
	}

	caBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("no certificates found in CA file %s", caFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
	}, nil
}

// ensureDatabaseExists creates cfg.database if it doesn't already exist, e.g.
// on a brand-new ClickHouse instance. It connects without pinning the session
// to that database: clickhouse-go validates Auth.Database at connect time, so
// a connection already scoped to a not-yet-created database would fail before
// any CREATE DATABASE could run.
func ensureDatabaseExists(cfg config, tlsConfig *tls.Config) (err error) {
	bootstrapDB := clickhousego.OpenDB(&clickhousego.Options{
		Addr: []string{fmt.Sprintf("%s:%s", cfg.host, cfg.port)},
		Auth: clickhousego.Auth{
			Username: cfg.username,
		},
		TLS: tlsConfig,
	})
	defer func() {
		err = errors.Join(err, bootstrapDB.Close())
	}()

	_, err = bootstrapDB.Exec("CREATE DATABASE IF NOT EXISTS " + quoteIdentifier(cfg.database))
	return err
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// applyMigrations brings db up to the latest migration in cfg.migrationsDir
// and reports the resulting version. db must already be scoped to
// cfg.database. Reaching the latest version already is not an error.
func applyMigrations(db *sql.DB, cfg config) (uint, bool, error) {
	driver, err := chmigrate.WithInstance(db, &chmigrate.Config{
		DatabaseName:    cfg.database,
		MigrationsTable: cfg.migrationsTable,
		// ClickHouse rejects multi-statement queries, so the driver must split
		// migration files on ";" itself. Without this a multi-statement file
		// fails after migrate flags the version dirty, blocking every later run.
		MultiStatementEnabled: true,
	})
	if err != nil {
		return 0, false, fmt.Errorf("initializing clickhouse migrate driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance("file://"+cfg.migrationsDir, "clickhouse", driver)
	if err != nil {
		return 0, false, fmt.Errorf("initializing migrate instance: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, false, fmt.Errorf("applying migrations: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil {
		return 0, false, fmt.Errorf("reading migration version: %w", err)
	}
	return version, dirty, nil
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := configFromEnv()
	if err != nil {
		return err
	}

	renderedDir, err := os.MkdirTemp("", "clickhouse-migrate-rendered-")
	if err != nil {
		return fmt.Errorf("creating rendered migrations dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(renderedDir); err != nil {
			logger.Error("removing rendered migrations dir", "error", err)
		}
	}()
	if err := renderMigrations(cfg.migrationsDir, renderedDir, map[string]string{
		"QUERYAPI_USER": cfg.queryapiUser,
	}); err != nil {
		return err
	}
	cfg.migrationsDir = renderedDir

	tlsConfig, err := loadClientTLSConfig(cfg.tlsCertFile, cfg.tlsKeyFile, cfg.tlsCAFile)
	if err != nil {
		return fmt.Errorf("loading TLS config: %w", err)
	}

	logger.Info("ensuring database exists", "database", cfg.database)
	if err := ensureDatabaseExists(cfg, tlsConfig); err != nil {
		return fmt.Errorf("ensuring database exists: %w", err)
	}

	db := clickhousego.OpenDB(&clickhousego.Options{
		Addr: []string{fmt.Sprintf("%s:%s", cfg.host, cfg.port)},
		Auth: clickhousego.Auth{
			Database: cfg.database,
			Username: cfg.username,
		},
		TLS: tlsConfig,
	})
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("closing clickhouse connection", "error", err)
		}
	}()

	logger.Info("applying migrations", "database", cfg.database, "migrations_dir", cfg.migrationsDir)
	version, dirty, err := applyMigrations(db, cfg)
	if err != nil {
		return err
	}
	logger.Info("migrations applied", "version", version, "dirty", dirty)
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("clickhouse-migrate failed", "error", err)
		os.Exit(1)
	}
}
