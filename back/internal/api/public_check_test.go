package api

import (
	"strings"
	"testing"
	"time"

	"go.upcontrol.io/back/internal/probe/discover"
	"go.upcontrol.io/back/internal/probe/executor"
)

func labels(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["label"].(string))
	}
	return out
}

func find(rows []map[string]any, label string) map[string]any {
	for _, r := range rows {
		if r["label"] == label {
			return r
		}
	}
	return nil
}

func TestStagesOmitUnmeasuredPhases(t *testing.T) {
	// An IP literal over plaintext: no name to resolve, no handshake. Sending
	// dns:0 and tls:0 would read as "instant", which is the opposite of true.
	res := executor.Result{ConnectMs: 41, TTFBMs: 182, TotalMs: 278}
	stages := stagesFrom(res)
	got := labels(stages)
	want := []string{"tcp", "wait", "html"}
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stages = %v, want %v", got, want)
		}
	}
	// html is the tail after the first byte, not a measured hook.
	if html := stages[2]["ms"]; html != uint32(96) {
		t.Errorf("html = %v, want 96", html)
	}
}

func TestStagesSumToTotal(t *testing.T) {
	// TTFBMs is measured from the start of the request: a naive pass-through
	// would render this 546 ms request as 958 ms of bars.
	res := executor.Result{DNSMs: 116, ConnectMs: 136, TLSMs: 160, TTFBMs: 546, TotalMs: 610}
	var sum uint32
	for _, s := range stagesFrom(res) {
		sum += s["ms"].(uint32)
	}
	if sum != res.TotalMs {
		t.Errorf("stages sum to %d, want %d", sum, res.TotalMs)
	}
}

func TestStagesGuardAgainstUnderflow(t *testing.T) {
	// A reused connection can leave the phases at or past TTFB. Unguarded, the
	// uint32 subtraction renders a bar about four billion milliseconds wide.
	res := executor.Result{DNSMs: 100, ConnectMs: 100, TLSMs: 100, TTFBMs: 50, TotalMs: 300}
	for _, s := range stagesFrom(res) {
		if s["label"] == "wait" {
			t.Fatalf("wait stage present when the phases already exceed TTFB: %v", s)
		}
		if s["ms"].(uint32) > res.TotalMs {
			t.Fatalf("stage wider than the whole request: %v", s)
		}
	}
}

func TestHtmlReportsAMeasuredZeroRatherThanVanishing(t *testing.T) {
	// total == ttfb for a single-page shell: html is derived from numbers we
	// hold, so a zero here is measured, not absent.
	res := executor.Result{DNSMs: 24, TTFBMs: 168, TotalMs: 168}
	html := find(stagesFrom(res), "html")
	if html == nil {
		t.Fatal("html stage dropped when it measured zero")
	}
	if html["ms"] != uint32(0) {
		t.Errorf("html = %v, want a measured 0", html["ms"])
	}
}

func TestPhasesThatNeverRanStayAbsent(t *testing.T) {
	// The other half of the rule: a zero from the recorder means the hook never
	// fired, and those phases must not render as "0 ms".
	res := executor.Result{ConnectMs: 41, TTFBMs: 60, TotalMs: 100}
	stages := stagesFrom(res)
	if find(stages, "dns") != nil {
		t.Error("dns stage present with no lookup")
	}
	if find(stages, "tls") != nil {
		t.Error("tls stage present with no handshake")
	}
}

func TestNetworkRowsReportOnlyMeasuredFacts(t *testing.T) {
	// No DNS lookup and no TLS: neither row may appear, because the landing
	// renders an absent row as unknown and a present one as fact.
	rows := networkRowsFrom(executor.Result{StatusCode: 200, TotalMs: 478, OK: true}, "ok")
	if find(rows, "dns") != nil {
		t.Error("dns row present without a resolved address")
	}
	if find(rows, "tls") != nil {
		t.Error("tls row present without a handshake")
	}
	// response is always measured once the request completed.
	resp := find(rows, "response")
	if resp == nil {
		t.Fatal("response row missing")
	}
	if resp["value"] != "478 ms" || resp["note"] != "HTTP 200" {
		t.Errorf("response row = %v", resp)
	}
	// Zero hops is a fact, not an absence.
	red := find(rows, "redirects")
	if red == nil || red["value"] != "0 hops" {
		t.Errorf("redirects row = %v, want 0 hops", red)
	}
}

func TestNetworkRowsCarryDNSAndTLSWhenMeasured(t *testing.T) {
	expires := time.Now().Add(32 * 24 * time.Hour)
	res := executor.Result{
		OK: true, StatusCode: 200, TotalMs: 478,
		DNSMs: 24, DNSAddrs: 1, TLSVersion: "TLS 1.3", SSLExpiresAt: expires,
		RedirectCount: 2,
	}
	rows := networkRowsFrom(res, "ok")

	dns := find(rows, "dns")
	if dns == nil || dns["value"] != "24 ms" || dns["note"] != "1 address" {
		t.Errorf("dns row = %v", dns)
	}
	tls := find(rows, "tls")
	if tls == nil || tls["value"] != "TLS 1.3" {
		t.Fatalf("tls row = %v", tls)
	}
	// Inside the 40-day window the row asks to be looked at.
	if tls["status"] != "check" {
		t.Errorf("tls status = %v, want check for an expiry 32 days out", tls["status"])
	}
	if red := find(rows, "redirects"); red == nil || red["value"] != "2 hops" {
		t.Errorf("redirects row = %v, want 2 hops", red)
	}
}

func TestNetworkRowsDropRedirectsForBlockedTarget(t *testing.T) {
	// The SSRF guard refuses before any request is made: "0 hops" would
	// describe a request that never happened.
	rows := networkRowsFrom(executor.Result{ErrorClass: "blocked_target"}, "down")
	if find(rows, "redirects") != nil {
		t.Error("redirects row present for a blocked target")
	}
	if find(rows, "response") == nil {
		t.Error("response row missing: the refusal is itself the outcome")
	}
}

func TestDiscoveredRowsOmitWhatWasNotMeasured(t *testing.T) {
	// Nothing established → no rows at all, so the landing renders its no-data
	// marker rather than a row that looks like a finding.
	if rows := discoveredRows(discover.Facts{}); len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
}

func TestDiscoveredRowsNameTheSPATrap(t *testing.T) {
	rows := discoveredRows(discover.Facts{
		ErrorPage: &discover.ErrorPage{Status: 200, Correct: false},
	})
	row := find(rows, "error page")
	if row == nil {
		t.Fatal("error page row missing")
	}
	if row["value"] != "200" {
		t.Errorf("value = %v, want 200", row["value"])
	}
	// The row has to say why 200 is a problem, not just show the number.
	if row["note"] != "should be an error, so checkers will miss outages" {
		t.Errorf("note = %v", row["note"])
	}
	if row["status"] != "check" {
		t.Errorf("status = %v, want check", row["status"])
	}
}

func TestDiscoveredRowsHealthNoneIsNotTheUnmeasuredMarker(t *testing.T) {
	// A measured "there is none" must still produce a row: the landing shows
	// 000 only where nothing was established at all.
	rows := discoveredRows(discover.Facts{Health: &discover.Health{}})
	row := find(rows, "health url")
	if row == nil {
		t.Fatal("health url row missing for a measured empty result")
	}
	if row["value"] != "none" {
		t.Errorf("value = %v, want none", row["value"])
	}
	if row["status"] != "nodata" {
		t.Errorf("status = %v, want nodata", row["status"])
	}
}

func TestDiscoveredRowsHeaders(t *testing.T) {
	rows := discoveredRows(discover.Facts{
		Headers: &discover.Headers{HSTS: false, Compression: "gzip", CacheControl: false},
	})
	row := find(rows, "headers")
	if row == nil || row["value"] != "no HSTS" {
		t.Fatalf("headers row = %v", row)
	}
	if row["note"] != "compression on, no cache policy" {
		t.Errorf("note = %v", row["note"])
	}
	if row["status"] != "check" {
		t.Errorf("status = %v, want check when HSTS is absent", row["status"])
	}

	secure := discoveredRows(discover.Facts{
		Headers: &discover.Headers{HSTS: true, Compression: "br", CacheControl: true},
	})
	row = find(secure, "headers")
	if row["value"] != "HSTS on" || row["status"] != "ok" {
		t.Errorf("secure headers row = %v", row)
	}
	if row["note"] != "compression on, cache policy set" {
		t.Errorf("note = %v", row["note"])
	}
}

func TestSameHostTargetsRefusesAForeignURL(t *testing.T) {
	// The boundary where a client-supplied URL stops being trusted: without it,
	// a stranger's endpoint joins the probe schedule forever.
	got := sameHostTargets("https://mine.com", []string{
		"https://mine.com/pricing",
		"https://victim.example/heavy",
		"http://169.254.169.254/latest/meta-data/",
		"javascript:alert(1)",
	})
	if len(got) != 1 || got[0] != "https://mine.com/pricing" {
		t.Errorf("targets = %v, want only the same-host page", got)
	}
}

func TestSameHostTargetsDropsTheHostItselfAndDuplicates(t *testing.T) {
	// The host is watched unconditionally, so repeating it here would spend a
	// second of three free checks on the same URL.
	got := sameHostTargets("https://mine.com", []string{
		"https://mine.com",
		"https://mine.com/a?utm=x",
		"https://mine.com/a",
		"https://mine.com/a#top",
	})
	if len(got) != 1 || got[0] != "https://mine.com/a" {
		t.Errorf("targets = %v, want /a once", got)
	}
}

func TestMonitorNameReadsAsAList(t *testing.T) {
	if got := monitorName("mine.com", "https://mine.com"); got != "mine.com" {
		t.Errorf("name = %q, want the bare host", got)
	}
	if got := monitorName("mine.com", "https://mine.com/pricing"); got != "mine.com/pricing" {
		t.Errorf("name = %q, want host + path", got)
	}
}

func TestPageRowsCarryTheirURLAsID(t *testing.T) {
	// The id IS the target, which is what lets the watch request send the ids
	// back with no lookup table in between.
	rows := pageRows([]discover.Page{
		{URL: "https://x.io/pricing", Path: "/pricing", Source: "in sitemap.xml", Status: 200, TotalMs: 120, OK: true},
	})
	if len(rows) != 1 || rows[0]["id"] != "https://x.io/pricing" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["name"] != "/pricing" || rows[0]["recommended"] != true {
		t.Errorf("row = %v", rows[0])
	}
}

func TestPageRowsDistinguishNotProbedFromDown(t *testing.T) {
	rows := pageRows([]discover.Page{
		{URL: "https://x.io/a", Path: "/a", Source: "in sitemap.xml", Status: 0},
		{URL: "https://x.io/b", Path: "/b", Source: "in sitemap.xml", Status: 500, TotalMs: 12},
	})
	// Found but out of budget is unknown, never down — the same rule the strip's
	// no-data marker follows.
	if rows[0]["status"] != "nodata" {
		t.Errorf("unprobed row = %v, want nodata", rows[0])
	}
	if rows[0]["recommended"] != false {
		t.Errorf("unprobed page recommended: %v", rows[0])
	}
	if rows[1]["status"] != "down" {
		t.Errorf("500 row = %v, want down", rows[1])
	}
}

func TestPageRowsNameTheSlowest(t *testing.T) {
	rows := pageRows([]discover.Page{
		{URL: "https://x.io/a", Path: "/a", Source: "in sitemap.xml", Status: 200, TotalMs: 480, OK: true, Slowest: true},
	})
	if !strings.Contains(rows[0]["meta"].(string), "the slowest here") {
		t.Errorf("meta = %v, want the slowest page named", rows[0]["meta"])
	}
	if rows[0]["status"] != "check" {
		t.Errorf("slowest row status = %v, want check", rows[0]["status"])
	}
}

func TestPageRowsSeparateSilenceFromNeverAsking(t *testing.T) {
	// Both leave Status at 0, and conflating them is how a broken host reads as
	// a gap in our coverage rather than a finding about theirs.
	rows := pageRows([]discover.Page{
		{URL: "https://a.x.io", Path: "a.x.io", Source: "linked from the site"},
		{URL: "https://b.x.io", Path: "b.x.io", Source: "found in DNS", Error: "timeout"},
	})
	if rows[0]["status"] != "nodata" || !strings.Contains(rows[0]["meta"].(string), "not probed") {
		t.Errorf("unprobed row = %v, want nodata/not probed", rows[0])
	}
	if rows[1]["status"] != "down" {
		t.Errorf("silent host = %v, want down", rows[1])
	}
	if !strings.Contains(rows[1]["meta"].(string), "timeout") {
		t.Errorf("meta = %v, want the error class named", rows[1]["meta"])
	}
}

func TestAPIRowIsPickableAndCarriesItsURLAsID(t *testing.T) {
	// The API sits with the things that have a checkbox: a path-based API is
	// the same thing on a site that routes instead of subdomaining.
	row := apiRow("https://x.io", &discover.API{
		Path: "/api", Source: "answers directly", Status: 401, Confirmed: true,
	})
	if row == nil || row["id"] != "https://x.io/api" || row["name"] != "/api" {
		t.Fatalf("api row = %v", row)
	}
	if row["recommended"] != true {
		t.Error("a confirmed API is not offered as a default")
	}
	if !strings.Contains(row["meta"].(string), "guarded") {
		t.Errorf("meta = %v, want the guard named", row["meta"])
	}
}

func TestAPIRowDoesNotPreTickAPermanent404(t *testing.T) {
	// The base is real — the app calls through it — but a check on a root that
	// answers 404 would pin a permanent failure. Offer it, say so, leave it off.
	row := apiRow("https://x.io", &discover.API{
		Path: "/api/api", Source: "in the app bundle", Status: 404,
	})
	if row == nil {
		t.Fatal("api row missing: knowing where the API lives is worth offering")
	}
	if row["recommended"] != false || row["status"] != "check" {
		t.Errorf("row = %v, want an unticked check row", row)
	}
	if !strings.Contains(row["meta"].(string), "pick an endpoint under it") {
		t.Errorf("meta = %v", row["meta"])
	}
}

func TestAPIRowAbsentWhenThereIsNothingToWatch(t *testing.T) {
	// A group with no target is not a choice. "None" belongs to the facts strip,
	// not to a list of things you tick.
	if row := apiRow("https://x.io", nil); row != nil {
		t.Errorf("row = %v, want nil when unmeasured", row)
	}
	if row := apiRow("https://x.io", &discover.API{}); row != nil {
		t.Errorf("row = %v, want nil when measured absent", row)
	}
}
