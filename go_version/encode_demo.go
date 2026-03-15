//go:build demo

// encode_demo.go — pure-Go JPEG encoder used in -tags demo builds.
// In demo mode, mock frames are already encoded as JPEG by mock.go,
// so these functions are only called if somehow a raw BGR slice slips through.

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
)

// encodeJPEG encodes a raw BGR byte slice to JPEG.
// Assumes 640×480 if dimensions aren't specified.
func encodeJPEG(bgr []byte, quality int) []byte {
	return encodeJPEGSized(bgr, 640, 480, quality)
}

func encodeJPEGSized(bgr []byte, width, height, quality int) []byte {
	if len(bgr) < width*height*3 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 3
			// BGR → RGB
			img.SetRGBA(x, y, color.RGBA{R: bgr[i+2], G: bgr[i+1], B: bgr[i], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil
	}
	return buf.Bytes()
}
