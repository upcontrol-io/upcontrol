// The board's web source: what the site tag measured beyond page views.
// engaged and reach draw a line out of web_engage; engaged ranks pages too,
// and rage, dead and clicks rank pages out of web_heat.

package api

import (
	"context"
	"time"

	"go.upcontrol.io/back/internal/storage/pgstore"
)

// webHeatKinds maps the web source's per-page names to their web_heat kind.
var webHeatKinds = map[string]string{"rage": "rage", "dead": "dead", "clicks": "click"}

// webReachAfter maps where.after to its index in EngageBucket.R.
var webReachAfter = map[string]int{"10": 0, "30": 1, "120": 2, "300": 3}

// validateWeb is validateSeries for the web source; "" when answerable.
func validateWeb(q seriesQuery) string {
	_, heat := webHeatKinds[q.Name]
	if !heat && q.Name != "engaged" && q.Name != "reach" {
		return "unknown web name " + q.Name + " in " + q.ID
	}
	if len(q.Labels) > 0 || q.Count != "" {
		return "labels and count are not for the web source in " + q.ID
	}
	byPath := len(q.Group) == 1 && q.Group[0] == "path"
	if len(q.Group) > 0 && !byPath {
		return "the web source groups by path alone in " + q.ID
	}
	switch {
	case heat && !byPath:
		return q.Name + " needs group [path] in " + q.ID
	case q.Name == "reach" && byPath:
		return "reach takes no group in " + q.ID
	case q.Name == "reach":
		if _, ok := webReachAfter[q.Where["after"]]; !ok {
			return "reach needs where.after of 10, 30, 120 or 300 in " + q.ID
		}
	}
	return ""
}

// webRange is r for the web source: web_engage is hourly, so a web read never
// steps below an hour, over the same span.
func webRange(r seriesRange) seriesRange {
	if r.step >= 3600 {
		return r
	}
	return seriesRange{step: 3600, buckets: r.step * r.buckets / 3600}
}

// webValue is one bucket's engaged seconds per view, or the percent of views
// that reached `after`; nil with no view, because a number nobody measured is
// never printed.
//
// ponytail: a heat beacon lands in its own hour, which may be the hour after
// its view, so one bucket can carry ms or reach of a view another holds; the
// clamp keeps that edge from printing past 100 %.
func webValue(q seriesQuery, b pgstore.EngageBucket) any {
	if b.Views == 0 {
		return nil
	}
	if q.Name == "engaged" {
		return float64(b.MS) / float64(b.Views) / 1000
	}
	return min(100*float64(b.R[webReachAfter[q.Where["after"]]])/float64(b.Views), 100)
}

// webSeries answers engaged or reach per bucket. A bucket with no row has no
// views and stays null, which also covers every hour before the oldest row
// kept. The sums add up across buckets, so the total is the buckets' sum read
// as one; the previous span is one bucket that wide.
func (h *writeAPI) webSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) ([]any, any, any, error) {
	if h.pgs == nil {
		return nil, nil, nil, nil
	}
	path, device := q.Where["path"], q.Where["device"]
	rows, err := h.pgs.EngageBuckets(ctx, tenantID, projectID, path, device, from, to, r.step)
	if err != nil {
		return nil, nil, nil, err
	}
	points := make([]any, r.buckets)
	var whole pgstore.EngageBucket
	for _, b := range rows {
		if b.Index < 0 || b.Index >= int64(r.buckets) {
			continue
		}
		points[b.Index] = webValue(q, b)
		whole.Views += b.Views
		whole.MS += b.MS
		for i := range whole.R {
			whole.R[i] += b.R[i]
		}
	}
	span := to.Sub(from)
	prev, err := h.pgs.EngageBuckets(ctx, tenantID, projectID, path, device, from.Add(-span), from, int(span/time.Second))
	if err != nil {
		return nil, nil, nil, err
	}
	var previous any
	if len(prev) > 0 {
		previous = webValue(q, prev[0])
	}
	return points, webValue(q, whole), previous, nil
}

// webGroupedSeries ranks pages: engaged by engaged seconds per view, rage,
// dead and clicks by their web_heat sum. Past a week the heat is in weekly
// rows on their Monday, so the read starts at its first day's Monday, as the
// heatmap read does.
func (h *writeAPI) webGroupedSeries(ctx context.Context, tenantID, projectID int64, q seriesQuery, from, to time.Time, r seriesRange) (map[string]any, error) {
	rows := []any{}
	if h.pgs != nil {
		device := q.Where["device"]
		var sums []pgstore.LabelSum
		var err error
		if q.Name == "engaged" {
			sums, err = h.pgs.EngagePaths(ctx, tenantID, projectID, device, from, to)
		} else {
			fromDay := from.Truncate(24 * time.Hour)
			if to.Sub(from) > 7*24*time.Hour {
				fromDay = fromDay.AddDate(0, 0, -(int(fromDay.Weekday())+6)%7)
			}
			sums, err = h.pgs.HeatPaths(ctx, tenantID, projectID, webHeatKinds[q.Name], device, fromDay)
		}
		if err != nil {
			return nil, err
		}
		rows = labelRows(sums)
	}
	return groupedAnswer(q, from, to, r, rows), nil
}
