package query

import (
	"testing"
	"time"
)

func TestStream_AlwaysScopedToCutoff(t *testing.T) {
	q := New(7, 11, 4242) // tenant 7, project 11, cutoff 4242
	got := q.Stream(50, nil, nil, "", Range{})
	if !contains(got.SQL, "tenant_id = $1") || !contains(got.SQL, "project_id = $2") || !contains(got.SQL, "seq >= $3") {
		t.Fatalf("Stream must always scope to tenant+project+cutoff; got:\n%s", got.SQL)
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
	if !contains(got.SQL, "LIMIT 50") {
		t.Fatalf("limit must appear in SQL; got:\n%s", got.SQL)
	}
}

func TestStream_FiltersAppendConditions(t *testing.T) {
	q := New(1, 2, 3)
	got := q.Stream(10, []string{"error"}, []string{"api"}, "oom", Range{})
	for _, want := range []string{"level = 'error'", "service IN ($4)", "message ILIKE $5"} {
		if !contains(got.SQL, want) {
			t.Fatalf("filter %q missing from SQL:\n%s", want, got.SQL)
		}
	}
	// Args order: tenant, project, cutoff, service, search (levels are inlined
	// from the validated enum, never bound).
	if len(got.Args) != 5 {
		t.Fatalf("expected 5 args, got %d (%v)", len(got.Args), got.Args)
	}
	if got.Args[3] != "api" {
		t.Fatalf("service must be bound as an arg; got %v", got.Args)
	}
	if got.Args[4] != "%oom%" {
		t.Fatalf("search must be wrapped as ILIKE %%…%%; got %v", got.Args[4])
	}
}

func TestStream_MultiServiceBindsEveryName(t *testing.T) {
	// Two picked services are one IN over both, and the unlabelled service
	// rides as the empty string, a value like any other (not a dropped filter).
	got := New(1, 2, 3).Stream(10, nil, []string{"api", ""}, "", Range{})
	if !contains(got.SQL, "service IN ($4, $5)") {
		t.Fatalf("two services must bind two placeholders; got:\n%s", got.SQL)
	}
	if got.Args[3] != "api" || got.Args[4] != "" {
		t.Fatalf("both names (including the empty one) must be bound; got %v", got.Args)
	}
}

func TestStream_InfoBucketIsEverythingButErrorAndWarn(t *testing.T) {
	// The panel's third bucket has to partition the stream with the other two,
	// or debug lines vanish from every combination that includes "info".
	got := New(1, 2, 3).Stream(10, []string{"warn", "info"}, nil, "", Range{})
	if !contains(got.SQL, "(level = 'warn' OR level NOT IN ('error', 'warn'))") {
		t.Fatalf("info must mean NOT error/warn, OR-joined with the picked levels; got:\n%s", got.SQL)
	}
}

func TestStream_UnknownLevelIsDroppedNotBound(t *testing.T) {
	// Levels are inlined into SQL, so anything outside the enum must be dropped
	// here — matched literally it would be an injection seam.
	got := New(1, 2, 3).Stream(10, []string{"fatal'; DROP TABLE logs;--"}, nil, "", Range{})
	if contains(got.SQL, "DROP TABLE") || contains(got.SQL, "level = ") {
		t.Fatalf("unknown level must leave no trace in the predicate; got:\n%s", got.SQL)
	}
}

func TestServices_ListsTheWindowsServicesWithCounts(t *testing.T) {
	q := New(7, 11, 4242)
	got := q.Services(0)
	for _, want := range []string{"GROUP BY service", "count(*)", "tenant_id = $1", "project_id = $2", "seq >= $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Services must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
}

func TestServices_IsNeverFilteredByService(t *testing.T) {
	// Filtering the list by the service already picked would leave the picker
	// holding one option, with no way back to the rest of the window.
	got := New(1, 2, 3).Services(0)
	if contains(got.SQL, "service = $") {
		t.Fatalf("Services must not filter by service; got:\n%s", got.SQL)
	}
}

func TestVolume_GroupsByMinute(t *testing.T) {
	q := New(1, 2, 3)
	got := q.Volume(nil, nil)
	if !contains(got.SQL, "date_trunc('minute', ts)") || !contains(got.SQL, "seq >= $3") {
		t.Fatalf("volume must group by minute within the cutoff; got:\n%s", got.SQL)
	}
}

func TestVolume_FollowsTheFilters(t *testing.T) {
	// The strip sits directly above the lines it describes; left unfiltered it
	// drew the whole window's mass over a narrowed stream.
	got := New(1, 2, 3).Volume([]string{"error"}, []string{"api"})
	if !contains(got.SQL, "service IN ($4)") || !contains(got.SQL, "level = 'error'") {
		t.Fatalf("volume must honour the stream's filters; got:\n%s", got.SQL)
	}
	if len(got.Args) != 4 || got.Args[3] != "api" {
		t.Fatalf("service must be bound as an arg; got %v", got.Args)
	}
}

func TestDetailBucketSeconds_SnapsAndRefuses(t *testing.T) {
	minute := Range{From: time.Unix(0, 0).UTC(), To: time.Unix(60, 0).UTC()}
	cases := []struct {
		name      string
		requested int
		within    Range
		want      int
	}{
		{"a request the ladder has is kept", 5, minute, 5},
		{"a request between rungs snaps up, never down", 3, minute, 5},
		{"unasked means no detail", 0, minute, 0},
		{"a range with no bounds has no minute to detail", 5, Range{}, 0},
		{
			// Every width this could answer at would be coarser than the minute
			// `volume` already draws, so there is nothing to add.
			name: "a range too wide for any offered width is refused",
			// 600 buckets is the cap, so 60s buckets cover 10 hours; 11 is past it.
			requested: 1,
			within:    Range{From: time.Unix(0, 0).UTC(), To: time.Unix(11*3600, 0).UTC()},
			want:      0,
		},
		{
			// One second across an hour is 3600 buckets, past the cap — it
			// coarsens rather than refusing, because a width that fits exists.
			name:      "too many buckets at the asked width coarsens to one that fits",
			requested: 1,
			within:    Range{From: time.Unix(0, 0).UTC(), To: time.Unix(3600, 0).UTC()},
			want:      10,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetailBucketSeconds(c.requested, c.within); got != c.want {
				t.Errorf("DetailBucketSeconds(%d) = %d, want %d", c.requested, got, c.want)
			}
		})
	}
}

func TestVolumeDetail_BoundedAndFiltered(t *testing.T) {
	within := Range{From: time.Unix(0, 0).UTC(), To: time.Unix(60, 0).UTC()}
	got := New(1, 2, 3).VolumeDetail(5, within, []string{"error"}, []string{"api"})
	if !contains(got.SQL, "to_timestamp(floor(extract(epoch from ts) / 5) * 5)") {
		t.Fatalf("detail must bucket at the snapped width; got:\n%s", got.SQL)
	}
	// Unlike Volume, this one IS bounded: it describes a range the reader picked,
	// and unbounded it would group the whole ring one bucket at a time.
	if !contains(got.SQL, "ts >= $5") || !contains(got.SQL, "ts < $6") {
		t.Fatalf("detail must be bounded by the range; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "service IN ($4)") || !contains(got.SQL, "level = 'error'") {
		t.Fatalf("detail must honour the stream's filters; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "seq >= $3") {
		t.Fatalf("detail must stay inside the cutoff; got:\n%s", got.SQL)
	}
}

func TestVolumeDetail_RefusedRequestBuildsNoQuery(t *testing.T) {
	// A caller that asks for detail on an unbounded range gets no SQL at all,
	// rather than a query that quietly scans the ring.
	got := New(1, 2, 3).VolumeDetail(5, Range{}, nil, nil)
	if got.SQL != "" {
		t.Fatalf("an unanswerable detail request must build nothing; got:\n%s", got.SQL)
	}
}

func TestSlice_RangeInclusiveOfCutoff(t *testing.T) {
	q := New(1, 2, 3000)
	got := q.Slice(1000, 2000, 500)
	// Slice uses fromSeq/toSeq directly but still carries the cutoff so a query
	// can't reach below the visible window even with a crafted range.
	if !contains(got.SQL, "seq >= $3") {
		t.Fatalf("slice must still enforce the cutoff; got:\n%s", got.SQL)
	}
	assertArgsHave(t, got.Args, int64(1), int64(2), int64(1000), int64(2000))
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return len(needle) == 0
}

func assertArgsHave(t *testing.T, args []any, wants ...int64) {
	t.Helper()
	for _, w := range wants {
		found := false
		for _, a := range args {
			if v, ok := a.(int64); ok && v == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("args %v missing expected int64 %d", args, w)
		}
	}
}

func TestSummary_ScopedToCutoff(t *testing.T) {
	// "Does this project send anything" must be answered about the VISIBLE
	// window, or a fully displaced project would still read as connected.
	q := New(7, 11, 4242)
	got := q.Summary()
	for _, want := range []string{"count(*)", "max(ts)", "tenant_id = $1", "project_id = $2", "seq >= $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Summary must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
}

func TestBeyondErrors_CountsOnlyTheDisplacedTail(t *testing.T) {
	// "Beyond the window" means between retain_seq and cutoff_seq: still stored,
	// no longer shown. Dropping either bound would count the visible window too.
	q := New(3, 4, 900) // cutoff 900
	got := q.BeyondErrors(100)
	for _, want := range []string{"seq >= $3", "seq < $4", "level = 'error'"} {
		if !contains(got.SQL, want) {
			t.Fatalf("BeyondErrors must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(3), int64(4), int64(100), int64(900))
}

func TestCutoffSeq_IsReportedAsBuilt(t *testing.T) {
	// /v1/plan reports this number to the account; it must be the same boundary
	// the queries use, not a recomputed one.
	if got := New(1, 2, 4242).CutoffSeq(); got != 4242 {
		t.Fatalf("CutoffSeq() = %d, want 4242", got)
	}
}

func TestEventSeen_ScopedAndNamed(t *testing.T) {
	q := New(7, 11, 4242)
	got := q.EventSeen("install_verified")
	for _, want := range []string{"tenant_id = $1", "project_id = $2", "seq >= $3", "message = $4"} {
		if !contains(got.SQL, want) {
			t.Fatalf("EventSeen must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
	if got.Args[3] != "install_verified" {
		t.Fatalf("event name must be a bound arg; got %v", got.Args)
	}
}

func TestRecentEvents_ScopedGroupedLimited(t *testing.T) {
	q := New(7, 11, 4242)
	got := q.RecentEvents(15*60*1e9, 12) // 15 minutes in time.Duration units
	for _, want := range []string{"GROUP BY message", "LIMIT 12", "tenant_id = $1", "project_id = $2", "seq >= $3", "ts >= $4"} {
		if !contains(got.SQL, want) {
			t.Fatalf("RecentEvents must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
}

// The failing lines must outrank the tail, but the filter must never be
// exclusive, or an incident with no error-level logs freezes an empty slice.
func TestEvidence_RanksErrorsAndWarningsAboveTheTail(t *testing.T) {
	q := New(7, 11, 4242)
	got := q.Evidence(12)

	if !contains(got.SQL, "ORDER BY level IN ('error', 'warn') DESC") {
		t.Fatalf("errors and warnings must sort first; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "seq DESC") {
		t.Fatalf("within a rank the newest line still wins; got:\n%s", got.SQL)
	}
	// The tie-break IS the top-up: without a level predicate in WHERE, a window
	// holding no errors still returns its newest ordinary lines.
	if contains(got.SQL, "WHERE") && contains(got.SQL, "level =") {
		t.Fatalf("the level must rank, never filter; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "LIMIT 12") {
		t.Fatalf("limit must appear in SQL; got:\n%s", got.SQL)
	}
}

func TestEvidence_AlwaysScopedToCutoff(t *testing.T) {
	q := New(7, 11, 4242)
	got := q.Evidence(12)
	for _, want := range []string{"tenant_id = $1", "project_id = $2", "seq >= $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Evidence must scope like Stream (%s); got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11), int64(4242))
}
