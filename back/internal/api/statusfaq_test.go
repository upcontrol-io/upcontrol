package api

import (
	"strings"
	"testing"
)

func TestBrandOf(t *testing.T) {
	for host, want := range map[string]string{
		"datrade.io":         "Datrade",
		"app.datrade.io":     "Datrade",
		"shop.example.co.uk": "Example",
		"localhost":          "localhost",
		"xn--80ak6aa92e.com": "xn--80ak6aa92e.com",
	} {
		if got := brandOf(host); got != want {
			t.Errorf("brandOf(%q) = %q, want %q", host, got, want)
		}
	}
}

// The FAQ answers from the measured state only: an up host says no, a down
// one says yes and names why, an ongoing incident wins the outage question,
// and only a page nobody owns offers Get alerts.
func TestStatusFAQ(t *testing.T) {
	okState := map[string]any{"kind": "ok", "asOf": "12:04 UTC",
		"sentence": "As of 12:04 UTC, datrade.io answered HTTP 200 in 146 ms from our check."}
	comps := []map[string]any{{"uptime": "99.9%", "bars": []string{"ok", "ok"}, "barSpanSec": 43200}}
	faq := statusFAQ("datrade.io", okState, comps, nil, false)
	text := faqText(faq)
	for _, want := range []string{
		"Is Datrade down right now? No. As of 12:04 UTC, datrade.io answered HTTP 200",
		"Why is datrade.io not working or not loading for me?",
		"Is Datrade down for everyone or just me? Not for everyone",
		"Is there a Datrade outage? No outage has been recorded",
		"What is Datrade's uptime? 99.9% over the last 24 h",
		"How do I get alerted when datrade.io goes down?",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ok FAQ misses %q in:\n%s", want, text)
		}
	}

	downState := map[string]any{"kind": "down", "asOf": "12:04 UTC",
		"sentence": "Our check has had no answer from datrade.io since 11:50 UTC (connection timed out)."}
	incidents := []map[string]any{{"title": "datrade.io is down", "since": "Sep 25, 11:50", "ongoing": true}}
	text = faqText(statusFAQ("datrade.io", downState, nil, incidents, true))
	for _, want := range []string{
		"Is Datrade down right now? Yes. Our check has had no answer",
		"Is Datrade down for everyone or just me? Not just you",
		"Is there a Datrade outage? Yes, one is ongoing: datrade.io is down, since Sep 25, 11:50.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("down FAQ misses %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Get alerts") || strings.Contains(text, "uptime") {
		t.Errorf("a claimed page without components offered alerts or an uptime:\n%s", text)
	}
	if strings.Contains(text, "—") {
		t.Error("the FAQ carries an em-dash")
	}
}

func faqText(faq []faqItem) string {
	var b strings.Builder
	for _, f := range faq {
		b.WriteString(f.Q + " " + f.A + "\n")
	}
	return b.String()
}
