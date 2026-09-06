package query

import (
	"testing"
	"time"
)

func TestStream_AlwaysScopedToTenantAndProject(t *testing.T) {
	q := New(7, 11) // tenant 7, project 11
	got := q.Stream(50, nil, nil, "", Range{})
	if !contains(got.SQL, "tenant_id = $1") || !contains(got.SQL, "project_id = $2") {
		t.Fatalf("Stream must always scope to tenant+project; got:\n%s", got.SQL)
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
	if !contains(got.SQL, "LIMIT 50") {
		t.Fatalf("limit must appear in SQL; got:\n%s", got.SQL)
	}
}

func TestStream_FiltersAppendConditions(t *testing.T) {
	q := New(1, 2)
	got := q.Stream(10, []string{"error"}, []string{"api"}, "oom", Range{})
	for _, want := range []string{"level = 'error'", "service IN ($3)", "message ILIKE $4"} {
		if !contains(got.SQL, want) {
			t.Fatalf("filter %q missing from SQL:\n%s", want, got.SQL)
		}
	}
	// Args order: tenant, project, service, search (levels are inlined from the
	// validated enum, never bound).
	if len(got.Args) != 4 {
		t.Fatalf("expected 4 args, got %d (%v)", len(got.Args), got.Args)
	}
	if got.Args[2] != "api" {
		t.Fatalf("service must be bound as an arg; got %v", got.Args)
	}
	if got.Args[3] != "%oom%" {
		t.Fatalf("search must be wrapped as ILIKE %%…%%; got %v", got.Args[3])
	}
}

func TestStream_MultiServiceBindsEveryName(t *testing.T) {
	// Two picked services are one IN over both, and the unlabelled service
	// rides as the empty string, a value like any other (not a dropped filter).
	got := New(1, 2).Stream(10, nil, []string{"api", ""}, "", Range{})
	if !contains(got.SQL, "service IN ($3, $4)") {
		t.Fatalf("two services must bind two placeholders; got:\n%s", got.SQL)
	}
	if got.Args[2] != "api" || got.Args[3] != "" {
		t.Fatalf("both names (including the empty one) must be bound; got %v", got.Args)
	}
}

func TestStream_InfoBucketIsEverythingButErrorAndWarn(t *testing.T) {
	// The panel's third bucket has to partition the stream with the other two,
	// or debug lines vanish from every combination that includes "info".
	got := New(1, 2).Stream(10, []string{"warn", "info"}, nil, "", Range{})
	if !contains(got.SQL, "(level = 'warn' OR level NOT IN ('error', 'warn'))") {
		t.Fatalf("info must mean NOT error/warn, OR-joined with the picked levels; got:\n%s", got.SQL)
	}
}

func TestStream_UnknownLevelIsDroppedNotBound(t *testing.T) {
	// Levels are inlined into SQL, so anything outside the enum must be dropped
	// here — matched literally it would be an injection seam.
	got := New(1, 2).Stream(10, []string{"fatal'; DROP TABLE logs;--"}, nil, "", Range{})
	if contains(got.SQL, "DROP TABLE") || contains(got.SQL, "level = ") {
		t.Fatalf("unknown level must leave no trace in the predicate; got:\n%s", got.SQL)
	}
}

func TestServices_ListsTheProjectsServicesWithCounts(t *testing.T) {
	q := New(7, 11)
	got := q.Services(0)
	for _, want := range []string{"GROUP BY service", "count(*)", "tenant_id = $1", "project_id = $2"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Services must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

func TestServices_IsNeverFilteredByService(t *testing.T) {
	// Filtering the list by the service already picked would leave the picker
	// holding one option, with no way back to the rest of the lines.
	got := New(1, 2).Services(0)
	if contains(got.SQL, "service = $") {
		t.Fatalf("Services must not filter by service; got:\n%s", got.SQL)
	}
}

func TestVolume_GroupsByMinuteWithinScope(t *testing.T) {
	q := New(1, 2)
	got := q.Volume(nil, nil)
	if !contains(got.SQL, "date_trunc('minute', ts)") {
		t.Fatalf("volume must group by minute; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "tenant_id = $1") || !contains(got.SQL, "project_id = $2") {
		t.Fatalf("volume must stay scoped to tenant+project; got:\n%s", got.SQL)
	}
}

func TestVolume_FollowsTheFilters(t *testing.T) {
	// The strip sits directly above the lines it describes; left unfiltered it
	// drew the whole project's mass over a narrowed stream.
	got := New(1, 2).Volume([]string{"error"}, []string{"api"})
	if !contains(got.SQL, "service IN ($3)") || !contains(got.SQL, "level = 'error'") {
		t.Fatalf("volume must honour the stream's filters; got:\n%s", got.SQL)
	}
	if len(got.Args) != 3 || got.Args[2] != "api" {
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
	got := New(1, 2).VolumeDetail(5, within, []string{"error"}, []string{"api"})
	if !contains(got.SQL, "to_timestamp(floor(extract(epoch from ts) / 5) * 5)") {
		t.Fatalf("detail must bucket at the snapped width; got:\n%s", got.SQL)
	}
	// Unlike Volume, this one IS bounded: it describes a range the reader picked,
	// and unbounded it would group every line one bucket at a time.
	if !contains(got.SQL, "ts >= $4") || !contains(got.SQL, "ts < $5") {
		t.Fatalf("detail must be bounded by the range; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "service IN ($3)") || !contains(got.SQL, "level = 'error'") {
		t.Fatalf("detail must honour the stream's filters; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "tenant_id = $1") || !contains(got.SQL, "project_id = $2") {
		t.Fatalf("detail must stay scoped to tenant+project; got:\n%s", got.SQL)
	}
}

func TestVolumeDetail_RefusedRequestBuildsNoQuery(t *testing.T) {
	// A caller that asks for detail on an unbounded range gets no SQL at all,
	// rather than a query that quietly scans the whole table.
	got := New(1, 2).VolumeDetail(5, Range{}, nil, nil)
	if got.SQL != "" {
		t.Fatalf("an unanswerable detail request must build nothing; got:\n%s", got.SQL)
	}
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

func TestSummary_ScopedToTenantAndProject(t *testing.T) {
	// "Does this project send anything" must be answered about THIS project, or
	// a neighbour's traffic reads as this one being connected.
	q := New(7, 11)
	got := q.Summary()
	for _, want := range []string{"count(*)", "max(ts)", "tenant_id = $1", "project_id = $2"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Summary must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

func TestEventSeen_ScopedAndNamed(t *testing.T) {
	q := New(7, 11)
	got := q.EventSeen("install_verified")
	for _, want := range []string{"tenant_id = $1", "project_id = $2", "message = $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("EventSeen must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
	if got.Args[2] != "install_verified" {
		t.Fatalf("event name must be a bound arg; got %v", got.Args)
	}
}

func TestRecentEvents_ScopedGroupedLimited(t *testing.T) {
	q := New(7, 11)
	got := q.RecentEvents(15*60*1e9, 12) // 15 minutes in time.Duration units
	for _, want := range []string{"GROUP BY message", "LIMIT 12", "tenant_id = $1", "project_id = $2", "ts >= $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("RecentEvents must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

// The failing lines must outrank the tail, but the filter must never be
// exclusive, or an incident with no error-level logs freezes an empty slice.
func TestEvidence_RanksErrorsAndWarningsAboveTheTail(t *testing.T) {
	q := New(7, 11)
	got := q.Evidence(12)

	if !contains(got.SQL, "ORDER BY level IN ('error', 'warn') DESC") {
		t.Fatalf("errors and warnings must sort first; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "seq DESC") {
		t.Fatalf("within a rank the newest line still wins; got:\n%s", got.SQL)
	}
	// The tie-break IS the top-up: without a level predicate in WHERE, a project
	// holding no errors still returns its newest ordinary lines.
	if contains(got.SQL, "WHERE") && contains(got.SQL, "level =") {
		t.Fatalf("the level must rank, never filter; got:\n%s", got.SQL)
	}
	if !contains(got.SQL, "LIMIT 12") {
		t.Fatalf("limit must appear in SQL; got:\n%s", got.SQL)
	}
}

func TestEvidence_AlwaysScopedToTenantAndProject(t *testing.T) {
	q := New(7, 11)
	got := q.Evidence(12)
	for _, want := range []string{"tenant_id = $1", "project_id = $2"} {
		if !contains(got.SQL, want) {
			t.Fatalf("Evidence must scope like Stream (%s); got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

func TestGroups_ScopedAndCappedPerServiceAndLevel(t *testing.T) {
	got := New(7, 11).Groups(7*24*time.Hour, 8)
	for _, want := range []string{
		"tenant_id = $1", "project_id = $2", "ts >= $3",
		"GROUP BY service, 2, fingerprint",
		"row_number() OVER (PARTITION BY service, CASE WHEN level IN ('error', 'warn') THEN level ELSE 'info' END ORDER BY count(*) DESC)",
		"WHERE rn <= 8",
		"left((array_agg(message ORDER BY ts DESC))[1], 200)",
	} {
		if !contains(got.SQL, want) {
			t.Fatalf("Groups must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

// The picker's levels have to partition the ring the same way the filter does,
// or a group listed under "info" is unreachable from the level it was listed at.
func TestGroups_FoldsEveryOtherLevelIntoInfo(t *testing.T) {
	got := New(1, 2).Groups(time.Hour, 8)
	if !contains(got.SQL, "CASE WHEN level IN ('error', 'warn') THEN level ELSE 'info' END AS level") {
		t.Fatalf("Groups must emit the same three levels the API knows; got:\n%s", got.SQL)
	}
}

func TestAttrPairs_BoundedScanAndValueCap(t *testing.T) {
	got := New(7, 11).AttrPairs(7*24*time.Hour, 20000, 10)
	for _, want := range []string{
		"tenant_id = $1", "project_id = $2", "attrs IS NOT NULL",
		"ORDER BY ts DESC LIMIT 20000",
		"CROSS JOIN LATERAL jsonb_each_text(attrs)",
		"length(value) <= 80",
		"WHERE rn <= 10",
	} {
		if !contains(got.SQL, want) {
			t.Fatalf("AttrPairs must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

func TestSeriesBuckets_ScopedAndIndexedFromTheRangeStart(t *testing.T) {
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	got := New(7, 11).SeriesBuckets(Range{From: from, To: from.Add(time.Hour)}, 60, SeriesFilter{})
	// The base rides in the SELECT list, so it must be $1 or the args slice
	// has drifted from the placeholders.
	if !contains(got.SQL, "floor((extract(epoch from ts)::float8 - $1) / 60)::bigint AS bucket") {
		t.Fatalf("the bucket must be indexed from the bound base; got:\n%s", got.SQL)
	}
	if got.Args[0] != float64(from.Unix()) {
		t.Fatalf("the base must be the range start; got %v", got.Args[0])
	}
	for _, want := range []string{"tenant_id = $2", "project_id = $3"} {
		if !contains(got.SQL, want) {
			t.Fatalf("SeriesBuckets must contain %q; got:\n%s", want, got.SQL)
		}
	}
	assertArgsHave(t, got.Args, int64(7), int64(11))
}

func TestSeriesBuckets_BindsTheAttributeKeyAsWellAsItsValue(t *testing.T) {
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	got := New(1, 2).SeriesBuckets(Range{From: from, To: from.Add(time.Hour)}, 60,
		SeriesFilter{Attrs: map[string]string{"route": "/checkout", "env": "prod"}})
	if !contains(got.SQL, "attrs ->> $4::text = $5") || !contains(got.SQL, "attrs ->> $6::text = $7") {
		t.Fatalf("both halves of an attribute pair must be parameters; got:\n%s", got.SQL)
	}
	// Sorted, so the same filter always builds the same statement text.
	if got.Args[3] != "env" || got.Args[4] != "prod" || got.Args[5] != "route" || got.Args[6] != "/checkout" {
		t.Fatalf("attribute pairs must bind in key order; got %v", got.Args)
	}
}

func TestSeriesBuckets_DropsAnUnknownLevelRatherThanBindingIt(t *testing.T) {
	// A crafted level must not reach the engine at all: appendLevelFilter
	// inlines only the three it recognises, so anything else is no predicate.
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	got := New(1, 2).SeriesBuckets(Range{From: from, To: from.Add(time.Hour)}, 60,
		SeriesFilter{Level: "'; DROP TABLE logs; --"})
	if contains(got.SQL, "DROP TABLE") {
		t.Fatalf("an unknown level must be dropped, not spliced; got:\n%s", got.SQL)
	}
	for _, a := range got.Args {
		if s, ok := a.(string); ok && s != "" {
			t.Fatalf("an unknown level must not be bound either; got %v", got.Args)
		}
	}
}

func TestSeriesBuckets_FingerprintAndSearchAreParameters(t *testing.T) {
	from := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	fp := int64(-1)
	got := New(1, 2).SeriesBuckets(Range{From: from, To: from.Add(time.Hour)}, 60,
		SeriesFilter{Fingerprint: &fp, Search: "oom"})
	if !contains(got.SQL, "fingerprint = $4") || !contains(got.SQL, "message ILIKE $5") {
		t.Fatalf("fingerprint and search must bind; got:\n%s", got.SQL)
	}
	if got.Args[3] != int64(-1) || got.Args[4] != "%oom%" {
		t.Fatalf("args must carry the wrapped fingerprint and the wrapped search; got %v", got.Args)
	}
}

func TestSeriesBuckets_NeedsABoundedRange(t *testing.T) {
	// An unbounded series has no bucket zero, so there is nothing to answer.
	if got := New(1, 2).SeriesBuckets(Range{}, 60, SeriesFilter{}); got.SQL != "" {
		t.Fatalf("an unbounded range must build nothing; got:\n%s", got.SQL)
	}
}
