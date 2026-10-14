package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apigen "go.upcontrol.io/back/gen/api"
	"go.upcontrol.io/back/internal/account/session"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// The session switch reaches the door. routes_test reads main.go as text and
// passes while the switch still falls through to its 404; a range the board
// does not know is answered before any read, so this needs no database.
func TestTheMarksRouteReachesItsHandler(t *testing.T) {
	h := &writeAPI{sess: session.New(nil, 0, nil).WithFixedIdentity(1, 1)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard/marks?range=2h", nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_query") {
		t.Fatalf("got %d %s, want the door's own 400", w.Code, w.Body.String())
	}
}

// marksText prints marks one per line, so a failure reads as the chart would.
func marksText(ms []apigen.DashboardMark) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m.Kind) + " " + m.At.Format("15:04") + " " + m.Label
		if m.Until != nil {
			out[i] += " until " + m.Until.Format("15:04")
		}
	}
	return out
}

// A pipeline reports one deploy as several events, and a burst folds into its
// latest, which gives the mark its time and its label. "Closer than" is
// strict: an event exactly ten minutes after the last is a deploy of its own.
func TestDeploysWithinTenMinutesFoldIntoTheLatest(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	got := marksText(buildMarks([]pgstore.MarkRow{
		{At: t0, Name: "deploy_started"},
		{At: t0.Add(4 * time.Minute), Name: "deploy"},
		// Nine minutes after the first, five after the one before: the chain
		// holds, because each event is measured against the mark it would join.
		{At: t0.Add(9 * time.Minute), Name: "deployment_succeeded", SHA: "a1b2c3d4e5f6"},
		{At: t0.Add(30 * time.Minute), Name: "deploy"},
		{At: t0.Add(40 * time.Minute), Name: "deploy", SHA: "ffffffffff"},
	}, nil, nil))
	want := []string{"deploy 12:09 a1b2c3d", "deploy 12:30 deploy", "deploy 12:40 fffffff"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A burst keeps its sha, the label a rollback command takes: a latest event
// that carries none moves the time and leaves the label alone, in either order
// of the two, and a sha never outlives its burst.
func TestABurstKeepsItsSHA(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }
	for _, c := range []struct {
		name string
		rows []pgstore.MarkRow
		want []string
	}{
		{"the sha first, events with none after it", []pgstore.MarkRow{
			{At: at(0), Name: "deploy_started", SHA: "a1b2c3d4e5f6"},
			{At: at(4), Name: "deploy"},
			{At: at(8), Name: "deployment_succeeded"},
		}, []string{"deploy 12:08 a1b2c3d"}},
		{"the sha last", []pgstore.MarkRow{
			{At: at(0), Name: "deploy_started"},
			{At: at(4), Name: "deployment_succeeded", SHA: "a1b2c3d4e5f6"},
		}, []string{"deploy 12:04 a1b2c3d"}},
		{"two shas: the later one names the burst", []pgstore.MarkRow{
			{At: at(0), Name: "deploy_started", SHA: "a1b2c3d4e5f6"},
			{At: at(4), Name: "deploy", SHA: "ffffffffff"},
			{At: at(8), Name: "deployment_succeeded"},
		}, []string{"deploy 12:08 fffffff"}},
		{"no sha anywhere: the latest event names it", []pgstore.MarkRow{
			{At: at(0), Name: "deploy_started"},
			{At: at(4), Name: "deployment_succeeded"},
		}, []string{"deploy 12:04 deployment_succeeded"}},
		{"a sha is not lent to the next burst", []pgstore.MarkRow{
			{At: at(0), Name: "deploy", SHA: "a1b2c3d4e5f6"},
			{At: at(30), Name: "deploy"},
		}, []string{"deploy 12:00 a1b2c3d", "deploy 12:30 deploy"}},
	} {
		if got := marksText(buildMarks(c.rows, nil, nil)); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMarkLabels(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	wide := strings.Repeat("é", 50) // two bytes a rune: a byte cut would split one
	for _, c := range []struct{ name, got, want string }{
		{"a deploy names its short sha", deployLabel("deploy", sha), "0123456"},
		{"a deploy with no sha names its event", deployLabel("deployment_succeeded", ""), "deployment_succeeded"},
		{"a long name is cut to 40 whole runes", deployLabel(wide, ""), strings.Repeat("é", 40)},
		{"a release that is a full sha reads short", releaseLabel(sha), "0123456"},
		{"in either case", releaseLabel(strings.ToUpper(sha)), "0123456"},
		{"a version reads whole", releaseLabel("1.4.2"), "1.4.2"},
		{"forty characters that are not hex are a name", releaseLabel(strings.Repeat("v", 40)), strings.Repeat("v", 40)},
		{"a long release is cut to 40", releaseLabel(strings.Repeat("r", 41)), strings.Repeat("r", 40)},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// The three kinds come back as one list, oldest first; an open incident has
// no until, and nothing at all is an empty list rather than null.
func TestMarksAreOneListOldestFirst(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	healed := t0.Add(3 * time.Hour)
	got := marksText(buildMarks(
		[]pgstore.MarkRow{{At: t0.Add(2 * time.Hour), Name: "deploy"}},
		[]pgstore.MarkRow{{At: t0.Add(time.Hour), Name: "1.4.2"}},
		[]pgstore.MarkRow{{At: t0, Until: &healed, Name: "Checkout is down"}, {At: t0.Add(4 * time.Hour), Name: "API errors"}},
	))
	want := []string{
		"incident 12:00 Checkout is down until 15:00",
		"release 13:00 1.4.2",
		"deploy 14:00 deploy",
		"incident 16:00 API errors",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if none := buildMarks(nil, nil, nil); none == nil || len(none) != 0 {
		t.Fatalf("no marks is an empty list; got %#v", none)
	}
}

// A marks read starts no later than any chart of its range, whichever bucket
// draws it, and reaches now: a deploy in a chart's oldest hour is in the answer.
func TestMarksWindowCoversEveryBucketOfItsRange(t *testing.T) {
	now := time.Date(2026, 10, 14, 5, 30, 0, 0, time.UTC)
	for _, rng := range []string{"1h", "4h", "12h", "24h", "7d", "31d"} {
		from, to := marksWindow(rng, now)
		if to.Before(now) {
			t.Fatalf("%s: the read ends at %v, before now", rng, to)
		}
		for _, bucket := range []string{"", "hour", "day"} {
			chart := rebucket(seriesRanges[rng], bucket)
			if bucket != "" && (chart.buckets < 2 || chart.buckets > maxSeriesBuckets) {
				continue // the server refuses this bucket on this range
			}
			if start, _ := chart.window(now); start.Before(from) {
				t.Fatalf("%s by %q starts at %v, before the read's %v", rng, bucket, start, from)
			}
		}
	}
}
