// mock.go — demo camera: animated BGR frames, pure Go, zero dependencies.
// Used by both demo and real builds (startMockPreview lives in main.go).

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"time"
)

// mockPreviewWorker generates animated 640×480 JPEG frames at ~15 fps.
func mockPreviewWorker(serial string, stop <-chan struct{}) {
	fs := ensureFrameSlot(serial)

	hueBase := serialHue(serial)
	ticker := time.NewTicker(time.Second / 15)
	defer ticker.Stop()

	for frameN := 0; ; frameN++ {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if jpeg := renderMockFrame(serial, hueBase, frameN); jpeg != nil {
			fs.push(jpeg)
		}
	}
}

func serialHue(serial string) int {
	h := 0
	for _, c := range serial {
		h = (h*31 + int(c)) % 360
	}
	return h
}

func renderMockFrame(serial string, hueBase, frameN int) []byte {
	const W, H = 640, 480
	img := image.NewRGBA(image.Rect(0, 0, W, H))

	shift := (frameN * 3) % 360
	for x := 0; x < W; x++ {
		hue := float64((hueBase + shift + x/3) % 360)
		val := 0.4 + 0.5*(float64(x)/float64(W))
		r, g, b := hsvToRGB(hue, 0.85, val)
		col := color.RGBA{R: r, G: g, B: b, A: 255}
		for y := 0; y < H; y++ {
			img.SetRGBA(x, y, col)
		}
	}

	tag := serial
	if len(tag) > 4 {
		tag = tag[len(tag)-4:]
	}
	blit(img, fmt.Sprintf("DEMO CAM %s", tag), 20, 36, color.RGBA{255, 255, 255, 255})
	blit(img, time.Now().Format("15:04:05.000"), 20, 60, color.RGBA{200, 200, 200, 255})

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil
	}
	return buf.Bytes()
}

func hsvToRGB(h, s, v float64) (uint8, uint8, uint8) {
	h = math.Mod(h, 360)
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return uint8((r + m) * 255), uint8((g + m) * 255), uint8((b + m) * 255)
}

// blit renders text onto img using a tiny embedded bitmap font.
func blit(img *image.RGBA, text string, ox, oy int, col color.RGBA) {
	for ci, ch := range text {
		glyph, ok := glyphs[ch]
		if !ok {
			continue
		}
		bx := ox + ci*6
		for row, bits := range glyph {
			for col2 := 0; col2 < 5; col2++ {
				if bits&(1<<(4-col2)) != 0 {
					img.SetRGBA(bx+col2, oy+row, col)
				}
			}
		}
	}
}

// glyphs: each character is 7 rows × 5 cols packed into a uint8 bitmask.
// Bit 4 = leftmost pixel, bit 0 = rightmost pixel.
var glyphs = map[rune][7]uint8{
	' ': {},
	':': {0, 0b00100, 0, 0, 0b00100, 0, 0},
	'.': {0, 0, 0, 0, 0, 0b00100, 0},
	'-': {0, 0, 0b01110, 0, 0, 0, 0},
	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00110, 0b01000, 0b10000, 0b11111},
	'3': {0b11110, 0b00001, 0b00001, 0b01110, 0b00001, 0b00001, 0b11110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b01110, 0b10000, 0b11110, 0b10001, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00001, 0b01110},
	'A': {0b01110, 0b10001, 0b10001, 0b11111, 0b10001, 0b10001, 0b10001},
	'B': {0b11110, 0b10001, 0b10001, 0b11110, 0b10001, 0b10001, 0b11110},
	'C': {0b01110, 0b10001, 0b10000, 0b10000, 0b10000, 0b10001, 0b01110},
	'D': {0b11100, 0b10010, 0b10001, 0b10001, 0b10001, 0b10010, 0b11100},
	'E': {0b11111, 0b10000, 0b10000, 0b11110, 0b10000, 0b10000, 0b11111},
	'M': {0b10001, 0b11011, 0b10101, 0b10001, 0b10001, 0b10001, 0b10001},
	'O': {0b01110, 0b10001, 0b10001, 0b10001, 0b10001, 0b10001, 0b01110},
}
