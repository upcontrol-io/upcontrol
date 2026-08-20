//go:build contract

// Package api_test contains the contract test (build-plan §2.7): each golden
// JSON fixture — taken verbatim from front/src/lib/mockData.ts — is round-tripped
// through the generated Go types. If a field in mockData does not fit a generated
// struct, the round-trip loses it and the test fails. That is the contract: the
// generated server speaks the same shapes the front already renders.
//
// Run: go test -tags=contract ./internal/api/...
package api_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	apigen "go.upcontrol.io/back/gen/api"
)

// canonical re-serializes JSON compactly so key order and whitespace never
// cause a diff: only shape and value matter for the contract.
func canonical(t *testing.T, b []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonical: unmarshal %q: %v", b, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonical: marshal: %v", err)
	}
	return out
}

func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// roundTrip unmarshals golden into a generated-type value, re-marshals it, and
// compares the canonical forms. The ptrTy param lets the caller pass a fresh
// instance of the generated type (e.g. &apigen.Account{}).
func roundTrip[T any](t *testing.T, name string, golden []byte, ptrTy T) {
	t.Helper()
	if err := json.Unmarshal(golden, ptrTy); err != nil {
		t.Fatalf("%s: golden does not fit the generated type: %v", name, err)
	}
	remarshalled, err := json.Marshal(ptrTy)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	// Compare structurally on canonical JSON: a field dropped by the Go type, or
	// a value that changed type, is a contract break.
	want, got := canonical(t, golden), canonical(t, remarshalled)
	if !bytes.Equal(want, got) {
		t.Errorf("%s: round-trip changed the shape\n%s", name,
			cmp.Diff(string(want), string(got)))
	}
}

func TestAccountContract(t *testing.T) {
	g := loadGolden(t, "account.golden.json")
	roundTrip(t, "account", g, &apigen.Account{})
}

func TestMonitorContract(t *testing.T) {
	g := loadGolden(t, "monitor.golden.json")
	roundTrip(t, "monitor", g, &apigen.Monitor{})
}

func TestPlanContract(t *testing.T) {
	g := loadGolden(t, "plan.golden.json")
	roundTrip(t, "plan", g, &apigen.PlanResponse{})
}

// TestGoldenFilesParse guards against committing a malformed fixture: every
// golden must be valid JSON, parseable on its own. (Field order is preserved from
// mockData for readability, so we do not require alphabetical key order.)
func TestGoldenFilesParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", filepath.Base(f), err)
		}
	}
}
