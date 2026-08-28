// Package migrate applies the Postgres schema migrations (goose, Up/Down);
// invoked by `ucapi migrate` before the app starts.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"

	// pgx stdlib driver for database/sql (goose needs *sql.DB).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Run applies all pending Postgres migrations.
func Run(ctx context.Context, pgURL, pgDir string) error {
	if err := runPostgres(ctx, pgURL, pgDir); err != nil {
		return fmt.Errorf("postgres migrations: %w", err)
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
