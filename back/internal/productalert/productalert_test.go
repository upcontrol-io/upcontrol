package productalert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/storage/pgstore"
)

func change(direction string, percent int) Rule {
	return Rule{Event: "signup", Kind: "change", Direction: direction, Percent: percent, WindowHours: 6}
}

func TestCondition(t *testing.T) {
	silence := Rule{Event: "checkout_completed", Kind: "silence", WindowHours: 3}
	cases := []struct {
		name string
		r    Rule
		rd   Reading
		want string
	}{
		{"down holds", change("down", 50), Reading{Cur: 42, Base: 100, BaseEvents: 100}, StateFiring},
		{"down at the edge", change("down", 50), Reading{Cur: 50, Base: 100, BaseEvents: 100}, StateFiring},
		{"down short of it", change("down", 50), Reading{Cur: 51, Base: 100, BaseEvents: 100}, StateOK},
		{"down ignores a rise", change("down", 50), Reading{Cur: 300, Base: 100, BaseEvents: 100}, StateOK},
		{"up holds", change("up", 100), Reading{Cur: 200, Base: 100, BaseEvents: 100}, StateFiring},
		{"up ignores a drop", change("up", 50), Reading{Cur: 10, Base: 100, BaseEvents: 100}, StateOK},
		{"any on a drop", change("any", 50), Reading{Cur: 40, Base: 100, BaseEvents: 100}, StateFiring},
		{"any on a rise", change("any", 50), Reading{Cur: 160, Base: 100, BaseEvents: 100}, StateFiring},
		{"any inside", change("any", 50), Reading{Cur: 120, Base: 100, BaseEvents: 100}, StateOK},
		{"under 20 base events", change("down", 50), Reading{Cur: 0, Base: 19, BaseEvents: 19}, StateLearning},
		{"money guard counts events", Rule{Kind: "change", Measure: "amount", Direction: "down", Percent: 50, WindowHours: 6},
			Reading{Cur: 0, Base: 500000, BaseEvents: 5}, StateLearning},
		{"zero base value", Rule{Kind: "change", Measure: "amount", Direction: "up", Percent: 50, WindowHours: 6},
			Reading{Cur: 100, Base: 0, BaseEvents: 40}, StateLearning},
		{"silent with history", silence, Reading{CurEvents: 0, History: 56}, StateFiring},
		{"silent without history", silence, Reading{CurEvents: 0, History: 55}, StateLearning},
		{"not silent", silence, Reading{CurEvents: 1, History: 0}, StateOK},
	}
	for _, c := range cases {
		if got := Condition(c.r, c.rd); got != c.want {
			t.Errorf("%s: Condition = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestStep(t *testing.T) {
	cases := []struct{ state, cond, next, send string }{
		{StateLearning, StateFiring, StateFiring, SendFired},
		{StateOK, StateFiring, StateFiring, SendFired},
		{StateFiring, StateFiring, StateFiring, ""}, // no repeat while firing
		{StateFiring, StateOK, StateOK, SendRecovered},
		{StateFiring, StateLearning, StateLearning, SendRecovered},
		{StateOK, StateOK, StateOK, ""},                   // recovered once, then quiet
		{StateLearning, StateLearning, StateLearning, ""}, // a learning rule sends nothing
		{StateOK, StateLearning, StateLearning, ""},
	}
	for _, c := range cases {
		next, send := Step(c.state, c.cond)
		if next != c.next || send != c.send {
			t.Errorf("Step(%s, %s) = (%s, %q), want (%s, %q)", c.state, c.cond, next, send, c.next, c.send)
		}
	}
}

func TestTitles(t *testing.T) {
	money := Rule{Event: "payment", Kind: "change", Measure: "amount", Currency: "usd", Direction: "down", Percent: 50, WindowHours: 6}
	named := change("up", 100)
	named.Name = "Signups"
	cases := []struct {
		r    Rule
		rd   Reading
		send string
		want string
	}{
		{change("down", 50), Reading{Cur: 42, Base: 100}, SendFired, "signup dropped 58% vs the same 6h last week"},
		{named, Reading{Cur: 230, Base: 100}, SendFired, "Signups rose 130% vs the same 6h last week"},
		{money, Reading{Cur: 1000, Base: 4000}, SendFired, "payment revenue dropped 75% vs the same 6h last week"},
		{Rule{Event: "checkout_completed", Kind: "silence", WindowHours: 3}, Reading{}, SendFired, "checkout_completed has been silent for 3h"},
		{change("down", 50), Reading{Cur: 100, Base: 100}, SendRecovered, "signup is back to normal"},
	}
	for _, c := range cases {
		if got := Title(c.r, c.rd, c.send); got != c.want {
			t.Errorf("Title = %q, want %q", got, c.want)
		}
	}
}

func TestPayload(t *testing.T) {
	r := change("down", 50)
	r.Labels = map[string]string{"plan": "pro", "country": "de"}
	var body struct {
		Title, Status, Summary, Detector string
		LinesLabel                       string `json:"lines_label"`
		Fields                           []struct{ Label, Value string }
		Lines                            []string
	}
	decode := func(b []byte) {
		t.Helper()
		body.Lines, body.Fields, body.Summary, body.LinesLabel = nil, nil, "", ""
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
	}
	decode(Payload(r, Reading{Cur: 1200, Base: 2900}, SendFired, nil))
	got := []string{}
	for _, f := range body.Fields {
		got = append(got, f.Label+": "+f.Value)
	}
	want := "Now: 1,200|Week ago: 2,900|Rule: drops 50% in 6h|Filter: country=de, plan=pro"
	if strings.Join(got, "|") != want || body.Status != "check" || body.Detector != "product_alert" {
		t.Errorf("fired payload = %v status %s detector %s", got, body.Status, body.Detector)
	}
	if body.Summary != "No deploys or releases in this window." || body.Lines != nil {
		t.Errorf("no marks: summary %q lines %v", body.Summary, body.Lines)
	}

	decode(Payload(Rule{Event: "x", Kind: "silence", WindowHours: 3}, Reading{History: 300}, SendFired, []string{"deploy abc1234 at 14:02 UTC"}))
	if len(body.Fields) != 1 || body.Fields[0].Value != "about 5 in 3h" || body.LinesLabel != "Deploys in this window" || body.Summary != "" {
		t.Errorf("silence payload = %+v", body)
	}

	decode(Payload(r, Reading{Cur: 2800, Base: 2900}, SendRecovered, nil))
	if body.Status != "ok" || len(body.Fields) != 1 || body.Fields[0].Label != "Now" || body.Fields[0].Value != "2,800" {
		t.Errorf("recovered payload = %+v", body)
	}
}

func TestMarkLines(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, time.UTC) }
	deploys := []pgstore.MarkRow{
		{At: at(9, 0), Name: "deploy", SHA: "0123456789abcdef"},
		{At: at(10, 0), Name: "deploy_api"},
		{At: at(11, 0), Name: "deploy"},
		{At: at(12, 0), Name: "deploy"},
		{At: at(13, 0), Name: "deploy"},
	}
	releases := []pgstore.MarkRow{{At: at(14, 2), Name: "v1.2.0"}}
	got := markLines(deploys, releases)
	if len(got) != 5 || got[0] != "release v1.2.0 at 14:02 UTC" || got[4] != "deploy deploy_api at 10:00 UTC" {
		t.Errorf("markLines = %v", got)
	}
	if got := markLines(deploys[:1], nil); got[0] != "deploy 0123456 at 09:00 UTC" {
		t.Errorf("sha line = %v", got)
	}
}

func TestFormatMoney(t *testing.T) {
	cases := [][2]string{
		{formatMoney(123456, "usd"), "1,234.56 USD"},
		{formatMoney(5, "EUR"), "0.05 EUR"},
		{formatMoney(-150, "GBP"), "-1.50 GBP"},
		{formatMoney(1234567, "JPY"), "1,234,567 JPY"},
		{formatMoney(100000000, "USD"), "1,000,000.00 USD"},
		{group(999), "999"},
		{group(1000), "1,000"},
	}
	for _, c := range cases {
		if got, want := c[0], c[1]; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
