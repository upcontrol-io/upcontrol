package ogrender

import (
	"bytes"
	"image"
	"testing"
)

// decode reads the PNG header back: a rendered image must be a real,
// decodable 1200x630 PNG.
func decode(t *testing.T, b []byte) image.Config {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return cfg
}

func TestRenderFullPageDecodesAt1200x630(t *testing.T) {
	bars := make([]string, 24)
	for i := range bars {
		bars[i] = "ok"
	}
	bars[7] = "down"
	bars[19] = "nodata"
	b, err := Render(Page{
		Host:          "datrade.io",
		StateSentence: "As of 12:04 UTC, datrade.io answered HTTP 200 in 230 ms from our check.",
		CheckedLine:   "checked 3 min ago",
		Components: []Component{
			{Name: "datrade.io", Bars: bars},
			{Name: "api.datrade.io", Bars: bars},
		},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cfg := decode(t, b)
	if cfg.Width != 1200 || cfg.Height != 630 {
		t.Fatalf("image is %dx%d, want 1200x630", cfg.Width, cfg.Height)
	}
}

// A page with no measurements yet renders "no data yet": the image must
// still decode, never panic, never come back empty.
func TestRenderNoDataPageDecodes(t *testing.T) {
	b, err := Render(Page{Host: "datrade.io"})
	if err != nil {
		t.Fatalf("render no-data page: %v", err)
	}
	cfg := decode(t, b)
	if cfg.Width != 1200 || cfg.Height != 630 {
		t.Fatalf("no-data image is %dx%d, want 1200x630", cfg.Width, cfg.Height)
	}
}

// A long state sentence wraps instead of running off the canvas, and an
// overlong one is ellipsised on its second line without error.
func TestRenderWrapsLongSentences(t *testing.T) {
	long := "Our check has had no answer from example-with-a-very-long-name.example since 11:40 UTC (connection timed out), and this sentence is deliberately longer than one line can hold."
	if _, err := Render(Page{
		Host:          "example-with-a-very-long-name.example",
		StateSentence: long,
		CheckedLine:   "checked 12 min ago",
		Components:    []Component{{Name: "root", Bars: []string{"down"}}},
	}); err != nil {
		t.Fatalf("render long sentence: %v", err)
	}
}
