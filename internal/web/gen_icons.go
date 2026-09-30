//go:build ignore

// gen_icons draws the dashboard's favicon and app icons into static/ from
// one pixel map. Run `go generate ./internal/web` (or `make icons`) after
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
	for name, b := range files {
		if err := os.WriteFile(filepath.Join("static", name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
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
