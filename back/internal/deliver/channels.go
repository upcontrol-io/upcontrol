// Channel is the interface every alert delivery channel implements. The queue
// worker calls Send for each item; the channel returns the HTTP status code
// (0 for a network error) so ClassifyError can classify the outcome.

package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Channel sends one alert to one target.
type Channel interface {
	// Kind returns the channel type: telegram|email|discord|slack.
	Kind() string
	// Send delivers the payload to the target. statusCode is 0 for network
	// errors; the caller uses ClassifyError to decide retry vs DLQ.
	Send(ctx context.Context, target string, payload AlertPayload) (statusCode int, err error)
}

// AlertPayload is the JSON payload sent to a channel. It carries enough context
// for each channel to format its own message (the plan §9: telegram gets
// buttons, email gets a subject+body, discord/slack get a webhook JSON).
type AlertPayload struct {
	Title       string            `json:"title"`
	Status      string            `json:"status"` // down|check|ok
	IncidentID  string            `json:"incident_id"`
	MonitorName string            `json:"monitor_name"`
	Actions     []ActionButton    `json:"actions,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
	// Buttons: set by the worker for PERSONAL telegram channels only — the
	// message then carries Acknowledge/Resolve inline buttons whose callback
	// data is "ack:<incident_id>" / "resolve:<incident_id>". A broadcast
	// group (no recipient person) never sets it: in a group, the button's
	// presser cannot be name-verified (design D5).
	Buttons bool `json:"buttons,omitempty"`
}

// ActionButton is an inline button (Telegram) or link (email/discord/slack).
type ActionButton struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// --- implementations ---

// HTTPClient is overridable for tests.
var HTTPClient = &http.Client{Timeout: 10 * time.Second}

// TelegramChannel sends alerts via the Telegram Bot API. The token is
// resolved per send: it can arrive from the environment at boot or from the
// Settings screen at runtime (instance_setting), and alerts must start
// flowing the moment it is saved — no restart. Empty resolution fails the
// delivery with a named error, never silently.
type TelegramChannel struct{ Token func(ctx context.Context) string }

func (c *TelegramChannel) Kind() string { return "telegram" }

func (c *TelegramChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	token := ""
	if c.Token != nil {
		token = c.Token(ctx)
	}
	if token == "" {
		// "Undelivered alerts are named, not silent": the delivery row says
		// why, instead of the Bot API 404ing on an empty token path.
		return 0, fmt.Errorf("telegram: no bot token configured (Settings, or UC_TELEGRAM_BOT_TOKEN)")
	}
	// target is a chat ID or @channel.
	text := formatTelegram(p)
	body := map[string]any{
		"chat_id":    target,
		"text":       text,
		"parse_mode": "HTML",
	}
	if p.Buttons && p.IncidentID != "" {
		body["reply_markup"] = map[string]any{
			"inline_keyboard": [][]map[string]string{
				{{"text": "Acknowledge", "callback_data": "ack:" + p.IncidentID}},
				{{"text": "Resolve", "callback_data": "resolve:" + p.IncidentID}},
			},
		}
	}
	encoded, _ := json.Marshal(body)
	return doPost(ctx, fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "", encoded)
}

// EmailChannel sends alert emails through an external email agent,
// the single door for outbound email: one POST per alert to {APIURL}/send,
// and the agent owns the queue, retries and provider. ucworker registers it
// only when UC_EMAIL_URL is set.
type EmailChannel struct {
	APIURL string // service base, e.g. http://mail-agent:8080
	APIKey string // bearer token; empty = the service runs with auth disabled
}

func (c *EmailChannel) Kind() string { return "email" }

func (c *EmailChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"kind":    "notification",
		"to":      target,
		"subject": "[" + p.Status + "] " + p.Title,
		"text":    formatEmail(p),
	})
	return doPost(ctx, strings.TrimRight(c.APIURL, "/")+"/send", c.APIKey, body)
}

// DiscordChannel sends alerts via a Discord webhook URL.
type DiscordChannel struct{}

func (c *DiscordChannel) Kind() string { return "discord" }

func (c *DiscordChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	// target IS the webhook URL.
	body, _ := json.Marshal(map[string]any{
		"embeds": []map[string]any{
			{
				"title":       "[" + p.Status + "] " + p.Title,
				"description": formatEmail(p),
				"color":       statusColor(p.Status),
			},
		},
	})
	return doPost(ctx, target, "", body)
}

// SlackChannel sends alerts via a Slack webhook URL.
type SlackChannel struct{}

func (c *SlackChannel) Kind() string { return "slack" }

func (c *SlackChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	// target IS the webhook URL.
	body, _ := json.Marshal(map[string]any{
		"text": formatEmail(p),
		"blocks": []map[string]any{
			{"type": "header", "text": map[string]string{"type": "plain_text", "text": "[" + p.Status + "] " + p.Title}},
		},
	})
	return doPost(ctx, target, "", body)
}

// --- helpers ---

// doPost sends a JSON POST, optionally with a bearer key, and returns the
// HTTP status code.
func doPost(ctx context.Context, url, bearer string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func formatTelegram(p AlertPayload) string {
	s := fmt.Sprintf("<b>[%s] %s</b>\n%s", p.Status, p.Title, formatEmail(p))
	for _, a := range p.Actions {
		s += fmt.Sprintf("\n<a href=\"%s\">%s</a>", a.URL, a.Label)
	}
	return s
}

func formatEmail(p AlertPayload) string {
	s := p.Title + "\n"
	if p.MonitorName != "" {
		s += "Monitor: " + p.MonitorName + "\n"
	}
	for k, v := range p.Fields {
		s += k + ": " + v + "\n"
	}
	return s
}

func statusColor(status string) int {
	switch status {
	case "down":
		return 0xFF0000 // red
	case "check":
		return 0xFFA500 // orange
	default:
		return 0x00FF00 // green
	}
}
