package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// OpenAISettings is one resolution of the three operator knobs: the endpoint
// (OpenAI itself or any proxy/gateway speaking the same wire format), the
// model name, and the key. All three can arrive from the environment at boot
// or from the Settings screen at runtime (instance_setting) — the resolver,
// not this client, decides precedence.
type OpenAISettings struct {
	BaseURL string // e.g. https://api.openai.com/v1
	Model   string
	Key     string // empty = not configured; the value is never logged
}

// OpenAIClient talks to any OpenAI-compatible chat-completions endpoint.
// Settings resolve per call, so a model, base URL or key pasted into the
// Settings screen takes effect on the next question — no restart. An empty
// key means Explain is not configured: Complete answers ErrNotConfigured,
// never a guessed fallback.
type OpenAIClient struct {
	Settings   func(ctx context.Context) OpenAISettings
	Timeout    time.Duration
	HTTPClient *http.Client // injectable for tests; Timeout rides the context either way
	// LogPrompt echoes the exact system+user bytes of every call to the log
	// (the prompt-editing loop: see it, edit scenario.go, see it again).
	LogPrompt bool
}

func (c *OpenAIClient) settings(ctx context.Context) OpenAISettings {
	if c.Settings == nil {
		return OpenAISettings{}
	}
	return c.Settings(ctx)
}

// Configured reports whether a key resolves right now — the Accountant asks
// before spending anything, and the explain preview turns it into the
// front's "is the feature on" fact.
func (c *OpenAIClient) Configured(ctx context.Context) bool {
	return c.settings(ctx).Key != ""
}

// maxConsecutiveGarbage is how many unparseable data lines in a row the
// reader tolerates before declaring the endpoint broken; maxSkippedChunks is
// the same bound on the total, whatever the spacing — a gateway alternating
// one valid-but-empty chunk with one garbage line never trips the run cap
// and assembles no content (so MaxOutputBytes cannot trip either), and
// without the total the only stop is the deadline.
const (
	maxConsecutiveGarbage = 16
	maxSkippedChunks      = 64
)

// ID names the answering brain for the cache hash (Decision 7). The model is
// the operator-configured one, not a name streamed by the provider — the
// stream must not steer the cache identity — and the base URL rides along
// because one model name behind two gateways is two different brains:
// gpt-4o-mini on OpenAI, on an Azure deployment and behind a LiteLLM proxy
// answer differently under one name. The URL is operator config, never
// provider input, so it is safe to hash; the trailing slash is trimmed the
// way the request path trims it, so the same gateway spelled both ways stays
// one brain. Resolved per call: changing the model in Settings IS changing
// the brain, and the cache splits with it.
func (c *OpenAIClient) ID(ctx context.Context) string {
	s := c.settings(ctx)
	return "openai:" + strings.TrimRight(s.BaseURL, "/") + ":" + s.Model
}

// modelCap bounds the streamed model name before it reaches ai_call.model:
// the column is unbounded text and the name is provider-controlled input —
// a label for the ledger, not a value to store whole.
const modelCap = 128

// paramQuirk records which OpenAI-spec parameter spellings THIS gateway
// refuses. "All OpenAI-compatible" endpoints disagree exactly here: OpenAI's
// newer models reject max_tokens (use max_completion_tokens), older gateways
// reject or ignore max_completion_tokens, reasoning models reject a
// temperature. Learned from the provider's own 400, remembered per brain for
// the life of the process — the first call after a boot pays one extra round
// trip, nothing else does.
type paramQuirk struct {
	dropMaxTokens           bool
	dropMaxCompletionTokens bool
	dropTemperature         bool
}

// paramQuirks maps LLM.ID() → paramQuirk.
var paramQuirks sync.Map

// adapt inspects a provider refusal and returns the quirk that would avoid
// it, with ok=false when the error does not name a parameter this client
// sends (a real error, not a spelling disagreement). Order matters: OpenAI's
// own refusal names BOTH spellings ("'max_tokens' is not supported ... use
// 'max_completion_tokens'"), so the redirect form is matched first.
func (q paramQuirk) adapt(err error) (paramQuirk, bool) {
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "provider http 4") {
		return q, false
	}
	refused := func(name string) bool {
		if !strings.Contains(msg, name) {
			return false
		}
		for _, w := range []string{"unsupported", "not supported", "unknown", "unrecognized", "invalid", "extra", "not permitted", "does not support"} {
			if strings.Contains(msg, w) {
				return true
			}
		}
		return false
	}
	switch {
	case !q.dropMaxTokens && strings.Contains(msg, "max_tokens") && strings.Contains(msg, "max_completion_tokens") && refused("max_tokens"):
		q.dropMaxTokens = true
	case !q.dropMaxCompletionTokens && refused("max_completion_tokens"):
		q.dropMaxCompletionTokens = true
	case !q.dropMaxTokens && refused("max_tokens"):
		q.dropMaxTokens = true
	case !q.dropTemperature && refused("temperature"):
		q.dropTemperature = true
	default:
		return q, false
	}
	return q, true
}

// Complete streams one chat completion, adapting the request to the
// gateway's OpenAI dialect: a 400 that names a parameter spelling is
// answered by re-sending without it (at most once per parameter), and the
// learned quirk sticks for the process. Everything else is completeOnce's
// contract, unchanged.
func (c *OpenAIClient) Complete(ctx context.Context, sc Scenario, input Input) (Completion, error) {
	brain := c.ID(ctx)
	q := paramQuirk{}
	if v, ok := paramQuirks.Load(brain); ok {
		q = v.(paramQuirk)
	}
	for attempt := 0; ; attempt++ {
		comp, err := c.completeOnce(ctx, sc, input, q)
		if err == nil || attempt >= 3 {
			return comp, err
		}
		next, ok := q.adapt(err)
		if !ok {
			return comp, err
		}
		q = next
		paramQuirks.Store(brain, q)
		slog.Info("ai: provider parameter quirk learned, retrying", "brain", brain,
			"drop_max_tokens", q.dropMaxTokens, "drop_max_completion_tokens", q.dropMaxCompletionTokens,
			"drop_temperature", q.dropTemperature)
	}
}

// completeOnce sends one chat completion and returns the assembled answer
// with the usage numbers the provider reported on the final chunk. Tokens
// stay -1 (unknown) when the stream carried no usage; an error object pushed
// mid-stream and any finish reason but "stop" are errors, never answers — a
// truncation error still carries the model and usage out with it, because
// the provider was paid either way. Unparseable data lines are skipped and
// counted (a run of maxConsecutiveGarbage in a row fails the call); when
// nothing contentful arrived, the skip count and the first malformed line
// travel with the returned error instead of a generic "no content".
func (c *OpenAIClient) completeOnce(ctx context.Context, sc Scenario, input Input, q paramQuirk) (Completion, error) {
	s := c.settings(ctx)
	if s.Key == "" {
		return Completion{}, ErrNotConfigured
	}
	reqShape := chatRequest{
		Model:               s.Model,
		Temperature:         sc.Temperature,
		MaxTokens:           sc.MaxOutputTokens,
		MaxCompletionTokens: sc.MaxOutputTokens,
		ReasoningEffort:     sc.ReasoningEffort,
		Stream:              true,
		StreamOptions:       streamOptions{IncludeUsage: true},
		ResponseFormat:      responseFormat{Type: "json_object"},
		Messages: []chatMessage{
			{Role: "system", Content: sc.SystemPrompt},
			{Role: "user", Content: input.UserMessage()},
		},
	}
	if q.dropMaxTokens {
		reqShape.MaxTokens = 0
	}
	if q.dropMaxCompletionTokens {
		reqShape.MaxCompletionTokens = 0
	}
	if q.dropTemperature {
		reqShape.Temperature = 0
	}
	body, err := json.Marshal(reqShape)
	if err != nil {
		return Completion{}, fmt.Errorf("ai: building provider request: %w", err)
	}
	if c.LogPrompt {
		slog.Info("ai: prompt", "scenario", sc.Key, "model", s.Model,
			"system", sc.SystemPrompt, "user", input.UserMessage())
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}

	// The timeout rides the context, not http.Client.Timeout, so it binds the
	// call no matter who supplied the client (an injected client ignores the
	// field). Returning from Complete — normally, or on the byte-cap abort
	// below — runs the deferred cancel and the deferred body close, so a
	// runaway model is cut off mid-stream, never drained to its end.
	var streamCtx context.Context
	var cancel context.CancelFunc
	if c.Timeout > 0 {
		streamCtx, cancel = context.WithTimeout(ctx, c.Timeout)
	} else {
		streamCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost,
		strings.TrimRight(s.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Completion{}, fmt.Errorf("ai: building provider request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := hc.Do(req)
	if err != nil {
		return Completion{}, fmt.Errorf("ai: provider request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("ai: closing provider response body", "err", cerr)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Completion{}, fmt.Errorf("ai: provider HTTP %d: %s", resp.StatusCode, snippet)
	}

	var content []byte
	var model string
	// -1 = the stream never said; a false 0 would read as a free call.
	promptTokens, completionTokens := -1, -1
	// Garbage accounting: one broken line in an otherwise healthy stream is
	// the provider's glitch — skipped, counted, and named once after the
	// loop — but a run of them is a broken endpoint, bounded below; without
	// the bound, a garbage-only stream would read and log one warn per line
	// until the deadline (MaxOutputBytes counts assembled content, and
	// garbage assembles none).
	skipped, consecutive := 0, 0
	var firstBadLine string
	var firstParseErr error
	// Any finish reason but "stop" means the assembled text is not the whole
	// answer ("length" = cut at max_tokens); charging it as one would be a
	// lie. The stream is still drained past it: the usage chunk that follows
	// carries the spend out with the error.
	var badFinish string
	scanner := bufio.NewScanner(resp.Body)
	// One SSE line carries one whole JSON chunk; 1 MiB is far past any honest
	// delta yet still bounds memory against a broken endpoint.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue // event boundary, comment, or another SSE field
		}
		// SSE allows exactly one space between field name and value.
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		if payload == "[DONE]" {
			break
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			skipped++
			consecutive++
			if firstParseErr == nil {
				firstParseErr, firstBadLine = err, cutAt(payload, 120)
			}
			if consecutive >= maxConsecutiveGarbage {
				return Completion{}, fmt.Errorf(
					"ai: provider stream sent %d malformed chunks in a row, first %q: %w", consecutive, firstBadLine, firstParseErr)
			}
			if skipped >= maxSkippedChunks {
				return Completion{}, fmt.Errorf(
					"ai: provider stream sent %d malformed chunks in total, first %q: %w", skipped, firstBadLine, firstParseErr)
			}
			continue
		}
		consecutive = 0
		if e := chunk.Error; e != nil {
			// Some providers fail mid-stream (quota, content filter) instead
			// of answering; the empty assemble below would bury why.
			msg := e.Message
			if msg == "" {
				msg = strings.TrimSpace(e.Type + " " + e.Code)
			}
			if msg == "" {
				msg = "provider reported an error with no message"
			}
			return Completion{}, fmt.Errorf("ai: provider stream error: %s", msg)
		}
		if chunk.Model != "" {
			// The name lands in ai_call.model, an unbounded column, and it
			// arrives on a provider-controlled stream: bound it.
			model = cutAt(chunk.Model, modelCap)
		}
		if len(chunk.Choices) > 0 {
			// Past a bad finish reason the deltas are not an answer anymore;
			// reading continues only so the usage chunk reaches the caller.
			if badFinish == "" {
				content = append(content, chunk.Choices[0].Delta.Content...)
			}
			if reason := chunk.Choices[0].FinishReason; reason != "" && reason != "stop" {
				badFinish = reason
			}
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
		}
		if len(content) > sc.MaxOutputBytes {
			return Completion{}, fmt.Errorf("ai: provider output passed the %d-byte cap", sc.MaxOutputBytes)
		}
	}
	if err := scanner.Err(); err != nil {
		return Completion{}, fmt.Errorf("ai: reading provider stream: %w", err)
	}
	if skipped > 0 {
		slog.Warn("ai: skipped malformed provider chunks", "count", skipped, "first", firstBadLine)
	}
	if model == "" {
		model = s.Model
	}
	if badFinish != "" {
		// The spend is real, so the model name and the usage travel out with
		// the error. RawJSON deliberately stays nil: a truncated answer must
		// never be parsed or served.
		return Completion{Model: model, PromptTokens: promptTokens, CompletionTokens: completionTokens},
			fmt.Errorf("ai: provider finish_reason %q, answer incomplete", badFinish)
	}
	if len(content) == 0 {
		if skipped > 0 {
			// Nothing contentful arrived and lines were unparseable — the
			// stream's own reason (a string-shaped error object included) is
			// in those lines, not in a generic "no content".
			return Completion{}, fmt.Errorf(
				"ai: provider stream carried no content; %d malformed chunks, first %q: %w", skipped, firstBadLine, firstParseErr)
		}
		return Completion{}, errors.New("ai: provider stream carried no content")
	}
	return Completion{
		RawJSON:          content,
		Model:            model,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
	}, nil
}

// chatRequest is the OpenAI chat-completions body. The output cap rides
// max_completion_tokens — the current OpenAI parameter; the gpt-5 and o-series
// families reject the legacy max_tokens outright (HTTP 400,
// "unsupported_parameter"). A compatible gateway that only knows max_tokens
// ignores the field, and MaxOutputBytes still aborts a runaway stream.
// Temperature is omitted at 0 (the provider's default answers): the same
// reasoning families reject any non-default value.
type chatRequest struct {
	Model       string  `json:"model"`
	Temperature float32 `json:"temperature,omitempty"`
	// Both spellings of the output cap ride by default: OpenAI's newer
	// models take max_completion_tokens (and reject max_tokens), most
	// compatible gateways take max_tokens (and silently IGNORE the other —
	// an uncapped call, not an error). A 400 naming either parameter
	// teaches paramQuirks which one this gateway wants; the client-side
	// MaxOutputBytes abort bounds the stream regardless.
	MaxTokens           int    `json:"max_tokens,omitempty"`
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   streamOptions  `json:"stream_options"`
	ResponseFormat  responseFormat `json:"response_format"`
	Messages        []chatMessage  `json:"messages"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type string `json:"type"` // "json_object" — the whole answer is one JSON object
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatChunk is one SSE payload: an accumulated delta plus, on the final chunk
// before [DONE], the usage numbers. Error is set only when the provider
// pushes an error object mid-stream instead of finishing the completion.
type chatChunk struct {
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
	Error   *chatError   `json:"error"`
}

type chatChoice struct {
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"` // "" mid-stream, "stop" on the last delta
}

// chatError is the error object inside a provider error chunk.
type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type chatDelta struct {
	Content string `json:"content"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// cutAt truncates s to at most n bytes without splitting a rune, so a
// bounded provider string stays valid UTF-8.
func cutAt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // the cut landed inside a multi-byte rune; back up to its start
	}
	return s[:n]
}
