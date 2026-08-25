// Channel is the interface every alert delivery channel implements; Send
// returns the HTTP status (0 for a network error) for ClassifyError.

package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
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

// AlertPayload is the JSON payload sent to a channel, carrying enough context
// for each channel to format its own message.
type AlertPayload struct {
	Title       string         `json:"title"`
	Status      string         `json:"status"` // down|check|ok
	IncidentID  string         `json:"incident_id"`
	MonitorName string         `json:"monitor_name"`
	Actions     []ActionButton `json:"actions,omitempty"`
	Fields      []Field        `json:"fields,omitempty"`
	// Summary is the one sentence under the title, and only a measured one;
	// no renderer invents a line to fill the gap.
	Summary string `json:"summary,omitempty"`
	// Lines is machine output the detector already had in hand; LinesLabel
	// names what they are.
	Lines      []string `json:"lines,omitempty"`
	LinesLabel string   `json:"lines_label,omitempty"`
	// Class is the delivery's own class (test|page|ticket|followup), copied
	// on by the worker: the same status can read differently per class.
	Class string `json:"class,omitempty"`
	// Detector names the detector behind the incident, set only for detection
	// incidents; telegram and email pick their button set and badge by it.
	Detector string `json:"detector,omitempty"`
	// Buttons: set by the worker for every telegram channel; a press is
	// authorised by WHO pressed it, never by the chat it landed in.
	Buttons bool `json:"buttons,omitempty"`
	// Group: set for a broadcast group channel; the Bot API refuses web_app
	// buttons outside private chats, so the keyboard drops its Open row there.
	Group bool `json:"group,omitempty"`
}

// ActionButton is an inline button (Telegram) or link (email/discord/slack).
type ActionButton struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// Field is one label/value row of a fact table; Mono renders the value
// monospaced. A slice, not a map: map iteration is randomized on purpose.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Mono  bool   `json:"mono,omitempty"`
}

// HTTPClient is overridable for tests.
var HTTPClient = &http.Client{Timeout: 10 * time.Second}

// TelegramChannel sends alerts via the Bot API. The token resolves per send
// (env at boot or Settings at runtime); empty fails with a named error.
type TelegramChannel struct {
	Token func(ctx context.Context) string
	// AppURL is the account app's public origin plus /app — where the
	// message's closing link sends the reader. Empty renders no link.
	AppURL string
}

func (c *TelegramChannel) Kind() string { return "telegram" }

func (c *TelegramChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	token := ""
	if c.Token != nil {
		token = c.Token(ctx)
	}
	if token == "" {
		// Undelivered alerts are named, not silent: the row says why, instead
		// of the Bot API 404ing on an empty token path.
		return 0, fmt.Errorf("telegram: no bot token configured (Settings, or UC_TELEGRAM_BOT_TOKEN)")
	}
	// target is a chat ID or @channel.
	text := formatTelegram(p, c.AppURL)
	body := map[string]any{
		"chat_id":    target,
		"text":       text,
		"parse_mode": "HTML",
	}
	if kb := telegramKeyboard(p, c.AppURL); kb != nil {
		body["reply_markup"] = map[string]any{"inline_keyboard": kb}
	}
	encoded, _ := json.Marshal(body)
	return doPost(ctx, fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token), "", encoded)
}

// telegramKeyboard is the inline keyboard under an alert, nil with no actions.
// web_app needs https and a private chat; detector pages drop Resolve.
func telegramKeyboard(p AlertPayload, appURL string) [][]map[string]any {
	if !p.Buttons || p.IncidentID == "" {
		return nil
	}
	rows := [][]map[string]any{
		{{"text": "Acknowledge", "callback_data": "ack:" + p.IncidentID}},
	}
	if p.Detector == "" {
		rows = append(rows, []map[string]any{
			{"text": "Resolve", "callback_data": "resolve:" + p.IncidentID},
		})
	}
	if !p.Group && strings.HasPrefix(appURL, "https://") {
		label, query := "Open", ""
		if p.Detector != "" {
			label, query = "Explain", "&explain=1"
		}
		rows = append(rows, []map[string]any{{
			"text": label,
			// appURL already ends in /app — alertLink builds on the same value.
			"web_app": map[string]string{
				"url": appURL + "?incident=" + url.QueryEscape(p.IncidentID) + query,
			},
		}})
	}
	return rows
}

// EmailChannel sends alert emails through the external email agent: one POST
// per alert; the agent owns queue, retries and provider.
type EmailChannel struct {
	APIURL string // service base, e.g. http://mail-agent:8080
	APIKey string // bearer token; empty = the service runs with auth disabled
	AppURL string // the account app's public origin, for the mail's CTA
}

func (c *EmailChannel) Kind() string { return "email" }

// Send hands the agent the facts and lets it render; a self-host without the
// agent still gets the plain-text half from SMTPChannel below.
func (c *EmailChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	vars := map[string]any{
		"class":  p.Class,
		"status": p.Status,
		"title":  p.Title,
		"to":     target,
		// The two the mail's button is built from: the origin this deployment
		// answers on, and the incident the reader was written to about.
		"app_url":     c.AppURL,
		"incident_id": p.IncidentID,
	}
	if p.Summary != "" {
		vars["summary"] = p.Summary
	}
	// A detector page is not an outage page: the agent needs to know which
	// one this is, or it prints "Down" over a spike.
	if p.Detector != "" {
		vars["detector"] = p.Detector
	}
	if fields := alertFields(p); len(fields) > 0 {
		vars["fields"] = fields
	}
	if len(p.Lines) > 0 {
		vars["lines"] = p.Lines
		vars["lines_label"] = p.LinesLabel
	}
	if len(p.Actions) > 0 {
		vars["actions"] = p.Actions
	}
	body, _ := json.Marshal(map[string]any{
		"kind":     "notification",
		"to":       target,
		"template": "alert",
		"vars":     vars,
	})
	return doPost(ctx, strings.TrimRight(c.APIURL, "/")+"/send", c.APIKey, body)
}

// alertFields is the mail's fact table: what the queue always knows first,
// then whatever the detector attached, in order.
func alertFields(p AlertPayload) [][]any {
	out := make([][]any, 0, len(p.Fields)+2)
	if p.MonitorName != "" {
		out = append(out, []any{"Monitor", p.MonitorName, false})
	}
	for _, f := range p.Fields {
		out = append(out, []any{f.Label, f.Value, f.Mono})
	}
	if p.IncidentID != "" {
		out = append(out, []any{"Incident", p.IncidentID, true})
	}
	return out
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

// statusEmoji pairs a shape with the status; the words beside it carry the
// same fact, so colour is never the only channel.
func statusEmoji(status string) string {
	switch status {
	case "down":
		return "🔴"
	case "check":
		return "🟠"
	default:
		return "🟢"
	}
}

// formatTelegram renders the alert layout in Telegram's HTML dialect. Every
// dynamic string is escaped: one '<' in a title and the Bot API rejects it.
func formatTelegram(p AlertPayload, appURL string) string {
	esc := html.EscapeString
	s := statusEmoji(p.Status) + " <b>" + esc(p.Title) + "</b>\n"
	if p.Summary != "" {
		s += "\n" + esc(p.Summary) + "\n"
	}
	// No "Monitor:" row here: the title already names the monitor, and a
	// phone screen has no height to spend saying it twice.
	if len(p.Fields) > 0 {
		s += "\n"
		for _, f := range p.Fields {
			v := esc(f.Value)
			if f.Mono {
				v = "<code>" + v + "</code>"
			}
			s += esc(f.Label) + ": " + v + "\n"
		}
	}
	if len(p.Lines) > 0 {
		lines := make([]string, len(p.Lines))
		for i, line := range p.Lines {
			lines[i] = esc(line)
		}
		s += "\n<pre>" + strings.Join(lines, "\n") + "</pre>\n"
	}
	// The class link joins the payload's own actions so one loop writes every
	// anchor. Copied first: an append must not scribble on the payload.
	links := append([]ActionButton(nil), p.Actions...)
	if href, label := alertLink(p, appURL); href != "" {
		links = append(links, ActionButton{Label: label, URL: href})
	}
	for _, a := range links {
		if a.URL != "" {
			s += "\n<a href=\"" + esc(a.URL) + "\">" + esc(a.Label) + "</a>"
		}
	}
	return s
}

// alertLink is the message's closing link, chosen by the delivery's class —
// the same routing the email template applies to its button.
func alertLink(p AlertPayload, appURL string) (href, label string) {
	if appURL == "" {
		return "", ""
	}
	switch p.Class {
	case "test":
		return appURL + "/alerts", "Alert settings"
	case "ticket":
		return appURL, "Open this log group"
	}
	if p.IncidentID == "" {
		return appURL, "Open the dashboard"
	}
	href = appURL + "?incident=" + url.QueryEscape(p.IncidentID)
	// Good news closes differently: the recovered follow-up's link is the
	// quiet "it is over" line, not a call to action.
	if p.Class == "followup" && p.Status == "ok" {
		return href, "The incident and its timeline are on the dashboard"
	}
	return href, "Open the incident"
}

// formatEmail is the plain-text body every channel that is not the email agent
// sends: SMTP, and the prose half of telegram/discord/slack.
func formatEmail(p AlertPayload) string {
	s := p.Title + "\n"
	if p.Summary != "" {
		s += p.Summary + "\n"
	}
	if p.MonitorName != "" {
		s += "Monitor: " + p.MonitorName + "\n"
	}
	for _, f := range p.Fields {
		s += f.Label + ": " + f.Value + "\n"
	}
	if len(p.Lines) > 0 {
		label := p.LinesLabel
		if label == "" {
			label = "Detail"
		}
		s += "\n" + label + ":\n"
		for _, line := range p.Lines {
			s += "  " + line + "\n"
		}
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
