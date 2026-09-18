package analytics

import "strings"

// ua is the parsed User-Agent taxonomy, hand-written, no dependency. Device is
// desktop|mobile|tablet|bot; OS and Browser lowercase or "".
type ua struct {
	Device  string
	OS      string
	Browser string
}

// botSubstrings are the automated-visitor markers; a match is device=bot
// regardless of the rest of the string.
var botSubstrings = []string{"crawl", "spider", "slurp", "headless", "lighthouse", "monitoring"}

// hasBotToken finds "bot" as a product token (Googlebot/2.1, Slackbot-Link…, "+…/bot.html"),
// not as letters inside a word: Android puts the handset model in its User-Agent, and a
// CUBOT phone is a visitor. It matters since the list gates a customer's ingest, where a
// false positive is a real person's events dropped with nothing to say so.
func hasBotToken(s string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], "bot")
		if j < 0 {
			return false
		}
		j += i
		end := j + len("bot")
		startsWord := j == 0 || s[j-1] < 'a' || s[j-1] > 'z'
		endsToken := end == len(s) || strings.IndexByte("/-;)", s[end]) >= 0
		if startsWord || endsToken {
			return true
		}
		i = end
	}
}

func parseUA(raw string) ua {
	s := strings.ToLower(raw)
	if hasBotToken(s) {
		return ua{Device: "bot"}
	}
	for _, m := range botSubstrings {
		if strings.Contains(s, m) {
			return ua{Device: "bot"}
		}
	}
	return ua{Device: deviceOf(s), OS: osOf(s), Browser: browserOf(s)}
}

// IsBot reports whether a User-Agent belongs to an automated visitor. Exported
// for the ingest coordinator, which drops a crawler's public-key batch but
// cannot import this package (storage/pg imports ingest), so it takes the
// detector as an injected function instead.
func IsBot(userAgent string) bool { return parseUA(userAgent).Device == "bot" }

func deviceOf(s string) string {
	// Android without "mobile" is a tablet (the Google convention); iPad and
	// the Kindle Silk browser are tablets too.
	if strings.Contains(s, "ipad") || strings.Contains(s, "tablet") ||
		strings.Contains(s, "silk") || strings.Contains(s, "playbook") ||
		(strings.Contains(s, "android") && !strings.Contains(s, "mobile")) {
		return "tablet"
	}
	if strings.Contains(s, "mobi") || strings.Contains(s, "iphone") || strings.Contains(s, "ipod") ||
		strings.Contains(s, "windows phone") || strings.Contains(s, "blackberry") ||
		strings.Contains(s, "opera mini") || strings.Contains(s, "iemobile") {
		return "mobile"
	}
	return "desktop"
}

func osOf(s string) string {
	switch {
	case strings.Contains(s, "windows"):
		return "windows"
	case strings.Contains(s, "iphone"), strings.Contains(s, "ipad"), strings.Contains(s, "ipod"):
		return "ios"
	case strings.Contains(s, "android"):
		return "android"
	case strings.Contains(s, "mac os x"), strings.Contains(s, "macintosh"):
		return "macos"
	case strings.Contains(s, "linux"), strings.Contains(s, "x11"):
		return "linux"
	}
	return ""
}

func browserOf(s string) string {
	switch {
	// Edge first: its ua contains "edg/" and "chrome". Firefox for iOS claims
	// Safari; Chrome claims Safari for everything.
	case strings.Contains(s, "edg/"), strings.Contains(s, "edge/"), strings.Contains(s, "edga/"), strings.Contains(s, "edgios/"):
		return "edge"
	case strings.Contains(s, "firefox"), strings.Contains(s, "fxios"):
		return "firefox"
	case strings.Contains(s, "chrome"), strings.Contains(s, "crios"):
		return "chrome"
	case strings.Contains(s, "safari"):
		return "safari"
	}
	return "other"
}
