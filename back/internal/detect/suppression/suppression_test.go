package suppression

import (
	"testing"
	"time"
)

var baseTime = time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC)

func TestPostDeploySuppresses(t *testing.T) {
	d := Evaluate(Input{
		LastDeployAt: baseTime,
		Now:          baseTime.Add(60 * time.Second),
	})
	if !d.Suppress || d.Reason != "post_deploy_blip" {
		t.Errorf("post-deploy should suppress, got %+v", d)
	}
}

func TestPostDeployExpires(t *testing.T) {
	d := Evaluate(Input{
		LastDeployAt: baseTime,
		Now:          baseTime.Add(120 * time.Second),
	})
	if d.Suppress {
		t.Error("post-deploy should expire after 90s")
	}
}

func TestMaintenanceSuppresses(t *testing.T) {
	d := Evaluate(Input{
		InMaintenance: true,
		Now:           baseTime,
	})
	if !d.Suppress || d.Reason != "maintenance_window" {
		t.Errorf("maintenance should suppress, got %+v", d)
	}
}

func TestDedupSuppresses(t *testing.T) {
	d := Evaluate(Input{
		HasOpenIncident: true,
		Now:             baseTime,
	})
	if !d.Suppress || d.Reason != "duplicate_open_incident" {
		t.Errorf("dedup should suppress, got %+v", d)
	}
}

func TestCooldownSuppresses(t *testing.T) {
	d := Evaluate(Input{
		LastFireAt: baseTime,
		Now:        baseTime.Add(15 * time.Minute),
	})
	if !d.Suppress || d.Reason != "cooldown" {
		t.Errorf("cooldown should suppress within 30min, got %+v", d)
	}
}

func TestCooldownExpires(t *testing.T) {
	d := Evaluate(Input{
		LastFireAt: baseTime,
		Now:        baseTime.Add(35 * time.Minute),
	})
	if d.Suppress {
		t.Error("cooldown should expire after 30min")
	}
}

func TestFreshFireAllowed(t *testing.T) {
	d := Evaluate(Input{
		Now: baseTime,
	})
	if d.Suppress {
		t.Errorf("fresh fire should be allowed, got %+v", d)
	}
}

func TestPriorityPostDeployOverMaintenance(t *testing.T) {
	// Post-deploy takes priority over maintenance.
	d := Evaluate(Input{
		LastDeployAt:  baseTime,
		InMaintenance: true,
		Now:           baseTime.Add(30 * time.Second),
	})
	if d.Reason != "post_deploy_blip" {
		t.Errorf("post-deploy should win over maintenance, got %q", d.Reason)
	}
}
