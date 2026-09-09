package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var unresolvedTokenRe = regexp.MustCompile(`\{\{[A-Z_]+\}\}`)

// renderMigrations copies every *.sql file from srcDir into dstDir,
// substituting {{KEY}} tokens from vars. It fails loudly -- rather than
// applying a migration with a literal, unsubstituted {{TOKEN}} in it -- if
// any placeholder survives substitution.
func renderMigrations(srcDir, dstDir string, vars map[string]string) error {
	files, err := filepath.Glob(filepath.Join(srcDir, "*.sql"))
	if err != nil {
		return fmt.Errorf("clickhouse-migrate: glob %s: %w", srcDir, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("clickhouse-migrate: no *.sql files found in %s", srcDir)
	}

	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("clickhouse-migrate: read %s: %w", file, err)
		}

		rendered := string(content)
		for key, value := range vars {
			rendered = strings.ReplaceAll(rendered, "{{"+key+"}}", value)
		}

		if m := unresolvedTokenRe.FindString(rendered); m != "" {
			return fmt.Errorf("clickhouse-migrate: %s: unresolved placeholder %s -- set the matching env var", filepath.Base(file), m)
		}

		dst := filepath.Join(dstDir, filepath.Base(file))
		if err := os.WriteFile(dst, []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("clickhouse-migrate: write %s: %w", dst, err)
		}
	}

	return nil
}
