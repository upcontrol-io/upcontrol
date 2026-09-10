// Package ogrender draws the Open Graph image of a public status page
// (plan part 4): an achromatic 1200x630 PNG built from the same measured
// data the HTML door renders. No saturated colors on purpose (the house
// palette rule): ok reads dark, trouble reads grey, absence reads light.
package ogrender

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// The static Inter TTFs (rsms/inter v4.1, SIL OFL 1.1; see
// assets/OFL-LICENSE.txt) are embedded so the binary carries its own fonts
// and never fetches anything at render time.
//
//go:embed assets/Inter-Regular.ttf assets/Inter-SemiBold.ttf assets/OFL-LICENSE.txt assets/README.md
var assets embed.FS

var (
	fontsOnce sync.Once
	regular   *opentype.Font
	semibold  *opentype.Font
	fontsErr  error
)

// loadFonts parses both faces once; every Render shares the parsed fonts and
// builds its own faces (opentype faces are not safe for concurrent use, and
// a face per render costs microseconds against a 60 s cache lifetime).
func loadFonts() {
	fontsOnce.Do(func() {
		rb, err := assets.ReadFile("assets/Inter-Regular.ttf")
		if err != nil {
			fontsErr = fmt.Errorf("ogrender: read Inter-Regular: %w", err)
			return
		}
		sb, err := assets.ReadFile("assets/Inter-SemiBold.ttf")
		if err != nil {
			fontsErr = fmt.Errorf("ogrender: read Inter-SemiBold: %w", err)
			return
		}
		if regular, err = opentype.Parse(rb); err != nil {
			fontsErr = fmt.Errorf("ogrender: parse Inter-Regular: %w", err)
			return
		}
		if semibold, err = opentype.Parse(sb); err != nil {
			fontsErr = fmt.Errorf("ogrender: parse Inter-SemiBold: %w", err)
		}
	})
}

// The canvas and the layout constants (plan part 4): margin 60, title
// SemiBold 64, the state sentence Regular 34 wrapped to two lines, one bar
// strip per component (24 squares of 18 px), "checked N min ago" and the
// credit line Regular 28.
const (
	W      = 1200
	H      = 630
	margin = 60
)

// The achromatic palette. Background near-white, text near-black, ok bars
// dark, trouble mid grey, no data light: the page's own state sentence
// carries the verdict, the image never paints a status color.
var (
	colBG     = color.RGBA{R: 0xfa, G: 0xfa, B: 0xfa, A: 0xff}
	colText   = color.RGBA{R: 0x11, G: 0x11, B: 0x11, A: 0xff}
	colDown   = color.RGBA{R: 0x99, G: 0x99, B: 0x99, A: 0xff}
	colNoData = color.RGBA{R: 0xdd, G: 0xdd, B: 0xdd, A: 0xff}
)

// Component is one measured strip: the component's name and its bars, each
// bar one of "ok", "check", "down", "nodata" (the same vocabulary the JSON
// components carry).
type Component struct {
	Name string
	Bars []string
}

// Page is what the renderer needs from the shared status assembly. An empty
// CheckedLine means nothing has been measured yet: the image degrades to
// the title and a centered "no data yet".
type Page struct {
	Host          string
	StateSentence string
	CheckedLine   string
	Components    []Component
}

// Render draws the page and encodes it as PNG.
func Render(p Page) ([]byte, error) {
	loadFonts()
	if fontsErr != nil {
		return nil, fontsErr
	}
	face := func(f *opentype.Font, size float64) (font.Face, error) {
		return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	titleFace, err := face(semibold, 64)
	if err != nil {
		return nil, err
	}
	defer titleFace.Close()
	bodyFace, err := face(regular, 34)
	if err != nil {
		return nil, err
	}
	defer bodyFace.Close()
	smallFace, err := face(regular, 28)
	if err != nil {
		return nil, err
	}
	defer smallFace.Close()
	labelFace, err := face(regular, 24)
	if err != nil {
		return nil, err
	}
	defer labelFace.Close()

	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{colBG}, image.Point{}, draw.Src)

	title := p.Host + " status"
	drawText(img, titleFace, colText, margin, 120, title)

	// No data yet: title plus the honest marker, centered, nothing else.
	if p.CheckedLine == "" {
		drawText(img, bodyFace, colText, (W-width(bodyFace, "no data yet"))/2, H/2, "no data yet")
		return encode(img)
	}

	// The state sentence, wrapped to at most two lines under the title.
	lines := wrap(bodyFace, p.StateSentence, W-2*margin, 2)
	for i, line := range lines {
		drawText(img, bodyFace, colText, margin, 196+i*46, line)
	}

	// One bar strip per component, at most the first four (the OG image is a
	// summary; the page carries the whole list).
	y := 280
	for _, c := range p.Components[:min(len(p.Components), 4)] {
		drawText(img, labelFace, colText, margin, y, c.Name)
		x := margin
		for i := 0; i < 24; i++ {
			var col color.RGBA = colNoData
			if i < len(c.Bars) {
				switch c.Bars[i] {
				case "ok", "check":
					col = colText
				case "down":
					col = colDown
				}
			}
			draw.Draw(img, image.Rect(x, y+14, x+18, y+32), &image.Uniform{col}, image.Point{}, draw.Src)
			x += 20
		}
		y += 66
	}

	drawText(img, smallFace, colText, margin, 580, p.CheckedLine)
	credit := "Powered by UpControl"
	drawText(img, smallFace, colText, W-margin-width(smallFace, credit), 580, credit)
	return encode(img)
}

// drawText writes one line with its baseline at y.
func drawText(img *image.RGBA, f font.Face, c color.RGBA, x, y int, s string) {
	d := &font.Drawer{
		Dst: img, Src: &image.Uniform{c}, Face: f,
		Dot: fixed.P(x, y),
	}
	d.DrawString(s)
}

// width measures a string in pixels on one face.
func width(f font.Face, s string) int {
	d := &font.Drawer{Face: f}
	return d.MeasureString(s).Ceil()
}

// wrap breaks s into at most maxLines lines of at most maxWidth pixels,
// splitting on spaces; the last line is ellipsised when it still overflows.
func wrap(f font.Face, s string, maxWidth, maxLines int) []string {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, w := range words {
		candidate := w
		if cur != "" {
			candidate = cur + " " + w
		}
		if width(f, candidate) <= maxWidth || cur == "" {
			cur = candidate
			continue
		}
		lines = append(lines, cur)
		cur = w
		if len(lines) == maxLines-1 {
			break
		}
	}
	if cur != "" && len(lines) < maxLines {
		lines = append(lines, cur)
	}
	// Whatever did not fit is dropped, and the final line is cut to fit with
	// an ellipsis: a sentence that runs off the canvas is worse than a
	// shortened one.
	last := len(lines) - 1
	if last >= 0 {
		for width(f, lines[last]) > maxWidth && len(lines[last]) > 1 {
			lines[last] = lines[last][:len(lines[last])-1]
		}
		if width(f, lines[last]+"...") <= maxWidth {
			lines[last] += "..."
		}
	}
	return lines
}

func encode(img *image.RGBA) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("ogrender: encode png: %w", err)
	}
	return buf.Bytes(), nil
}
