package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

// sof builds a minimal JPEG header with the given luma sampling factors.
func sof(h, v byte, ncomp byte) []byte {
	d := []byte{0xFF, 0xD8}
	// A quantisation table first, so the parser has to walk segments rather
	// than assume SOF is at a fixed offset.
	d = append(d, 0xFF, 0xDB, 0x00, 0x05, 0x00, 0x01, 0x02)
	body := []byte{0x08, 0x04, 0x38, 0x07, 0x80, ncomp}
	for i := byte(0); i < ncomp; i++ {
		s := byte(0x11)
		if i == 0 {
			s = h<<4 | v
		}
		body = append(body, i+1, s, 0x00)
	}
	seg := len(body) + 2
	d = append(d, 0xFF, 0xC0, byte(seg>>8), byte(seg))
	d = append(d, body...)
	d = append(d, 0xFF, 0xDA, 0x00, 0x02)
	return d
}

func TestJPEGChroma(t *testing.T) {
	for _, c := range []struct {
		name string
		h, v byte
		n    byte
		want Chroma
	}{
		{"4:2:0", 2, 2, 3, Chroma420},
		{"4:2:2 (the ATEM)", 2, 1, 3, Chroma422},
		{"4:4:4", 1, 1, 3, Chroma444},
		{"4:4:0", 1, 2, 3, Chroma440},
		{"grayscale", 1, 1, 1, ChromaGrey},
	} {
		got, err := jpegChroma(sof(c.h, c.v, c.n))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// Only 4:2:0 may take the hardware path; the rest must fall back or they crash
// the process.
func TestHardwareDecodable(t *testing.T) {
	if !Chroma420.HardwareDecodable() {
		t.Error("4:2:0 should use the VPU")
	}
	for _, c := range []Chroma{Chroma422, Chroma444, Chroma440, ChromaGrey, ChromaUnknown} {
		if c.HardwareDecodable() {
			t.Errorf("%s must not use the VPU — mppjpegdec aborts on it", c)
		}
	}
}

// Real encoder output, as a check that the segment walk survives actual tables.
func TestJPEGChromaOnRealEncoderOutput(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewYCbCr(image.Rect(0, 0, 64, 64), image.YCbCrSubsampleRatio420)
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	got, err := jpegChroma(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got != Chroma420 {
		t.Errorf("Go's encoder emits 4:2:0 by default, got %s", got)
	}
}

// Trailing padding after EOI must not confuse it — the ATEM pads every frame to
// a fixed 142144 bytes.
func TestJPEGChromaIgnoresTrailingPadding(t *testing.T) {
	d := append(sof(2, 1, 3), bytes.Repeat([]byte{0xFF}, 47)...)
	got, err := jpegChroma(d)
	if err != nil || got != Chroma422 {
		t.Errorf("got %s, %v; want 4:2:2 despite padding", got, err)
	}
}

func TestJPEGChromaRejectsRubbish(t *testing.T) {
	for _, d := range [][]byte{nil, {0x00}, {0xFF, 0xD8}, bytes.Repeat([]byte{0x41}, 64)} {
		if _, err := jpegChroma(d); err == nil {
			t.Errorf("expected an error for %d bytes of non-JPEG", len(d))
		}
	}
}
