package availability

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)

func TestNormalOperation(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusNoData}
	for i := 0; i < 10; i++ {
		out := d.Process(s, true, t0.Add(time.Duration(i)*time.Minute))
		if out.Open || out.Close {
			t.Errorf("check %d: unexpected outcome %+v", i, out)
		}
		if s.Status != StatusOK {
			t.Errorf("check %d: status = %q, want ok", i, s.Status)
		}
	}
}

func TestSingleBlipDoesNotFire(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusOK}
	out := d.Process(s, false, t0)
	if out.Open {
		t.Error("single failure should not open an incident")
	}
	if s.Status != StatusCheck {
		t.Errorf("status = %q, want check", s.Status)
	}
	if s.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", s.ConsecutiveFailures)
	}
	// Recovery from the blip.
	out = d.Process(s, true, t0.Add(time.Minute))
	if out.Close {
		t.Error("blip recovery should not close (no incident was open)")
	}
	if s.Status != StatusOK {
		t.Errorf("status = %q, want ok after recovery", s.Status)
	}
}

func TestThresholdOpensIncident(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusOK}
	// Two failures → check, no incident.
	for i := 0; i < 2; i++ {
		out := d.Process(s, false, t0.Add(time.Duration(i)*time.Minute))
		if out.Open {
			t.Errorf("failure %d should not open yet", i+1)
		}
	}
	if s.Status != StatusCheck {
		t.Errorf("after 2 failures: status = %q, want check", s.Status)
	}
	// Third failure → threshold reached → down + open.
	out := d.Process(s, false, t0.Add(2*time.Minute))
	if !out.Open {
		t.Error("third failure should open an incident")
	}
	if s.Status != StatusDown {
		t.Errorf("after threshold: status = %q, want down", s.Status)
	}
	if s.ConsecutiveFailures != 3 {
		t.Errorf("failures = %d, want 3", s.ConsecutiveFailures)
	}
}

func TestRecoveryClosesIncident(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusDown, ConsecutiveFailures: 5}
	out := d.Process(s, true, t0)
	if !out.Close {
		t.Error("recovery should close the incident")
	}
	if out.CloseReason != "recovered" {
		t.Errorf("close reason = %q, want recovered", out.CloseReason)
	}
	if s.Status != StatusOK {
		t.Errorf("status = %q, want ok after recovery", s.Status)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0 after recovery", s.ConsecutiveFailures)
	}
}

func TestDownMoreFailuresDoNotReopen(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusDown, ConsecutiveFailures: 3}
	for i := 0; i < 5; i++ {
		out := d.Process(s, false, t0.Add(time.Duration(i)*time.Minute))
		if out.Open {
			t.Errorf("failure %d while down should not re-open", i)
		}
	}
	if s.Status != StatusDown {
		t.Errorf("status should stay down")
	}
}

func TestIntermittentNeverFires(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusOK}
	// fail, ok, fail, ok, fail, ok — never reaches 3 consecutive.
	pattern := []bool{false, true, false, true, false, true, false, true}
	for i, ok := range pattern {
		out := d.Process(s, ok, t0.Add(time.Duration(i)*time.Minute))
		if out.Open {
			t.Errorf("step %d: should not open (intermittent)", i)
		}
	}
	if s.Status != StatusOK {
		t.Errorf("status = %q, want ok after intermittent", s.Status)
	}
}

func TestCustomThreshold1(t *testing.T) {
	d := New(1) // any single failure opens
	s := &State{Status: StatusOK}
	out := d.Process(s, false, t0)
	if !out.Open {
		t.Error("threshold=1 should open on first failure")
	}
	if s.Status != StatusDown {
		t.Errorf("status = %q, want down", s.Status)
	}
}

func TestFromNoData(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusNoData}
	// First OK from nodata → ok.
	d.Process(s, true, t0)
	if s.Status != StatusOK {
		t.Errorf("first OK from nodata: status = %q, want ok", s.Status)
	}
}

func TestFromNoDataFirstFailure(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusNoData}
	out := d.Process(s, false, t0)
	if out.Open {
		t.Error("first failure from nodata should not open")
	}
	if s.Status != StatusCheck {
		t.Errorf("first failure from nodata: status = %q, want check", s.Status)
	}
}

func TestCheckRecoversToOK(t *testing.T) {
	d := New(3)
	s := &State{Status: StatusOK}
	d.Process(s, false, t0)                          // → check
	d.Process(s, false, t0.Add(time.Minute))         // → check (2 failures)
	out := d.Process(s, true, t0.Add(2*time.Minute)) // → ok (recovery before threshold)
	if out.Open || out.Close {
		t.Error("recovery from check should not open or close")
	}
	if s.Status != StatusOK {
		t.Errorf("status = %q, want ok after recovery from check", s.Status)
	}
	if s.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0", s.ConsecutiveFailures)
	}
}
