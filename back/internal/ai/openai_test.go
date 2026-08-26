package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// testScenario is a local value everywhere: ExplainLogs is mutable shared
// state and must not leak between tests.
func testScenario(maxOutputBytes int) Scenario {
	return Scenario{
		Key:             "openai_test",
		Version:         1,
		SystemPrompt:    "You are the test system prompt.",
		MaxOutputTokens: 42,
		MaxOutputBytes:  maxOutputBytes,
		Temperature:     0.5,
	}
}

// sseHandler serves one scripted stream and, when capture is non-nil, records
// the decoded request body the client sent.
func sseHandler(t *testing.T, chunks []string, capture *map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			} else {
				*capture = decoded
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprint(w, c)
		}
	}
}

// newTestClient is the client every stream test drives; tests that vary a
// field build their own.
func newTestClient(srv *httptest.Server) *OpenAIClient {
	return testOpenAIAt(srv.URL)
}

// testOpenAIAt builds a client pointed at the given base, for tests that
// vary the base URL spelling.
func testOpenAIAt(base string) *OpenAIClient {
	return &OpenAIClient{Settings: func(context.Context) OpenAISettings {
		return OpenAISettings{BaseURL: base, Model: "gpt-test", Key: "sk-test"}
	}}
}

// adaptServer 400s any request carrying the named parameter and streams a
// good answer otherwise; its own URL, so the quirk memo never crosses tests.
func adaptServer(t *testing.T, rejectParam, message string, calls *int, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		*bodies = append(*bodies, body)
		if _, has := body[rejectParam]; has {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":%q}}`, message)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":"+jsonMust(t, streamAnswer)+"},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n")
		fmt.Fprint(w, done)
	}))
}

// A 400 naming a parameter gets exactly one retry without it, and the cap
// rides whichever spelling survives — dropping one must not drop the rest.
func TestOpenAIClient_AdaptsToRefusedParameter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reject  string
		message string
		effort  string // set only where the case needs a reasoning scenario
		carrier string // the parameter that must still carry the 42
	}{
		{
			name:    "gateway rejects max_completion_tokens",
			reject:  "max_completion_tokens",
			message: "Unrecognized request argument supplied: max_completion_tokens",
			carrier: "max_tokens",
		},
		// OpenAI's own refusal names both spellings: the retry must drop
		// max_tokens, not the replacement it points at.
		{
			name:    "openai rejects max_tokens",
			reject:  "max_tokens",
			message: "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.",
			carrier: "max_completion_tokens",
		},
		{
			name:    "non-reasoning model rejects reasoning_effort",
			reject:  "reasoning_effort",
			message: "Unrecognized request argument supplied: reasoning_effort",
			effort:  "medium",
			carrier: "max_completion_tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var bodies []map[string]any
			srv := adaptServer(t, tc.reject, tc.message, &calls, &bodies)
			defer srv.Close()

			sc := testScenario(16384)
			sc.ReasoningEffort = tc.effort
			if _, err := newTestClient(srv).Complete(context.Background(), sc, Input{Lines: []string{"boom"}}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if calls != 2 {
				t.Fatalf("calls = %d, want 2 — one refusal, one adapted retry", calls)
			}
			if _, has := bodies[1][tc.reject]; has {
				t.Fatalf("the retry must drop %s — the parameter the provider refused", tc.reject)
			}
			if bodies[1][tc.carrier] != float64(42) {
				t.Fatalf("retry %s = %v, want 42 — the surviving spelling must still carry the cap", tc.carrier, bodies[1][tc.carrier])
			}
		})
	}
}

// The learned quirk is remembered: the second call spends no refusal round
// trip re-learning it.
func TestOpenAIClient_RemembersTheLearnedQuirk(t *testing.T) {
	var calls int
	var bodies []map[string]any
	srv := adaptServer(t, "max_completion_tokens",
		"Unrecognized request argument supplied: max_completion_tokens", &calls, &bodies)
	defer srv.Close()

	c := newTestClient(srv)
	for _, line := range []string{"boom", "boom2"} {
		if _, err := c.Complete(context.Background(), testScenario(16384), Input{Lines: []string{line}}); err != nil {
			t.Fatalf("Complete(%s): %v", line, err)
		}
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 — two completions, the refusal learned once", calls)
	}
}

// streamAnswer is the strict-shape payload every happy-path stream carries;
// tests may split or repeat it freely.
const streamAnswer = `{"problem":"p","cause":"c","confidence":"high","fix":null,"investigate":[]}`

// done closes every scripted stream.
const done = "data: [DONE]\n\n"

// Drives the client against a scripted SSE server and asserts the assembled
// JSON, the usage numbers and the request body the scenario dictated.
func TestOpenAIClient_StreamAssemblesAnswerAndUsage(t *testing.T) {
	answer := streamAnswer
	half := len(answer) / 2
	var gotReq map[string]any
	srv := httptest.NewServer(sseHandler(t, []string{
		"data: {\"model\":\"gpt-test-2025-01\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, answer[:half]) + "}}],\"usage\":null}\n\n",
		"data: {\"model\":\"gpt-test-2025-01\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, answer[half:]) + "},\"finish_reason\":\"stop\"}],\"usage\":null}\r\n\r\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":45}}\n\n",
		done,
	}, &gotReq))
	defer srv.Close()

	c := testOpenAIAt(srv.URL + "/") // trailing slash on purpose: the client must trim it
	comp, err := c.Complete(context.Background(), testScenario(16384), Input{
		Lines:   []string{"boom"},
		Context: []string{"services in window: api"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if string(comp.RawJSON) != answer {
		t.Fatalf("assembled JSON = %q, want the two chunks joined into the full answer", comp.RawJSON)
	}
	if comp.PromptTokens != 120 || comp.CompletionTokens != 45 {
		t.Fatalf("usage = %d/%d, want 120/45 from the final usage chunk", comp.PromptTokens, comp.CompletionTokens)
	}
	if comp.Model != "gpt-test-2025-01" {
		t.Fatalf("model = %q, want the name the stream carried, not the configured alias", comp.Model)
	}

	// The request must carry what the scenario and input dictate, not constants.
	if gotReq["model"] != "gpt-test" {
		t.Errorf("request model = %v, want gpt-test", gotReq["model"])
	}
	if gotReq["temperature"] != 0.5 {
		t.Errorf("request temperature = %v, want the scenario's 0.5", gotReq["temperature"])
	}
	if gotReq["max_completion_tokens"] != float64(42) {
		t.Errorf("request max_completion_tokens = %v, want the scenario's 42", gotReq["max_completion_tokens"])
	}
	if gotReq["stream"] != true {
		t.Errorf("request stream = %v, want true", gotReq["stream"])
	}
	if so, ok := gotReq["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("request stream_options = %v, want include_usage true", gotReq["stream_options"])
	}
	if rf, ok := gotReq["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("request response_format = %v, want json_object", gotReq["response_format"])
	}
	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("request messages = %v, want system then user", gotReq["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	usr, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are the test system prompt." {
		t.Errorf("system message = %v, want the scenario's prompt", msgs[0])
	}
	if usr["role"] != "user" || usr["content"] != "<context>\nservices in window: api\n</context>\n\n<log-lines>\nboom\n</log-lines>\n" {
		t.Errorf("user message = %v, want the framed UserMessage", msgs[1])
	}
}

// Streams more than MaxOutputBytes and asserts the call fails instead of
// draining; nothing assembled is returned to be charged for.
func TestOpenAIClient_AbortsPastByteCap(t *testing.T) {
	big := strings.Repeat(streamAnswer, 128) // ≥ 6000 bytes for the two slices below
	srv := httptest.NewServer(sseHandler(t, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, big[:3000]) + "}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, big[3000:6000]) + "}}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n",
		done,
	}, nil))
	defer srv.Close()

	const cap = 4096
	c := newTestClient(srv)
	comp, err := c.Complete(context.Background(), testScenario(cap), Input{Lines: []string{"boom"}})
	if err == nil {
		t.Fatalf("a stream past the %d-byte cap must fail, got a completion of %d bytes", cap, len(comp.RawJSON))
	}
	if !strings.Contains(err.Error(), fmt.Sprint(cap)) {
		t.Fatalf("the abort error must name the cap; got %v", err)
	}
	if comp.RawJSON != nil {
		t.Fatalf("an aborted stream must not return partial content; got %d bytes", len(comp.RawJSON))
	}
}

// The error carries the status plus the first bytes of the provider's own
// body, so a misconfigured gateway says so.
func TestOpenAIClient_Non2xxCarriesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
	if err == nil {
		t.Fatal("a 401 from the provider must fail the call")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("the error must carry the status and the provider's body; got %v", err)
	}
}

// finish_reason "length" is an error, not a truncated answer; the spend
// travels out with it.
func TestOpenAIClient_FinishReasonLengthIsError(t *testing.T) {
	answer := `{"problem":"p","cause":"c"`
	srv := httptest.NewServer(sseHandler(t, []string{
		"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, answer) + "},\"finish_reason\":\"length\"}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n",
		done,
	}, nil))
	defer srv.Close()

	c := newTestClient(srv)
	comp, err := c.Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
	if err == nil {
		t.Fatalf("finish_reason length must fail, got a %d-byte answer", len(comp.RawJSON))
	}
	if !strings.Contains(err.Error(), "length") {
		t.Fatalf("the error must name the finish reason; got %v", err)
	}
	if comp.PromptTokens != 10 || comp.CompletionTokens != 5 {
		t.Fatalf("usage carried with the truncation error = %d/%d, want the stream's 10/5 — the spend must be recordable", comp.PromptTokens, comp.CompletionTokens)
	}
	if comp.Model != "gpt-test" {
		t.Fatalf("model carried with the truncation error = %q, want the stream's name for the ledger", comp.Model)
	}
	if comp.RawJSON != nil {
		t.Fatal("a truncated stream must not return partial content")
	}
}

// Streams that do produce an answer: what the assembled JSON, the usage
// numbers and the model name must be once the stream ends.
func TestOpenAIClient_AssemblesFromHealthyStreams(t *testing.T) {
	half := len(streamAnswer) / 2
	for _, tc := range []struct {
		name           string
		chunks         []string
		wantPrompt     int
		wantCompletion int
		wantModel      string
	}{
		// No usage chunk means unknown, never a false zero, and the model
		// falls back to the configured name.
		{
			name: "no usage chunk",
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, streamAnswer) + "},\"finish_reason\":\"stop\"}]}\n\n",
				done,
			},
			wantPrompt: -1, wantCompletion: -1, wantModel: "gpt-test",
		},
		// One unparseable line between healthy chunks is skipped, not fatal.
		{
			name: "malformed line between healthy chunks",
			chunks: []string{
				"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, streamAnswer[:half]) + "}}]}\n\n",
				"data: {this is not json\n\n",
				"data: {\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, streamAnswer[half:]) + "}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n",
				done,
			},
			wantPrompt: 7, wantCompletion: 3, wantModel: "gpt-test",
		},
		// The final chunk's model field is the authoritative name for the
		// ledger, not the first delta's.
		{
			name: "model named twice",
			chunks: []string{
				"data: {\"model\":\"gpt-first\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, streamAnswer) + "},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"model\":\"gpt-final\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
				done,
			},
			wantPrompt: 1, wantCompletion: 1, wantModel: "gpt-final",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(sseHandler(t, tc.chunks, nil))
			defer srv.Close()

			comp, err := newTestClient(srv).Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if string(comp.RawJSON) != streamAnswer {
				t.Fatalf("assembled JSON = %q, want the chunks joined into the full answer", comp.RawJSON)
			}
			if comp.PromptTokens != tc.wantPrompt || comp.CompletionTokens != tc.wantCompletion {
				t.Fatalf("usage = %d/%d, want %d/%d", comp.PromptTokens, comp.CompletionTokens, tc.wantPrompt, tc.wantCompletion)
			}
			if comp.Model != tc.wantModel {
				t.Fatalf("model = %q, want %q", comp.Model, tc.wantModel)
			}
		})
	}
}

// The timeout rides the request context: an injected HTTP client is still
// cut at c.Timeout. Parking on r.Context().Done() would hang srv.Close().
func TestOpenAIClient_InjectedClientStillTimesOut(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-released
	}))
	defer srv.Close()

	c := newTestClient(srv)
	c.Timeout = 50 * time.Millisecond
	c.httpClient = &http.Client{} // no Timeout of its own; the context must bind
	_, err := c.Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
	close(released)
	if err == nil {
		t.Fatal("a provider slower than c.Timeout must fail the call")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("the failure must be the context deadline; got %v", err)
	}
}

// Every stream that cannot produce an answer fails the call, and the error
// carries the stream's own reason — never a bare "no content", never a
// dangling colon.
func TestOpenAIClient_FailingStreamsNameTheirReason(t *testing.T) {
	runOfGarbage := make([]string, 40)
	for i := range runOfGarbage {
		runOfGarbage[i] = "data: {garbage line " + fmt.Sprint(i) + "\n\n"
	}
	// Alternating valid-but-empty and garbage never trips the run cap, so the
	// total bound is the only honest stop short of the deadline.
	var totalGarbage []string
	for i := 0; i < 80; i++ {
		totalGarbage = append(totalGarbage,
			"data: {\"choices\":[]}\n\n", // parses, carries nothing: resets the run
			"data: {garbage "+fmt.Sprint(i)+"\n\n")
	}

	for _, tc := range []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name: "error object mid-stream",
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"{\"}}]}\n\n",
				"data: {\"error\":{\"message\":\"You exceeded your current quota\",\"type\":\"insufficient_quota\",\"code\":\"insufficient_quota\"}}\n\n",
				done,
			},
			want: []string{"You exceeded your current quota"},
		},
		// No message means type + code; neither means an explicit sentence.
		{
			name:   "error object carrying only type and code",
			chunks: []string{`data: {"error":{"type":"insufficient_quota","code":"quota_exceeded"}}` + "\n\n", done},
			want:   []string{"insufficient_quota quota_exceeded"},
		},
		{
			name:   "error object with no fields at all",
			chunks: []string{`data: {"error":{}}` + "\n\n", done},
			want:   []string{"provider reported an error with no message"},
		},
		// A string-shaped error object is the nothing-parseable case.
		{
			name:   "string-shaped error object",
			chunks: []string{`data: {"error":"rate limited by upstream"}` + "\n\n", done},
			want:   []string{"no content", "rate limited by upstream"},
		},
		{
			name:   "a run of unparseable lines",
			chunks: runOfGarbage,
			want:   []string{"in a row", "garbage line 0"},
		},
		{
			name:   "unparseable lines past the total bound",
			chunks: totalGarbage,
			want:   []string{"in total", "garbage 0"},
		},
		// Any finish reason other than "stop" is an error, never an answer.
		{
			name: "finish reason other than stop",
			chunks: []string{
				"data: {\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, `{"problem":"p"`) + "},\"finish_reason\":\"content_filter\"}]}\n\n",
				done,
			},
			want: []string{"content_filter"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(sseHandler(t, tc.chunks, nil))
			defer srv.Close()

			_, err := newTestClient(srv).Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
			if err == nil {
				t.Fatal("a stream that carries no answer must fail the call")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to carry %q", err.Error(), want)
				}
			}
			if strings.HasSuffix(err.Error(), ": ") {
				t.Fatalf("error = %q, must not end in a dangling colon", err.Error())
			}
		})
	}
}

// The streamed name lands in an unbounded column; it stays capped and valid
// UTF-8 after the cut.
func TestOpenAIClient_BoundsStreamedModelName(t *testing.T) {
	long := strings.Repeat("é", 200) // multibyte: the cut must not split a rune
	srv := httptest.NewServer(sseHandler(t, []string{
		"data: {\"model\":\"" + long + "\",\"choices\":[{\"delta\":{\"content\":" + jsonMust(t, streamAnswer) + "},\"finish_reason\":\"stop\"}]}\n\n",
		done,
	}, nil))
	defer srv.Close()

	comp, err := newTestClient(srv).Complete(context.Background(), testScenario(16384), Input{Lines: []string{"boom"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(comp.Model) > modelCap {
		t.Fatalf("streamed model name is %d bytes, want at most %d", len(comp.Model), modelCap)
	}
	if !utf8.ValidString(comp.Model) {
		t.Fatalf("bounded model name is not valid UTF-8: %q", comp.Model)
	}
}

// TestCutAt pins the shared bounding helper: byte cap, rune-safe cut.
func TestCutAt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 128, "short"},
		{"gpt-4o-mini", 4, "gpt-"},
		{"a££££", 2, "a"},     // the cut at 2 lands inside the first £
		{"a££££", 3, "a£"},    // the cut at 3 is the boundary after one £
		{"日本語のモデル", 9, "日本語"}, // 3 runes × 3 bytes = 9, exact fit
	} {
		if got := cutAt(tc.in, tc.n); got != tc.want || !utf8.ValidString(got) {
			t.Fatalf("cutAt(%q, %d) = %q, want %q (and valid UTF-8)", tc.in, tc.n, got, tc.want)
		}
	}
}

func jsonMust(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return string(b)
}
