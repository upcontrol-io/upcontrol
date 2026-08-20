package migrate

import (
	"strings"
	"testing"
)

func TestSplitStatements_Basics(t *testing.T) {
	got := splitStatements("CREATE TABLE a (x Int8);\nCREATE TABLE b (y String);")
	if len(got) != 2 {
		t.Fatalf("expected 2 statements, got %d: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a") || !strings.HasPrefix(got[1], "CREATE TABLE b") {
		t.Fatalf("statements split wrong: %v", got)
	}
}

func TestSplitStatements_StripsCommentsAndBlanks(t *testing.T) {
	// A -- line comment must not survive as its own statement, and trailing
	// empties are dropped (so we never Exec an empty string).
	got := splitStatements("-- header comment\nCREATE TABLE a (x Int8); -- trailing\n\n; ;")
	for _, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("no empty statements allowed, got %q in %v", s, got)
		}
		if strings.Contains(s, "--") {
			t.Fatalf("line comments must be stripped, got %q", s)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected a single real statement, got %d: %v", len(got), got)
	}
}

func TestSplitStatements_HandlesMultiLineDDL(t *testing.T) {
	src := `CREATE TABLE IF NOT EXISTS logs (
  ts DateTime,
  level String
) ENGINE = MergeTree ORDER BY ts;
ALTER TABLE logs ADD INDEX IF NOT EXISTS idx_level level TYPE set(16);`
	got := splitStatements(src)
	if len(got) != 2 {
		t.Fatalf("expected CREATE + ALTER, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "ENGINE = MergeTree") {
		t.Fatalf("multi-line statement must stay whole: %q", got[0])
	}
}
