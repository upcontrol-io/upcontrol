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
