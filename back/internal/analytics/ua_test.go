package analytics

import "testing"

func TestParseUA(t *testing.T) {
	cases := []struct {
		ua              string
		device, os, brw string
	}{
		// The visitor reality we actually serve: Windows Chrome, macOS Safari,
		// Linux Firefox, iPhone Safari, Android Chrome (phone and tablet),
		// Edge on Windows, curl as a non-browser client.
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "desktop", "windows", "chrome"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15", "desktop", "macos", "safari"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0", "desktop", "linux", "firefox"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1", "mobile", "ios", "safari"},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36", "mobile", "android", "chrome"},
		// Android WITHOUT "mobile" = tablet (Google convention).
		{"Mozilla/5.0 (Linux; Android 13; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36", "tablet", "android", "chrome"},
		{"Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/604.1", "tablet", "ios", "safari"},
		// Edge UAs contain the Chrome token too: Edge must win.
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0", "desktop", "windows", "edge"},
		// Firefox for iOS claims Safari; fxios must win.
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1 FxiOS/126.0", "mobile", "ios", "firefox"},
		{"curl/8.8.0", "desktop", "", "other"},
		{"", "desktop", "", "other"},
		// The bot list: every marker classifies the UA as a bot, whatever else
		// it claims to be.
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", "bot", "", ""},
		{"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/126.0.0.0", "bot", "", ""},
		{"Mozilla/5.0 (compatible; bingbot/2.0)", "bot", "", ""},
		{"Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)", "bot", "", ""},
		{"Mozilla/5.0 (Linux; Android 11) Chrome/90 Mobile Safari/537.36 Lighthouse", "bot", "", ""},
		{"Datadog-Monitoring/1.0", "bot", "", ""},
		{"Mozilla/5.0 (compatible; SemrushBot/7~bl; +http://www.semrush.com/bot.html)", "bot", "", ""},
	}
	for _, c := range cases {
		got := ParseUA(c.ua)
		if got.Device != c.device || got.OS != c.os || got.Browser != c.brw {
			t.Errorf("ParseUA(%q) = {%s %s %s}, want {%s %s %s}",
				c.ua, got.Device, got.OS, got.Browser, c.device, c.os, c.brw)
		}
	}
}
