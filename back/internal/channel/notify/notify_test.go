package notify

import (
	"encoding/json"
	"testing"
)

func TestResolveEmptyIsToday(t *testing.T) {
	// A row that predates the column behaves exactly as the product did before
	// the settings existed: pages on, everything else off.
	for _, raw := range [][]byte{nil, []byte(`{}`)} {
		s := Resolve(raw)
		if !s.WebsiteDown || s.ErrorLogs || s.RepeatingErrorLogs || s.ResolveFollowUp {
			t.Fatalf("Resolve(%q) = %+v, want defaults", raw, s)
		}
		if s.RepeatWindowMin != 5 {
			t.Fatalf("default window = %d, want 5", s.RepeatWindowMin)
		}
	}
}

func TestResolveSparseKeepsAbsentDefaults(t *testing.T) {
	s := Resolve([]byte(`{"errorLogs":true}`))
	if !s.ErrorLogs {
		t.Fatal("present key must apply")
	}
	if !s.WebsiteDown {
		t.Fatal("absent websiteDown must keep its default (true)")
	}
}

func TestResolveClampsWindow(t *testing.T) {
	if got := Resolve([]byte(`{"repeatWindowMin":0}`)).RepeatWindowMin; got != 1 {
		t.Fatalf("clamp low: %d", got)
	}
	if got := Resolve([]byte(`{"repeatWindowMin":10000}`)).RepeatWindowMin; got != 120 {
		t.Fatalf("clamp high: %d", got)
	}
}

func TestErrorLogCategoriesAreExclusive(t *testing.T) {
	// One axis, three states: turning one category on turns the other off,
	// whichever key the client happened to send.
	current := Resolve([]byte(`{"errorLogs":true}`))
	var p Patch
	_ = json.Unmarshal([]byte(`{"repeatingErrorLogs":true}`), &p)
	next := p.Apply(current)
	if next.ErrorLogs || !next.RepeatingErrorLogs {
		t.Fatalf("repeating on must switch errorLogs off, got %+v", next)
	}

	p = Patch{}
	_ = json.Unmarshal([]byte(`{"errorLogs":true}`), &p)
	next = p.Apply(next)
	if !next.ErrorLogs || next.RepeatingErrorLogs {
		t.Fatalf("errorLogs on must switch repeating off, got %+v", next)
	}

	// A hand-posted row carrying both resolves to the stricter reading.
	both := Resolve([]byte(`{"errorLogs":true,"repeatingErrorLogs":true}`))
	if both.ErrorLogs || !both.RepeatingErrorLogs {
		t.Fatalf("both stored must resolve to repeating only, got %+v", both)
	}
}

func TestPatchOverlaysOnlyPresentFields(t *testing.T) {
	current := Resolve([]byte(`{"errorLogs":true,"repeatWindowMin":10}`))
	var p Patch
	if err := json.Unmarshal([]byte(`{"websiteDown":false}`), &p); err != nil {
		t.Fatal(err)
	}
	next := p.Apply(current)
	if next.WebsiteDown {
		t.Fatal("sent field must apply")
	}
	if !next.ErrorLogs || next.RepeatWindowMin != 10 {
		t.Fatal("toggling one checkbox must not reset the others")
	}
}
