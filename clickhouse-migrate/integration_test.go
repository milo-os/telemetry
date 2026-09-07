package main

import (
	"database/sql"
	"os"
	"testing"

	clickhousego "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/require"
)

// TestApplyMigrations_FreshDatabase applies the repository's real migration
// set to an empty database, which is what the clickhouse-migrate Job does on
// a brand-new ClickHouse instance.
//
// It needs a live server, so it is skipped unless CLICKHOUSE_TEST_ADDR is set
// (see `task clickhouse-migrate:test-integration`, which starts one).
func TestApplyMigrations_FreshDatabase(t *testing.T) {
	addr := os.Getenv("CLICKHOUSE_TEST_ADDR")
	if addr == "" {
		t.Skip("CLICKHOUSE_TEST_ADDR not set")
	}

	const database = "o11y_migrate_test"
	const queryapiUser = "queryapi"

	renderedDir := t.TempDir()
	require.NoError(t, renderMigrations(
		"../config/clickhouse-migrations/migrations",
		renderedDir,
		map[string]string{"QUERYAPI_USER": queryapiUser},
	))

	cfg := config{
		database:      database,
		migrationsDir: renderedDir,
	}

	opts := &clickhousego.Options{
		Addr: []string{addr},
		Auth: clickhousego.Auth{
			Username: envOr("CLICKHOUSE_TEST_USER", "default"),
			Password: os.Getenv("CLICKHOUSE_TEST_PASSWORD"),
		},
	}

	bootstrap := clickhousego.OpenDB(opts)
	t.Cleanup(func() { require.NoError(t, bootstrap.Close()) })

	_, err := bootstrap.Exec("DROP DATABASE IF EXISTS " + quoteIdentifier(database))
	require.NoError(t, err)
	_, err = bootstrap.Exec("CREATE DATABASE " + quoteIdentifier(database))
	require.NoError(t, err)

	// ssl_auth.xml's certificate mapping is absent here, so create the users the
	// policy attaches to.
	createTestUser(t, bootstrap, "ops")
	createTestUser(t, bootstrap, queryapiUser)

	scoped := *opts
	scoped.Auth.Database = database
	db := clickhousego.OpenDB(&scoped)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	version, dirty, err := applyMigrations(db, cfg)
	require.NoError(t, err)
	require.False(t, dirty, "migrations left the database dirty at version %d", version)
	require.Equal(t, uint(1), version)

	version, dirty, err = applyMigrations(db, cfg)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, uint(1), version)

	var count uint64
	require.NoError(t, db.QueryRow(
		"SELECT count() FROM system.tables WHERE database = ? AND name = 'logs'",
		database,
	).Scan(&count))
	require.Equal(t, uint64(1), count, "expected the logs table to exist")

	assertRestrictedQueryapi(t, db, database)
}

// assertRestrictedQueryapi verifies the 000001 migration installed the queryapi
// row policy. Grants are deliberately not asserted: the real identity lives in
// users_xml, which SQL cannot GRANT to, so this test's SQL user can't model it.
func assertRestrictedQueryapi(t *testing.T, db *sql.DB, database string) {
	t.Helper()

	var policies uint64
	require.NoError(t, db.QueryRow(
		"SELECT count() FROM system.row_policies WHERE database = ? AND table = 'logs' AND short_name = 'queryapi_project_isolation'",
		database,
	).Scan(&policies))
	require.Equal(t, uint64(1), policies, "expected the queryapi_project_isolation row policy on logs")
}

// createTestUser creates a throwaway user for integration testing, dropping any
// leftover from a previous run first.
func createTestUser(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	for _, stmt := range []string{
		"DROP USER IF EXISTS " + name,
		"CREATE USER " + name + " IDENTIFIED WITH no_password",
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "exec %q", stmt)
	}
}

