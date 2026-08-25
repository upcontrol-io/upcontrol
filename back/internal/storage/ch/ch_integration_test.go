//go:build integration

// Integration test for storage/ch: real ClickHouse via testcontainers, insert
// and read back log rows. Run: go test -tags=integration ./internal/storage/ch/...
package ch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	chUser = "upcontrol"
	chPass = "secret"
	chDB   = "upcontrol"
)

func startClickHouse(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "clickhouse/clickhouse-server:latest",
		ExposedPorts: []string{"9000/tcp", "8123/tcp"},
		Env: map[string]string{
			"CLICKHOUSE_DB":       chDB,
			"CLICKHOUSE_USER":     chUser,
			"CLICKHOUSE_PASSWORD": chPass,
		},
		// Wait on the HTTP interface with auth: the native port accepts
		// connections during boot before CH can answer the protocol.
		WaitingFor: wait.ForHTTP("/?query=SELECT%201").WithPort("8123/tcp").
			WithBasicAuth(chUser, chPass).
			WithStartupTimeout(90 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	// PortEndpoint returns "host:port" as a string, sidestepping the nat.Port /
	// network.Port type collision across docker client versions.
	endpoint, err := c.PortEndpoint(ctx, "9000", "")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// applyMigration execs the Up block of db/clickhouse/001_init.sql statement by
// statement. The native driver runs one statement per Exec.
func applyMigration(t *testing.T, c *Conn) {
	t.Helper()
	// Resolve the migration file relative to back/ (the test cwd).
	path := filepath.Join("..", "..", "..", "..", "db", "clickhouse", "001_init.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	up := splitUp(string(body))
	for _, stmt := range splitStatements(up) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := c.Raw().Exec(context.Background(), stmt); err != nil {
			t.Fatalf("exec migration statement (%s…): %v", firstLine(stmt), err)
		}
	}
}

func splitUp(s string) string {
	i := strings.Index(s, "-- +goose StatementBegin")
	if i < 0 {
		return s
	}
	rest := s[i+len("-- +goose StatementBegin"):]
	j := strings.Index(rest, "-- +goose Down")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

func splitStatements(s string) []string {
	// Strip whole comment lines first: the migration's header comment contains
	// ';' (e.g. "logs is the ring-displaced table;"), which would split mid-comment.
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Split(strings.Join(keep, "\n"), ";")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestInsertLogsRoundTrip(t *testing.T) {
	endpoint := startClickHouse(t)
	c, err := Open(context.Background(), Options{
		Addr: []string{endpoint}, Database: chDB, Username: chUser, Password: chPass,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	applyMigration(t, c)

	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := []LogRow{
		{TenantID: 1, ProjectID: 1, TS: now, Seq: 1, Source: "sdk", Service: "app",
			Host: "h1", Level: "error", Message: "boom", Attrs: map[string]string{"k": "v"}},
		{TenantID: 1, ProjectID: 1, TS: now, Seq: 2, Source: "sdk", Service: "app",
			Host: "h1", Level: "info", Message: "ok"},
	}
	if err := c.InsertLogs(context.Background(), rows); err != nil {
		t.Fatalf("InsertLogs: %v", err)
	}

	// ClickHouse merges parts asynchronously; give the count a beat.
	var got uint64
	for i := 0; i < 20; i++ {
		if err := c.Raw().QueryRow(context.Background(),
			"SELECT count() FROM logs WHERE tenant_id=1").Scan(&got); err != nil {
			t.Fatalf("count: %v", err)
		}
		if got == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got != 2 {
		t.Fatalf("row count = %d, want 2", got)
	}

	var msg, lvl string
	var attrs map[string]string
	if err := c.Raw().QueryRow(context.Background(),
		"SELECT message, level, attrs FROM logs WHERE seq=1").Scan(&msg, &lvl, &attrs); err != nil {
		t.Fatalf("select seq=1: %v", err)
	}
	if msg != "boom" || lvl != "error" {
		t.Errorf("seq=1 got msg=%q lvl=%q", msg, lvl)
	}
	if attrs["k"] != "v" {
		t.Errorf("attrs = %v, want k=v", attrs)
	}
}
