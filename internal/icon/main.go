// SPDX-License-Identifier: MIT

// Command icon draws the program's icon and writes the two files that carry it.
//
// The picture is a magnifier: a blue ring, white glass, a handle running to the
// bottom right. It is drawn here rather than kept as a file somebody once
// exported, because there are two files of it and they have to be the same
// picture — the notification area is handed one image at runtime, and Explorer
// reads a set of sizes out of the executable. Two exports of "roughly the same
// icon" drift, and the drift is only ever noticed on somebody else's machine.
//
// Run it from the root of the repository:
//
//	go run ./internal/icon
//
// The executable's icon reaches the binary through rsrc_windows_*.syso beside
// the command, which "go build" picks up on its own — building this program is
// still "go build" and nothing else. Those two files are generated from
// gserp.ico, and generating them again is the one step that needs a tool:
//
//	cd cmd/gserp
//	go run github.com/tc-hib/go-winres@latest simply --icon gserp.ico --arch amd64,arm64
//
// That is only needed when the drawing below changes.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// The two colours, and nothing else. The blue is the one the interface uses for
// anything it wants read first; the glass is white so the icon reads at sixteen
// pixels, where a shaded one turns to mud.
var (
	blue  = color.NRGBA{R: 26, G: 115, B: 232, A: 255}
	glass = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
)

// over is how much larger the drawing is made before it is averaged down. Four
// is enough that a rim looks like a rim rather than a staircase, and cheap
// enough that every size is drawn in a moment.
const over = 4

// sizes are what Windows asks for: the small ones for lists and the tray, the
// large ones for the desktop and the Alt-Tab card.
var sizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dir := filepath.Join("cmd", "gserp")
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("run this from the root of the repository: %w", err)
	}
	// The whole set for the executable, and one image for the notification
	// area: that loader takes the first entry of the file it is given, which is
	// what a file with one entry is for.
	if err := write(filepath.Join(dir, "gserp.ico"), sizes); err != nil {
		return err
	}
	return write(filepath.Join(dir, "tray.ico"), []int{32})
}

// write draws every size and puts them in one .ico file.
func write(path string, at []int) error {
	var entries, blobs bytes.Buffer
	offset := 6 + 16*len(at)
	for _, n := range at {
		blob, err := encode(draw32(n), n)
		if err != nil {
			return err
		}
		// A dimension of 256 is written as nought: the field is one byte.
		side := byte(n)
		if n == 256 {
			side = 0
		}
		for _, v := range []any{side, side, byte(0), byte(0),
			uint16(1), uint16(32), uint32(len(blob)), uint32(offset)} {
			_ = binary.Write(&entries, binary.LittleEndian, v)
		}
		offset += len(blob)
		blobs.Write(blob)
	}

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(at))})
	out.Write(entries.Bytes())
	out.Write(blobs.Bytes())
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s — %d image(s), %d bytes\n", path, len(at), out.Len())
	return nil
}

// encode is one image as an .ico entry carries it: the large ones as PNG, which
// every Windows since Vista reads, and the small ones as the bitmap every
// Windows has always read.
func encode(img *image.NRGBA, n int) ([]byte, error) {
	if n >= 128 {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return bmp(img, n), nil
}

// bmp is the 32-bit bitmap an .ico entry holds: a header saying the image is
// twice as tall as it is (the colours and then the mask), the rows bottom
// upwards in blue-green-red-alpha, and the mask itself.
func bmp(img *image.NRGBA, n int) []byte {
	var out bytes.Buffer
	for _, v := range []any{
		uint32(40), int32(n), int32(n * 2), uint16(1), uint16(32),
		uint32(0), uint32(0), int32(0), int32(0), uint32(0), uint32(0),
	} {
		_ = binary.Write(&out, binary.LittleEndian, v)
	}
	for y := n - 1; y >= 0; y-- {
		for x := range n {
			c := img.NRGBAAt(x, y)
			out.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	// The mask is a bit per pixel, each row padded to four bytes. Nought means
	// "show the pixel", and the alpha above does the real work; it is here
	// because the format still requires it.
	stride := ((n + 31) / 32) * 4
	out.Write(make([]byte, stride*n))
	return out.Bytes()
}

// draw32 draws the magnifier at one size.
//
// It is drawn large and averaged down rather than drawn with a smoothing
// library: the shape is a circle, a ring and a bar, and averaging is the whole
// of what smoothing them needs.
func draw32(n int) *image.NRGBA {
	m := n * over
	big := image.NewNRGBA(image.Rect(0, 0, m, m))
	draw.Draw(big, big.Bounds(), image.Transparent, image.Point{}, draw.Src)

	f := float64(m)
	cx, cy := 0.415*f, 0.415*f
	outer := 0.315 * f
	inner := outer - math.Max(0.085*f, 1.6*over)
	// The handle leaves the rim along the diagonal and stops short of the
	// corner, so nothing of it is lost when the icon is drawn in a rounded box.
	x0, y0 := cx+outer*0.72, cy+outer*0.72
	x1, y1 := 0.93*f, 0.93*f
	half := math.Max(0.058*f, 1.2*over)

	for y := range m {
		for x := range m {
			px, py := float64(x)+0.5, float64(y)+0.5
			switch d := math.Hypot(px-cx, py-cy); {
			case d <= inner:
				big.SetNRGBA(x, y, glass)
			case d <= outer, toSegment(px, py, x0, y0, x1, y1) <= half:
				big.SetNRGBA(x, y, blue)
			}
		}
	}
	return shrink(big, n)
}

// toSegment is how far a point is from a line with two ends, which is what
// gives the handle its rounded ends for free.
func toSegment(px, py, x0, y0, x1, y1 float64) float64 {
	dx, dy := x1-x0, y1-y0
	t := ((px-x0)*dx + (py-y0)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(px-(x0+t*dx), py-(y0+t*dy))
}

// shrink averages each block down to one pixel.
//
// The colour is averaged over the covered part of the block only, and the cover
// becomes the opacity. Averaged over the whole block instead, a half-covered
// edge would be half black — which is how an icon ends up with a grey halo
// nobody drew.
func shrink(big *image.NRGBA, n int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, n, n))
	for y := range n {
		for x := range n {
			var r, g, b, covered int
			for j := range over {
				for i := range over {
					c := big.NRGBAAt(x*over+i, y*over+j)
					if c.A == 0 {
						continue
					}
					r, g, b, covered = r+int(c.R), g+int(c.G), b+int(c.B), covered+1
				}
			}
			if covered == 0 {
				continue
			}
			out.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r / covered), G: uint8(g / covered), B: uint8(b / covered),
				A: uint8((255*covered + over*over/2) / (over * over)),
			})
		}
	}
	return out
}
