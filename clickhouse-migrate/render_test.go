package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderMigrations_Substitutes(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(src, "0001_init.up.sql"),
		[]byte("GRANT SELECT ON logs TO `{{QUERYAPI_USER}}`;\n"),
		0o644,
	))

	require.NoError(t, renderMigrations(src, dst, map[string]string{"QUERYAPI_USER": "queryapi-clickhouse-client"}))

	got, err := os.ReadFile(filepath.Join(dst, "0001_init.up.sql"))
	require.NoError(t, err)
	require.Equal(t, "GRANT SELECT ON logs TO `queryapi-clickhouse-client`;\n", string(got))
}

func TestRenderMigrations_EmptySrcDirErrors(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	err := renderMigrations(src, dst, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), src)
}

func TestRenderMigrations_MissingSrcDirErrors(t *testing.T) {
	src := filepath.Join(t.TempDir(), "does-not-exist")
	dst := t.TempDir()

	err := renderMigrations(src, dst, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), src)
}

func TestRenderMigrations_UnresolvedToken(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(src, "0001_init.up.sql"),
		[]byte("GRANT SELECT ON logs TO `{{QUERYAPI_USER}}`;\n"),
		0o644,
	))

	err := renderMigrations(src, dst, map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "QUERYAPI_USER")
	require.Contains(t, err.Error(), "0001_init.up.sql")
}
