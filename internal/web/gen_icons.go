//go:build ignore

// gen_icons draws the dashboard's favicon, app icons and iOS launch
// screens into static/ from one pixel map, and lists the launch screens in
// index.html. Run `go generate ./internal/web` (or `make icons`) after
// changing it.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// glyph is three agents in a vic formation, the lead in the accent colour,
// each block with the right/bottom shadow of the ASCII logo.
var glyph = []string{
	"....LLL.....",
	"....LLLl....",
	"....LLLl....",
	".....lll....",
	"WWW.....WWW.",
	"WWWs....WWWs",
	"WWWs....WWWs",
	".sss.....sss",
}

// palette maps glyph cells to the dashboard's colours (Tailwind zinc and
// emerald, as in input.css).
var palette = []struct {
	key byte
	hex string
}{
	{'s', "#3f3f46"}, // zinc-700, the logo's shadow
	{'W', "#d4d4d8"}, // zinc-300, the logo's letters
	{'l', "#065f46"}, // emerald-800
	{'L', "#34d399"}, // emerald-400, the accent
}

// bg is ink-950, also the page's theme colour.
const bg = "#07090c"

// The favicon is a tile of 16 units with the glyph centred and corners of
// radius 3; the PNG tiles are the same at an integer scale.
const tile, radius = 16, 3

func main() {
	gw, gh := len(glyph[0]), len(glyph)
	files := map[string][]byte{
		"favicon.svg":  svg(gw, gh),
		"favicon.ico":  ico(tiled(1), tiled(2), tiled(3)),
		"icon-192.png": encode(tiled(12)),
		"icon-512.png": encode(tiled(32)),
		// Platforms that mask icons themselves get the background full-bleed,
		// the glyph inside their safe zone (a centred circle of 80% for
		// maskable icons).
		"apple-touch-icon.png":  encode(icon(180, 11, 0)),
		"icon-maskable-512.png": encode(icon(512, 26, 0)),
	}
	links := splashes(files)
	if err := os.MkdirAll(filepath.Join("static", "splash"), 0o755); err != nil {
		log.Fatal(err)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join("static", name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	writeLinks(filepath.Join("static", "index.html"), links)
}

func tiled(scale int) *image.NRGBA {
	return icon(tile*scale, scale, float64(radius*scale))
}

// icon draws a size×size icon: the glyph at scale, centred on the
// background, which is a square with corner radius r (0: full-bleed).
func icon(size, scale int, r float64) *image.NRGBA {
	gw, gh := len(glyph[0])*scale, len(glyph)*scale
	if (size-gw)%2 != 0 || (size-gh)%2 != 0 {
		log.Fatalf("glyph at scale %d is not centred on whole pixels in %d", scale, size)
	}
	x0, y0 := (size-gw)/2, (size-gh)/2
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	back := rgb(bg)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := back
			c.A = uint8(math.Round(255 * coverage(x, y, size, r)))
			img.SetNRGBA(x, y, c)
		}
	}
	for _, p := range palette {
		for row, line := range glyph {
			for col := range line {
				if line[col] != p.key {
					continue
				}
				cell := image.Rect(col*scale, row*scale, (col+1)*scale, (row+1)*scale).Add(image.Pt(x0, y0))
				draw.Draw(img, cell, image.NewUniform(rgb(p.hex)), image.Point{}, draw.Src)
			}
		}
	}
	return img
}

// coverage is the share of pixel (x, y) inside the size×size square with
// corner radius r, sampled 8×8.
func coverage(x, y, size int, r float64) float64 {
	if r == 0 {
		return 1
	}
	const n = 8
	in := 0
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			sx := float64(x) + (float64(i)+0.5)/n
			sy := float64(y) + (float64(j)+0.5)/n
			// The nearest point of the square shrunk by r.
			cx := math.Min(math.Max(sx, r), float64(size)-r)
			cy := math.Min(math.Max(sy, r), float64(size)-r)
			if (sx-cx)*(sx-cx)+(sy-cy)*(sy-cy) <= r*r {
				in++
			}
		}
	}
	return float64(in) / (n * n)
}

// svg is the favicon as vector: one path per colour, a subpath per run of
// cells in a row.
func svg(gw, gh int) []byte {
	x0, y0 := (tile-gw)/2, (tile-gh)/2
	var b strings.Builder
	fmt.Fprintf(&b, "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 %d %d\">\n", tile, tile)
	fmt.Fprintf(&b, "<rect width=\"%d\" height=\"%d\" rx=\"%d\" fill=\"%s\"/>\n", tile, tile, radius, bg)
	for _, p := range palette {
		var d strings.Builder
		for row, line := range glyph {
			for col := 0; col < len(line); {
				end := col
				for end < len(line) && line[end] == p.key {
					end++
				}
				if end > col {
					fmt.Fprintf(&d, "M%d %dh%dv1h-%dz", x0+col, y0+row, end-col, end-col)
					col = end
				} else {
					col++
				}
			}
		}
		fmt.Fprintf(&b, "<path fill=\"%s\" shape-rendering=\"crispEdges\" d=\"%s\"/>\n", p.hex, d.String())
	}
	b.WriteString("</svg>\n")
	return []byte(b.String())
}

// ico packs PNGs (each under 256 pixels) into an .ico file.
func ico(imgs ...*image.NRGBA) []byte {
	var head, data bytes.Buffer
	binary.Write(&head, binary.LittleEndian, [3]uint16{0, 1, uint16(len(imgs))})
	for _, img := range imgs {
		p, w := encode(img), img.Bounds().Dx()
		binary.Write(&head, binary.LittleEndian, struct {
			W, H, Colors, _ uint8
			Planes, BPP     uint16
			Size, Offset    uint32
		}{uint8(w), uint8(w), 0, 0, 1, 32, uint32(len(p)), uint32(6 + 16*len(imgs) + data.Len())})
		data.Write(p)
	}
	return append(head.Bytes(), data.Bytes()...)
}

func encode(img image.Image) []byte {
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		log.Fatal(err)
	}
	return b.Bytes()
}

func rgb(hex string) color.NRGBA {
	var c color.NRGBA
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &c.R, &c.G, &c.B); err != nil {
		log.Fatal(err)
	}
	c.A = 255
	return c
}

// ------------------------------------------------------------ launch screens

// iOS shows a launch screen while a web app added to the home screen
// loads, but only an image made for the exact screen: one per screen
// size and orientation, picked by the media query of its link.
var screens = []struct{ w, h, dpr int }{ // portrait, in CSS pixels
	{320, 568, 2},   // iPhone SE (1st)
	{375, 667, 2},   // iPhone 6–8, SE (2nd, 3rd)
	{414, 736, 3},   // iPhone 6–8 Plus
	{375, 812, 3},   // iPhone X, XS, 11 Pro, 12 mini, 13 mini
	{414, 896, 2},   // iPhone XR, 11
	{414, 896, 3},   // iPhone XS Max, 11 Pro Max
	{390, 844, 3},   // iPhone 12–14, 16e
	{428, 926, 3},   // iPhone 12 and 13 Pro Max, 14 Plus
	{393, 852, 3},   // iPhone 14 Pro, 15, 15 Pro, 16
	{430, 932, 3},   // iPhone 14 Pro Max, 15 Plus, 15 Pro Max, 16 Plus
	{402, 874, 3},   // iPhone 16 Pro, 17, 17 Pro
	{420, 912, 3},   // iPhone Air
	{440, 956, 3},   // iPhone 16 Pro Max, 17 Pro Max
	{744, 1133, 2},  // iPad mini (6th, 7th)
	{768, 1024, 2},  // iPad (up to 6th), mini (up to 5th), Air (up to 2nd), Pro 9.7"
	{810, 1080, 2},  // iPad (7th–9th)
	{820, 1180, 2},  // iPad (10th, A16), Air (4th, 5th)
	{834, 1112, 2},  // iPad Air (3rd), Pro 10.5"
	{834, 1194, 2},  // iPad Pro 11" (1st–4th), Air 11" (M2, M3)
	{834, 1210, 2},  // iPad Pro 11" (M4, M5)
	{1024, 1366, 2}, // iPad Pro 12.9", Air 13" (M2, M3)
	{1032, 1376, 2}, // iPad Pro 13" (M4, M5)
}

// wordmark is the ASCII logo of the page header: blocks are letters, the
// box-drawing characters their shadow.
var wordmark = []string{
	"███████╗██╗     ███████╗███████╗████████╗",
	"██╔════╝██║     ██╔════╝██╔════╝╚══██╔══╝",
	"█████╗  ██║     █████╗  █████╗     ██║   ",
	"██╔══╝  ██║     ██╔══╝  ██╔══╝     ██║   ",
	"██║     ███████╗███████╗███████╗   ██║   ",
	"╚═╝     ╚══════╝╚══════╝╚══════╝   ╚═╝   ",
}

// splashes adds a launch screen per screen and orientation to files, and
// returns their links.
func splashes(files map[string][]byte) []string {
	var links []string
	for _, sc := range screens {
		for _, o := range []string{"portrait", "landscape"} {
			w, h := sc.w*sc.dpr, sc.h*sc.dpr
			if o == "landscape" {
				w, h = h, w
			}
			name := fmt.Sprintf("splash/%dx%d.png", w, h)
			files[name] = encode(splash(w, h))
			links = append(links, fmt.Sprintf(`<link rel="apple-touch-startup-image" href="/%s" media="(device-width: %dpx) and (device-height: %dpx) and (-webkit-device-pixel-ratio: %d) and (orientation: %s)">`,
				name, sc.w, sc.h, sc.dpr, o))
		}
	}
	return links
}

// splash draws a w×h launch screen: the glyph above the wordmark, centred
// on the page background, sized by the shorter side.
func splash(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(rgb(bg)), image.Point{}, draw.Src)
	short := min(w, h)
	// The glyph a quarter of the short side wide, the wordmark half of it,
	// in cells twice as tall as wide like a terminal's.
	gs := max(1, short/4/len(glyph[0]))
	cw := max(1, short/2/len([]rune(wordmark[0])))
	gw, gh := len(glyph[0])*gs, len(glyph)*gs
	ww, wh := len([]rune(wordmark[0]))*cw, len(wordmark)*2*cw
	gap := gh / 2
	y0 := (h - gh - gap - wh) / 2
	for _, p := range palette {
		for row, line := range glyph {
			for col := range line {
				if line[col] == p.key {
					fill(img, (w-gw)/2+col*gs, y0+row*gs, gs, gs, p.hex)
				}
			}
		}
	}
	x0, y1 := (w-ww)/2, y0+gh+gap
	for row, line := range wordmark {
		for col, r := range []rune(line) {
			x, y := x0+col*cw, y1+row*2*cw
			if r == '█' {
				fill(img, x, y, cw, 2*cw, "#d4d4d8") // zinc-300, as the header's letters
				continue
			}
			// A box-drawing line from the cell's middle to the sides it
			// connects, in the header's shadow colour.
			up, down := strings.ContainsRune("║╚╝", r), strings.ContainsRune("║╔╗", r)
			left, right := strings.ContainsRune("═╗╝", r), strings.ContainsRune("═╔╚", r)
			t := max(1, cw/4)
			mx, my := x+(cw-t)/2, y+cw-t/2
			const shadow = "#3f3f46" // zinc-700
			if up {
				fill(img, mx, y, t, my-y+t, shadow)
			}
			if down {
				fill(img, mx, my, t, y+2*cw-my, shadow)
			}
			if left {
				fill(img, x, my, mx-x+t, t, shadow)
			}
			if right {
				fill(img, mx, my, x+cw-mx, t, shadow)
			}
		}
	}
	return img
}

func fill(img *image.NRGBA, x, y, w, h int, hex string) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(rgb(hex)), image.Point{}, draw.Src)
}

// writeLinks puts the launch screen links between the splash markers of
// the page.
func writeLinks(path string, links []string) {
	const begin, end = "<!-- splash:begin -->\n", "<!-- splash:end -->"
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}
	page := string(b)
	i, j := strings.Index(page, begin), strings.Index(page, end)
	if i < 0 || j < i {
		log.Fatalf("%s: no %q ... %q", path, strings.TrimSpace(begin), end)
	}
	page = page[:i+len(begin)] + strings.Join(links, "\n") + "\n" + page[j:]
	if err := os.WriteFile(path, []byte(page), 0o644); err != nil {
		log.Fatal(err)
	}
}
