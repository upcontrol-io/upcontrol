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

// OpenAISettings is one resolution of endpoint, model and key; they can come
// from the environment at boot or Settings at runtime.
type OpenAISettings struct {
	BaseURL string // e.g. https://api.openai.com/v1
	Model   string
	Key     string // empty = not configured; the value is never logged
}

// OpenAIClient talks to any OpenAI-compatible endpoint. Settings resolve per
// call, so a Settings change applies without restart; no key = ErrNotConfigured.
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

// Configured reports whether a key resolves right now; the Accountant asks
// before spending anything.
func (c *OpenAIClient) Configured(ctx context.Context) bool {
	return c.settings(ctx).Key != ""
}

// maxConsecutiveGarbage bounds unparseable lines in a row; maxSkippedChunks
// bounds the total, or spaced garbage would only stop at the deadline.
const (
	maxConsecutiveGarbage = 16
	maxSkippedChunks      = 64
)

// ID names the answering brain for the cache hash: configured model and base
// URL, never a provider-streamed name. Trimmed of the trailing slash.
func (c *OpenAIClient) ID(ctx context.Context) string {
	s := c.settings(ctx)
	return "openai:" + strings.TrimRight(s.BaseURL, "/") + ":" + s.Model
}

// modelCap bounds the provider-streamed model name before it reaches the
// ai_call.model column.
const modelCap = 128

// paramQuirk records which OpenAI-spec parameter spellings this gateway
// refuses, learned from its own 400s and kept per brain for the process life.
type paramQuirk struct {
	dropMaxTokens           bool
	dropMaxCompletionTokens bool
	dropTemperature         bool
	dropReasoningEffort     bool
}

// paramQuirks maps LLM.ID() → paramQuirk.
var paramQuirks sync.Map

// adapt maps a provider refusal to the quirk that avoids it; ok=false when
// the error names no parameter we send. The redirect form is matched first.
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
	// Mirror of the temperature case: a non-reasoning model refuses to be
	// asked for an effort.
	case !q.dropReasoningEffort && refused("reasoning_effort"):
		q.dropReasoningEffort = true
	default:
		return q, false
	}
	return q, true
}

// Complete streams one chat completion, retrying once per parameter a 400
// names; the learned quirk sticks for the process. See completeOnce.
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
			"drop_temperature", q.dropTemperature, "drop_reasoning_effort", q.dropReasoningEffort)
	}
}

// completeOnce sends one chat completion and assembles the answer. Any finish
// reason but "stop" is an error that still carries model and usage out.
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
	if q.dropReasoningEffort {
		reqShape.ReasoningEffort = ""
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

	// The timeout rides the context, not http.Client.Timeout, so it also
	// binds an injected client; a runaway model is cut mid-stream.
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
	// Garbage accounting: broken lines are skipped and counted; a run of
	// them fails the call below instead of logging until the deadline.
	skipped, consecutive := 0, 0
	var firstBadLine string
	var firstParseErr error
	// Any finish reason but "stop" means the text is not the whole answer;
	// the stream still drains so the usage chunk carries the spend out.
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
		// Model and usage travel out with the error; RawJSON stays nil so a
		// truncated answer is never parsed or served.
		return Completion{Model: model, PromptTokens: promptTokens, CompletionTokens: completionTokens},
			fmt.Errorf("ai: provider finish_reason %q, answer incomplete", badFinish)
	}
	if len(content) == 0 {
		if skipped > 0 {
			// Nothing contentful arrived and lines were unparseable; the
			// stream's own reason is in those lines, not "no content".
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

// chatRequest is the OpenAI chat-completions body; the client-side
// MaxOutputBytes abort bounds the stream whatever the gateway does with the cap fields.
type chatRequest struct {
	Model       string  `json:"model"`
	Temperature float32 `json:"temperature,omitempty"`
	// Both output-cap spellings ride by default; a 400 naming either teaches
	// paramQuirks which one this gateway takes.
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       streamOptions  `json:"stream_options"`
	ResponseFormat      responseFormat `json:"response_format"`
	Messages            []chatMessage  `json:"messages"`
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

// chatChunk is one SSE payload: a delta, usage on the final chunk, and an
// error object only on a mid-stream failure.
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
