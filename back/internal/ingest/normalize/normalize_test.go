package normalize

import "testing"

// TestDictionarySize pins the frozen dictionary at exactly 24 events. Adding or
// removing a name is a major version (plan §4.3); this test makes that explicit.
func TestDictionarySize(t *testing.T) {
	if got := len(canonical); got != 24 {
		t.Fatalf("canonical dictionary has %d events, want exactly 24", got)
	}
	// And exactly the right tier counts.
	counts := map[Tier]int{}
	for _, e := range canonical {
		counts[e.tier]++
	}
	if counts[Tier1] != 12 {
		t.Errorf("T1 = %d, want 12", counts[Tier1])
	}
	if counts[Tier2] != 10 {
		t.Errorf("T2 = %d, want 10", counts[Tier2])
	}
	if counts[Tier3] != 2 {
		t.Errorf("T3 = %d, want 2", counts[Tier3])
	}
}

func TestClassifyCanonical(t *testing.T) {
	// Every entry in the dictionary classifies to its own tier + canonical name.
	for name, want := range canonical {
		got := Classify(name)
		if got.Tier != want.tier || got.Name != want.name {
			t.Errorf("Classify(%q) = {Name:%q Tier:%d}, want {Name:%q Tier:%d}",
				name, got.Name, got.Tier, want.name, want.tier)
		}
	}
}

func TestClassifyCaseInsensitive(t *testing.T) {
	// "Payment_Failed", "PAYMENT_FAILED", and "  payment_failed  " all canonicalize.
	for _, in := range []string{"Payment_Failed", "PAYMENT_FAILED", "  payment_failed  "} {
		got := Classify(in)
		if got.Name != "payment_failed" || got.Tier != Tier1 {
			t.Errorf("Classify(%q) = {Name:%q Tier:%d}, want canonical payment_failed/T1", in, got.Name, got.Tier)
		}
	}
}

func TestClassifyT4(t *testing.T) {
	// Unknown names are T4 (ordinary line), never an error.
	for _, in := range []string{"", "  ", "checkout.button.click", "something_happened", "mY_rAnDoM_eVeNt"} {
		got := Classify(in)
		if got.Tier != Tier4 {
			t.Errorf("Classify(%q) = %d, want T4", in, got.Tier)
		}
		if got.Name != "" {
			t.Errorf("Classify(%q) Name = %q, want empty for T4", in, got.Name)
		}
	}
}

func TestClassifyReservedPrefix(t *testing.T) {
	// "uc." is upcontrol's namespace; client-sent events with it are dropped.
	for _, in := range []string{"uc.anything", "uc.internal.metric", "UC.Uppercase"} {
		got := Classify(in)
		if got.Tier != Tier5 {
			t.Errorf("Classify(%q) = %d, want T5 (reserved)", in, got.Tier)
		}
	}
	// Reserved wins even if it would otherwise be canonical-shaped.
	if got := Classify("uc.deploy"); got.Tier != Tier5 {
		t.Errorf("uc.deploy = %d, want T5 (reserved beats canonical)", got.Tier)
	}
}

func TestTierOrder(t *testing.T) {
	// T1 < T2 < T3 < T4 < T5 — the ladder matters for routing (wake vs queue).
	if Tier1 >= Tier2 || Tier2 >= Tier3 || Tier3 >= Tier4 || Tier4 >= Tier5 {
		t.Fatal("tier ordering is not T1<T2<T3<T4<T5")
	}
}
