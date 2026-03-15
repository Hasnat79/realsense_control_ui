//go:build !demo

package main

import (
    "bytes"
    "image"
    "image/color"
    "image/jpeg"
)

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
            img.SetRGBA(x, y, color.RGBA{R: bgr[i+2], G: bgr[i+1], B: bgr[i], A: 255})
        }
    }
    var buf bytes.Buffer
    _ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
    return buf.Bytes()
}