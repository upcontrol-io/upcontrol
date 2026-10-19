package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The rule validator, run on the merged rule: what a create sends on its own,
// and what a PATCH leaves once laid over the stored rule.
func TestValidateProductAlert(t *testing.T) {
	base := `"event":"signup","kind":"change","direction":"down","percent":50,"windowHours":6`
	cases := []struct {
		name string
		body string
		want string // "" accepts; else a fragment of the message
	}{
		{"a change rule", `{` + base + `}`, ""},
		{"a silence rule needs no direction", `{"event":"signup","kind":"silence","windowHours":24}`, ""},
		{"uc.pageview is the one SDK event allowed", `{"event":"uc.pageview","kind":"silence","windowHours":1}`, ""},
		{"money in a currency", `{` + base + `,"measure":"amount","currency":"USD"}`, ""},
		{"labels under the series rules", `{` + base + `,"labels":{"plan":"pro"}}`, ""},
		{"another uc. event", `{"event":"uc.click","kind":"silence","windowHours":1}`, "uc."},
		{"no event", `{"kind":"silence","windowHours":1}`, "event"},
		{"a 41-byte event", `{"event":"` + strings.Repeat("e", 41) + `","kind":"silence","windowHours":1}`, "event"},
		{"a control character in the event", `{"event":"sign\tup","kind":"silence","windowHours":1}`, "event"},
		{"amount without a currency", `{` + base + `,"measure":"amount"}`, "currency"},
		{"a lowercase currency", `{` + base + `,"measure":"amount","currency":"usd"}`, "currency"},
		{"a currency without measure", `{` + base + `,"currency":"USD"}`, "measure"},
		{"an unknown measure", `{` + base + `,"measure":"count"}`, "measure"},
		{"change without a direction", `{"event":"signup","kind":"change","percent":50,"windowHours":6}`, "direction"},
		{"change without a percent", `{"event":"signup","kind":"change","direction":"up","windowHours":6}`, "percent"},
		{"a percent below 10", `{"event":"signup","kind":"change","direction":"up","percent":9,"windowHours":6}`, "percent"},
		{"a percent above 1000", `{"event":"signup","kind":"change","direction":"up","percent":1001,"windowHours":6}`, "percent"},
		{"an unknown kind", `{"event":"signup","kind":"spike","windowHours":6}`, "kind"},
		{"a 2-hour window", `{"event":"signup","kind":"silence","windowHours":2}`, "windowHours"},
		{"an 81-byte name", `{` + base + `,"name":"` + strings.Repeat("n", 81) + `"}`, "name"},
		{"a 65-byte widget id", `{` + base + `,"widgetId":"` + strings.Repeat("w", 65) + `"}`, "widgetId"},
		{"four labels", `{` + base + `,"labels":{"a":"1","b":"2","c":"3","d":"4"}}`, "labels"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var req productAlertWrite
			if err := json.Unmarshal([]byte(c.body), &req); err != nil {
				t.Fatalf("decode %s: %v", c.body, err)
			}
			got := validateProductAlert(req.mergeInto(productAlertRule{}))
			if c.want == "" && got != "" {
				t.Fatalf("want accepted, got %q", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want a message naming %q, got %q", c.want, got)
			}
		})
	}
}

// A PATCH is validated as the rule it leaves behind: a currency alone is fine
// over a stored amount rule, and dropping the measure without the currency is
// a half-cleared pair.
func TestPatchValidatesTheMergedRule(t *testing.T) {
	stored := productAlertRule{
		Event: "purchase", Measure: "amount", Currency: "USD", Kind: "change",
		Direction: "down", Percent: 50, WindowHours: 6, Labels: map[string]string{},
	}
	for body, want := range map[string]string{
		`{"currency":"EUR"}`:           "",
		`{"name":"Revenue"}`:           "",
		`{"kind":"silence"}`:           "",
		`{"measure":""}`:               "measure",
		`{"measure":"","currency":""}`: "",
		`{"windowHours":5}`:            "windowHours",
		`{"event":"uc.identify"}`:      "uc.",
		`{"percent":5}`:                "percent",
		`{"labels":{"":"empty key"}}`:  "label",
		`{"labels":{}}`:                "",
	} {
		var req productAlertWrite
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		got := validateProductAlert(req.mergeInto(stored))
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("PATCH %s over the stored rule = %q, want a message naming %q", body, got, want)
		}
	}
}

// Only what the rule measures resets the episode; renaming or pausing it does not.
func TestEpisodeChanged(t *testing.T) {
	stored := productAlertRule{Event: "signup", Kind: "change", Direction: "down", Percent: 50,
		WindowHours: 6, Labels: map[string]string{"plan": "pro"}}
	for body, want := range map[string]bool{
		`{"name":"Signups"}`:         false,
		`{"paused":true}`:            false,
		`{"labels":{"plan":"pro"}}`:  false,
		`{"percent":60}`:             true,
		`{"labels":{"plan":"free"}}`: true,
		`{"windowHours":12}`:         true,
	} {
		var req productAlertWrite
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if got := episodeChanged(stored, req.mergeInto(stored)); got != want {
			t.Errorf("episodeChanged after %s = %v, want %v", body, got, want)
		}
	}
}

func TestProductAlertsReason(t *testing.T) {
	if got := productAlertsReason(2, "Indie", nil); got != "Your plan carries 2 product alerts. Indie carries them without a limit." {
		t.Fatalf("the Free wall = %q", got)
	}
	if got := productAlertsReason(2, "", nil); got != "Your plan carries 2 product alerts." {
		t.Fatalf("with nothing to name = %q", got)
	}
}
