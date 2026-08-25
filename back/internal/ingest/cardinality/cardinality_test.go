package cardinality

import (
	"strconv"
	"testing"
)

func TestAddUnderCeiling(t *testing.T) {
	l := New(3)
	for _, h := range []string{"a", "b", "c"} {
		store, warned := l.Add("host", h)
		if store != h || warned {
			t.Errorf("Add(%q) = (%q, %v), want (%q, false)", h, store, warned, h)
		}
	}
}

func TestCeilingCapsAndWarnsOnce(t *testing.T) {
	l := New(3) // ceiling 3
	l.Add("host", "a")
	l.Add("host", "b")
	l.Add("host", "c")
	// 4th distinct value is the one that crosses: it becomes the sentinel AND
	// warns once. The stored dictionary is now exactly ceiling real + __over__.
	store, warned := l.Add("host", "d")
	if store != Sentinel {
		t.Errorf("tipping value = %q, want sentinel", store)
	}
	if !warned {
		t.Error("expected warned=true the first time the field tips over")
	}
	// 5th distinct value is also over: sentinel again, no repeat warning.
	store, warned = l.Add("host", "e")
	if store != Sentinel {
		t.Errorf("over-cap value = %q, want %q", store, Sentinel)
	}
	if warned {
		t.Error("warned should not repeat on subsequent over-cap values")
	}
}

// With a 1000 ceiling, feeding 5000 distinct hosts must leave the stored set
// bounded at ceiling+1 and fire the warning.
func TestGate5000HostsDictionaryDoesNotGrow(t *testing.T) {
	l := New(1000)
	warnedAny := false
	stored := map[string]struct{}{}
	for i := 0; i < 5000; i++ {
		h := "host-" + strconv.Itoa(i)
		s, warned := l.Add("host", h)
		stored[s] = struct{}{}
		if warned {
			warnedAny = true
		}
	}
	if !warnedAny {
		t.Fatal("no cardinality_capped warning fired for 5000 hosts")
	}
	// The stored dictionary is the 1000 real values plus the sentinel: at most
	// ceiling+1 distinct strings reach ClickHouse.
	if len(stored) > 1001 {
		t.Errorf("stored dictionary has %d distinct values, want <= 1001 (ceiling+sentinel)", len(stored))
	}
	if _, ok := stored[Sentinel]; !ok {
		t.Error("sentinel __over__ never stored — over-cap values were not collapsed")
	}
}

func TestKnownValuePassesAfterCap(t *testing.T) {
	// Even after capping, a value that earned a slot earlier must still pass.
	l := New(2)
	l.Add("host", "a")
	l.Add("host", "b")
	l.Add("host", "c") // tips over
	store, _ := l.Add("host", "new")
	if store != Sentinel {
		t.Errorf("new over-cap = %q, want sentinel", store)
	}
	store, _ = l.Add("host", "a") // known
	if store != "a" {
		t.Errorf("known value after cap = %q, want a", store)
	}
}

func TestFieldsAreIndependent(t *testing.T) {
	l := New(2)
	l.Add("host", "a")
	l.Add("host", "b")
	// The `service` field has its own ceiling.
	s, _ := l.Add("service", "api")
	if s != "api" {
		t.Errorf("independent field capped: %q", s)
	}
}
