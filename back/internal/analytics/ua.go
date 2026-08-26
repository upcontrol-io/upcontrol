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
var botSubstrings = []string{"bot", "crawl", "spider", "slurp", "headless", "lighthouse", "monitoring"}

func parseUA(raw string) ua {
	s := strings.ToLower(raw)
	for _, m := range botSubstrings {
		if strings.Contains(s, m) {
			return ua{Device: "bot"}
		}
	}
	return ua{Device: deviceOf(s), OS: osOf(s), Browser: browserOf(s)}
}

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
