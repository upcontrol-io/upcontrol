package detector

import "testing"

func TestErrorRateFires(t *testing.T) {
	d := ErrorRate(50, 100, 0.02, 0.005, 1.4826)
	if !d.Fire {
		t.Error("50% error rate vs 2% baseline should fire")
	}
	if d.Reason == "" {
		t.Error("fire should have a reason")
	}
}

func TestErrorRateNoFireLowTotal(t *testing.T) {
	d := ErrorRate(5, 5, 0.02, 0.005, 1.4826)
	if d.Fire {
		t.Error("total < 10 should not fire")
	}
}

func TestErrorRateNoFireNormal(t *testing.T) {
	d := ErrorRate(2, 100, 0.02, 0.005, 1.4826)
	if d.Fire {
		t.Error("2% error rate matching baseline should not fire")
	}
}

func TestAbsenceFires(t *testing.T) {
	d := Absence(3.0, 0, 4)
	if !d.Fire {
		t.Error("3/hr expected, 0 in 4h should fire")
	}
}

func TestAbsenceNoFireObservedNonZero(t *testing.T) {
	d := Absence(3.0, 1, 4)
	if d.Fire {
		t.Error("observed=1 should not fire")
	}
}

func TestAbsenceNoFireLowCadence(t *testing.T) {
	d := Absence(0.5, 0, 4)
	if d.Fire {
		t.Error("0.5/hr cadence too low for absence detection")
	}
}

func TestLatencyFires(t *testing.T) {
	d := Latency(5000, 800, 50, 1.4826)
	if !d.Fire {
		t.Error("5s p95 vs 800ms baseline should fire")
	}
}

func TestLatencyNoFireNormal(t *testing.T) {
	d := Latency(850, 800, 50, 1.4826)
	if d.Fire {
		t.Error("850ms vs 800ms baseline should not fire")
	}
}

func TestDivergenceFires(t *testing.T) {
	d := Divergence(10, 10, 0, 10)
	if !d.Fire {
		t.Error("100% vs 0% success should fire divergence")
	}
}

func TestDivergenceNoFireAgreement(t *testing.T) {
	d := Divergence(9, 10, 9, 10)
	if d.Fire {
		t.Error("90% vs 90% should not fire")
	}
}

func TestNewFingerprintFires(t *testing.T) {
	d := NewFingerprint(false, 20)
	if !d.Fire {
		t.Error("unseen fingerprint at count=20 should fire")
	}
}

func TestNewFingerprintNoFireSeen(t *testing.T) {
	d := NewFingerprint(true, 100)
	if d.Fire {
		t.Error("previously-seen fingerprint should not fire")
	}
}

func TestNewFingerprintNoFireLowCount(t *testing.T) {
	d := NewFingerprint(false, 3)
	if d.Fire {
		t.Error("count < 5 should not fire")
	}
}

func TestBurnRateDecisionPages(t *testing.T) {
	d, page := BurnRateDecision(15, 16, 14.4, 14.4)
	if !d.Fire {
		t.Error("burn rate above threshold should fire")
	}
	if !page {
		t.Error("14.4x threshold should page")
	}
}

func TestBurnRateDecisionNoPage(t *testing.T) {
	d, page := BurnRateDecision(7, 7, 6, 6)
	if !d.Fire {
		t.Error("6x threshold should fire")
	}
	if page {
		t.Error("6x threshold should NOT page")
	}
}

func TestBurnRateDecisionNoFire(t *testing.T) {
	d, _ := BurnRateDecision(0.5, 0.5, 14.4, 14.4)
	if d.Fire {
		t.Error("0.5x burn should not fire at 14.4x threshold")
	}
}
