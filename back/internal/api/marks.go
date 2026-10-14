// GET /v1/dashboard/marks: the deploys, releases and incidents of the current
// project inside a range, for the board's charts to mark. A line that moved is
// half an answer; the change drawn beside it is the other half.

package api

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	apigen "go.upcontrol.io/back/gen/api"
	"go.upcontrol.io/back/internal/storage/pgstore"
)

// deployBurst is how close a deploy event has to follow the one before it to
// be the same deploy: a pipeline reports one as several events.
const deployBurst = 10 * time.Minute

// markLabelMax is the longest label a deploy or a release carries: it names a
// change on a chart, it does not describe it. An incident's label is its title,
// and a title is kept whole.
const markLabelMax = 40

// getDashboardMarks answers every kind of mark or none of them. An empty list
// is a real answer; a list missing one kind because its read failed is not,
// since a chart would draw it as a window in which that kind never happened.
func (h *writeAPI) getDashboardMarks(w http.ResponseWriter, r *http.Request, tenantID int64) {
	start := time.Now()
	rng := r.URL.Query().Get("range")
	if _, ok := seriesRanges[rng]; !ok {
		writeAPIErrMsg(w, http.StatusBadRequest, "bad_query", "unknown range "+strconv.Quote(rng))
		return
	}
	ctx := r.Context()
	// The depth gate /v1/series applies, for its reason: a range the plan does
	// not carry is refused whole, never answered clipped.
	msg, plan, err := h.historyRefusal(ctx, tenantID, []seriesQuery{{ID: "marks", Source: "event", Range: rng}})
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	if msg != "" {
		writeUpgradeRequired(w, msg, plan)
		return
	}
	projectID := h.currentProject(ctx, r, tenantID)
	if projectID == 0 || h.pgs == nil {
		writeAPIJSON(w, http.StatusOK, apigen.DashboardMarks{Marks: []apigen.DashboardMark{}})
		return
	}
	// Marks are board reads: the queue, the guard and the slow-read log of
	// /v1/series, whose log is what ReleaseMarks' scan waits on.
	queued := time.Now()
	freeSlot, err := takeReadSlot(ctx)
	if err != nil {
		return // the caller left while it waited
	}
	defer freeSlot()
	waited := time.Since(queued)
	defer func() {
		if took := time.Since(start); took > slowSeriesRead {
			slog.Warn("marks: slow read", "tenant_id", tenantID, "range", rng,
				"ms", took.Milliseconds(), "wait_ms", waited.Milliseconds())
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, seriesReadGuard)
	defer cancel()
	from, to := marksWindow(rng, time.Now().UTC())
	var deploys, releases, incidents []pgstore.MarkRow
	deploys, err = h.pgs.DeployMarks(ctx, tenantID, projectID, from, to)
	if err == nil {
		releases, err = h.pgs.ReleaseMarks(ctx, tenantID, projectID, from, to)
	}
	if err == nil {
		incidents, err = h.pgs.IncidentMarks(ctx, tenantID, projectID, from, to)
	}
	freeSlot()
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "read_failed")
		return
	}
	writeAPIJSON(w, http.StatusOK, apigen.DashboardMarks{Marks: buildMarks(deploys, releases, incidents)})
}

// marksWindow is the span one marks read covers. A range whose own step is an
// hour or more can be drawn by the hour or by the day (the board's bucket), and
// the window aligned on the hour starts no later than any of them, so the marks
// of a chart's oldest hours are in the answer whichever bucket draws it. Each
// chart keeps only what falls inside its own window.
func marksWindow(rng string, now time.Time) (from, to time.Time) {
	r := seriesRanges[rng]
	if r.step >= seriesBucketSteps["hour"] {
		r = rebucket(r, "hour")
	}
	return r.window(now)
}

// buildMarks words the three reads as one list, oldest first. A deploy closer
// than deployBurst to the mark before it folds into that mark, and the latest
// event of the burst gives it its time and its label: a pipeline reports one
// deploy as started, built and live, and the code changed at the last of them.
// A latest event that carries no sha keeps the sha the burst already had: a sha
// is what a rollback command takes, and the event's name is not.
func buildMarks(deploys, releases, incidents []pgstore.MarkRow) []apigen.DashboardMark {
	out := []apigen.DashboardMark{}
	var sha string // the latest sha of the burst the last mark stands for
	for _, d := range deploys {
		m := apigen.DashboardMark{Kind: apigen.DashboardMarkKindDeploy, At: d.At.UTC()}
		n := len(out)
		inBurst := n > 0 && m.At.Sub(out[n-1].At) < deployBurst
		if !inBurst || d.SHA != "" {
			sha = d.SHA
		}
		m.Label = deployLabel(d.Name, sha)
		if inBurst {
			out[n-1] = m
		} else {
			out = append(out, m)
		}
	}
	for _, rel := range releases {
		out = append(out, apigen.DashboardMark{Kind: apigen.DashboardMarkKindRelease, At: rel.At.UTC(), Label: releaseLabel(rel.Name)})
	}
	for _, inc := range incidents {
		m := apigen.DashboardMark{Kind: apigen.DashboardMarkKindIncident, At: inc.At.UTC(), Label: inc.Name}
		if inc.Until != nil {
			until := inc.Until.UTC()
			m.Until = &until
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b apigen.DashboardMark) int { return a.At.Compare(b.At) })
	return out
}

// deployLabel names a deploy by its commit's short sha, the form a rollback
// command takes, else by its event name.
func deployLabel(name, sha string) string {
	if sha != "" {
		return cutRunes(sha, 7)
	}
	return cutRunes(name, markLabelMax)
}

// releaseLabel names a release by its value, shortened the way a commit is
// when the value is one: forty hex characters are a full sha.
func releaseLabel(rel string) string {
	if len(rel) == 40 {
		if _, err := hex.DecodeString(rel); err == nil {
			return rel[:7]
		}
	}
	return cutRunes(rel, markLabelMax)
}

// cutRunes keeps a label's first n characters, never half of one.
func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
