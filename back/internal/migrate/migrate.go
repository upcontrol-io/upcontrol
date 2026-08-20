// Package migrate applies database schema migrations for both Postgres (via
// goose, with Up/Down) and ClickHouse (via raw SQL, idempotent IF NOT EXISTS).
// Called by `ucapi migrate` BEFORE the app starts — plan §1.3 requires migrations
// applied by command, not at startup, so two instances don't race.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pressly/goose/v3"

	"go.upcontrol.io/back/internal/storage/ch"

	// pgx stdlib driver for database/sql (goose needs *sql.DB).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Run applies all pending migrations for both databases.
func Run(ctx context.Context, pgURL, chAddr, chDB, chUser, chPass, pgDir, chDir string) error {
	// --- Postgres migrations (goose) ---
	if err := runPostgres(ctx, pgURL, pgDir); err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
	}

	// --- ClickHouse migrations (raw SQL, idempotent) ---
	if err := runClickHouse(ctx, chAddr, chDB, chUser, chPass, chDir); err != nil {
		return fmt.Errorf("clickhouse migrations: %w", err)
	}

	return nil
}

func runPostgres(ctx context.Context, pgURL, migrationsDir string) error {
	if pgURL == "" {
		return nil
	}
	if migrationsDir == "" {
		migrationsDir = "../../db/postgres"
	}
	db, err := sql.Open("pgx", pgURL)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, migrationsDir)
}

func runClickHouse(ctx context.Context, addr, db, user, pass, migrationsDir string) error {
	if addr == "" {
		return nil
	}
	if migrationsDir == "" {
		migrationsDir = "../../db/clickhouse"
	}

	// Find all .sql files in order.
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, filepath.Join(migrationsDir, e.Name()))
		}
	}
	sort.Strings(files)

	// Apply via the native clickhouse-go driver — no external binary, so the
	// image needs no clickhouse-client package and the password never reaches
	// a process argument list. Each statement runs in its own Exec: the native
	// protocol parses one statement per call, and IF NOT EXISTS makes a repeat
	// run idempotent.
	conn, err := ch.Open(ctx, ch.Options{
		Addr: []string{addr}, Database: db, Username: user, Password: pass,
	})
	if err != nil {
		return fmt.Errorf("open clickhouse: %w", err)
	}
	defer func() { _ = conn.Close() }()
	d := conn.Raw()

	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(f), err)
		}
		for n, stmt := range splitStatements(string(content)) {
			if err := d.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s statement %d: %w\n%s", filepath.Base(f), n+1, err, stmt)
			}
		}
	}
	return nil
}

// splitStatements breaks a migration file into individual SQL statements. Our
// migrations carry no string literals, so dropping -- line comments and then
// splitting on ';' is sufficient and dependency-free.
func splitStatements(content string) []string {
	var b strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	var out []string
	for _, s := range strings.Split(b.String(), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
