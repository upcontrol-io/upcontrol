package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	apigen "go.upcontrol.io/back/gen/api"
)

// The agent's dashboard topic teaches by example, and an agent copies an example
// verbatim, so an example this server refuses is worse than none: the user's first
// board would arrive as a 400. This reads the topic the installer bundles and sends
// every example through readLayout, the one door a stored layout enters by
// (decodeStrict, so an unknown field is refused, then validateLayout), instead of a
// second reading of the rules that could drift from it.
//
// Nothing is skipped on a guess. Every fenced ```json block, and every `{...}` code
// span in a table row (the ref each card takes), must be a JSON object, and its
// top-level keys say what it is, so a typo in the very key that names it fails
// instead of going unread:
//   - "widgets" is a complete layout, sent as it is;
//   - "kind" is a widget, wrapped in a layout;
//   - "source" is a ref, wrapped in a minimal widget (the card its table row names,
//     else a stat) and a layout.
//
// A block with none of the three fails. JSON that is not one of them, a command's
// answer say, is fenced without the json tag.
//
// Two narrow exceptions. The ref that lists every field a ref may carry has a "..."
// for each value, so only its keys are read: the strict decoder must take them all.
// And the topic holds exactly one sketch, a layout whose widgets are `[ ... ]`; it
// is read with that gap closed to `[]`, so its other keys are still checked. Any
// other block that is not JSON fails, and so does a second sketch.
//
// The topic is read from where the installer bundles it, which is the same relative
// path in this repository and in the public one.
const dashboardTopic = "../../../cli/plugin/references/dashboard.md"

// sketchGap is the topic's own mark for "more goes here".
const sketchGap = "[ ... ]"

// docBlock is one piece of JSON the topic shows: the line it sits on (1-based, what
// an editor jumps to), its text, and the card a table row names it under, if any.
type docBlock struct {
	line int
	text string
	card string
}

// jsonBlocks are the fenced ```json blocks, each at the line its fence opens on.
func jsonBlocks(md string) []docBlock {
	var out []docBlock
	var body []string
	open := 0
	for i, line := range strings.Split(md, "\n") {
		switch fence := strings.TrimSpace(line); {
		case open == 0 && fence == "```json":
			open = i + 1
		case open != 0 && fence == "```":
			out = append(out, docBlock{line: open, text: strings.Join(body, "\n")})
			open, body = 0, nil
		case open != 0:
			body = append(body, line)
		}
	}
	return out
}

// tableRefs are the inline `{...}` code spans of the topic's tables, where each row
// pairs a card (its first code span) with the ref it takes.
func tableRefs(md string) []docBlock {
	var out []docBlock
	for i, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		// Between backticks, the pieces at odd positions are the code spans.
		var spans []string
		for j, piece := range strings.Split(line, "`") {
			if j%2 == 1 {
				spans = append(spans, strings.TrimSpace(piece))
			}
		}
		for _, s := range spans {
			if strings.HasPrefix(s, "{") {
				out = append(out, docBlock{line: i + 1, text: s, card: spans[0]})
			}
		}
	}
	return out
}

func object(text string) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal([]byte(text), &obj)
	return obj, err
}

func TestDocsExamples_DashboardTopicIsStorable(t *testing.T) {
	md, err := os.ReadFile(dashboardTopic)
	if err != nil {
		t.Fatalf("read the topic the installer bundles: %v", err)
	}
	fenced, table := jsonBlocks(string(md)), tableRefs(string(md))
	// A reading of the topic that stops finding its examples must fail here rather
	// than pass by checking nothing.
	if len(fenced) == 0 || len(table) == 0 {
		t.Fatalf("dashboard.md: found %d fenced json blocks and %d table refs, want both", len(fenced), len(table))
	}
	var accepted, sketches int
	for _, b := range slices.Concat(fenced, table) {
		text := b.text
		obj, err := object(text)
		if err != nil && strings.Contains(text, sketchGap) {
			text = strings.Replace(text, sketchGap, "[]", 1)
			if obj, err = object(text); err == nil {
				sketches++
			}
		}
		if err != nil {
			t.Errorf("dashboard.md:%d: not a JSON object (%v); the only thing allowed to be unfinished is one %s", b.line, err, sketchGap)
			continue
		}

		_, isLayout := obj["widgets"]
		_, isWidget := obj["kind"]
		_, isRef := obj["source"]
		var what, layout string
		switch {
		case isLayout:
			what, layout = "layout", text
		case isWidget:
			what, layout = "widget", `{"version":2,"widgets":[`+text+`]}`
		case isRef && obj["source"] == "...":
			var ref apigen.DashboardMetricRef
			dec := json.NewDecoder(strings.NewReader(text))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ref); err != nil {
				t.Errorf("dashboard.md:%d: the field list names a key a ref does not take: %v", b.line, err)
			} else {
				t.Logf("dashboard.md:%d: field list, keys read and values left alone", b.line)
			}
			continue
		case isRef:
			what = "ref"
			layout = fmt.Sprintf(`{"version":2,"widgets":[{"id":"w_doc","kind":%q,"title":"doc","metrics":[%s],"x":0,"y":0,"w":3,"h":6}]}`,
				cmp.Or(b.card, "stat"), text)
		default:
			t.Errorf(`dashboard.md:%d: a json block with none of "widgets" (a layout), "kind" (a widget) or "source" (a ref): a key is misspelled, or this is not an example`, b.line)
			continue
		}

		w := httptest.NewRecorder()
		doc, ok := readLayout(w, httptest.NewRequest(http.MethodPut, "/v1/dashboard", strings.NewReader(layout)))
		if !ok {
			t.Errorf("dashboard.md:%d: the server refuses this %s: %d %s", b.line, what, w.Code, strings.TrimSpace(w.Body.String()))
			continue
		}
		accepted++
		t.Logf("dashboard.md:%d: %s accepted, %d widgets", b.line, what, len(doc.Widgets))
	}
	if sketches != 1 {
		t.Errorf("dashboard.md holds %d sketches (a %s gap), want exactly one", sketches, sketchGap)
	}
	t.Logf("%d examples accepted, %d sketch read with its gap closed", accepted, sketches)
}
