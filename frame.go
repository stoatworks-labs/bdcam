package main

// Frame conversion into something libndi will take, plus a synthetic source.
//
// The ordering here is deliberate. libndi's SpeedHQ encoder is the expensive
// part of this program on four A53s, and anything we do before it comes out of
// the same budget — so the fast paths hand libndi the driver's own mmap'd
// buffer with no copy at all, and only MJPEG pays for a decode.

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

type Frame struct {
	Width, Height int
	FourCC        uint32
	Stride        int
	Data          []byte
}

// Converter turns one captured buffer into one NDI frame. It owns its scratch
// buffers so the steady state allocates nothing.
type Converter struct {
	w, h    int
	srcFmt  uint32
	scratch []byte
	jpegBuf bytes.Reader
	frame   Frame
}

func NewConverter(w, h int, srcFmt uint32) (*Converter, error) {
	c := &Converter{w: w, h: h, srcFmt: srcFmt}
	switch srcFmt {
	case pixUYVY, pixNV12:
		// zero copy — no scratch needed
	case pixYUYV, pixMJPG:
		c.scratch = make([]byte, w*h*2)
	default:
		return nil, fmt.Errorf("no conversion from %s to an NDI format", fourCCName(srcFmt))
	}
	return c, nil
}

// Describe names the path taken, for the startup log.
func (c *Converter) Describe() string {
	switch c.srcFmt {
	case pixUYVY:
		return "UYVY -> UYVY (zero copy)"
	case pixNV12:
		return "NV12 -> NV12 (zero copy)"
	case pixYUYV:
		return "YUYV -> UYVY (byte swap)"
	case pixMJPG:
		return "MJPEG -> UYVY (software JPEG decode — see README)"
	}
	return "unknown"
}

func (c *Converter) Convert(src []byte, stride int) (*Frame, error) {
	switch c.srcFmt {
	case pixUYVY:
		c.frame = Frame{Width: c.w, Height: c.h, FourCC: fourCCUYVY, Stride: c.w * 2, Data: src}
		if stride > 0 {
			c.frame.Stride = stride
		}
		return &c.frame, nil

	case pixNV12:
		// NDI's NV12 is Y plane then interleaved CbCr, which is exactly what
		// the driver produced. Stride is the luma stride.
		c.frame = Frame{Width: c.w, Height: c.h, FourCC: fourCCNV12, Stride: c.w, Data: src}
		if stride > 0 {
			c.frame.Stride = stride
		}
		return &c.frame, nil

	case pixYUYV:
		yuyvToUYVY(src, c.scratch)
		c.frame = Frame{Width: c.w, Height: c.h, FourCC: fourCCUYVY, Stride: c.w * 2, Data: c.scratch}
		return &c.frame, nil

	case pixMJPG:
		c.jpegBuf.Reset(src)
		img, err := jpeg.Decode(&c.jpegBuf)
		if err != nil {
			return nil, fmt.Errorf("jpeg decode: %w", err)
		}
		yc, ok := img.(*image.YCbCr)
		if !ok {
			return nil, fmt.Errorf("jpeg decoded to %T, expected YCbCr", img)
		}
		if err := ycbcrToUYVY(yc, c.scratch, c.w, c.h); err != nil {
			return nil, err
		}
		c.frame = Frame{Width: c.w, Height: c.h, FourCC: fourCCUYVY, Stride: c.w * 2, Data: c.scratch}
		return &c.frame, nil
	}
	return nil, fmt.Errorf("unreachable")
}

// YUYV is Y0 U0 Y1 V0; UYVY is U0 Y0 V0 Y1. Same bytes, swapped in pairs.
func yuyvToUYVY(src, dst []byte) {
	n := len(src)
	if len(dst) < n {
		n = len(dst)
	}
	n &^= 3
	for i := 0; i < n; i += 4 {
		dst[i+0] = src[i+1]
		dst[i+1] = src[i+0]
		dst[i+2] = src[i+3]
		dst[i+3] = src[i+2]
	}
}

func chromaDivisors(r image.YCbCrSubsampleRatio) (hx, vy int) {
	switch r {
	case image.YCbCrSubsampleRatio444:
		return 1, 1
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	}
	return 2, 2
}

func ycbcrToUYVY(img *image.YCbCr, dst []byte, w, h int) error {
	if img.Rect.Dx() < w || img.Rect.Dy() < h {
		return fmt.Errorf("jpeg is %dx%d, expected at least %dx%d",
			img.Rect.Dx(), img.Rect.Dy(), w, h)
	}
	hx, vy := chromaDivisors(img.SubsampleRatio)
	stride := w * 2
	pw := w &^ 1 // UYVY works in pixel pairs
	for y := 0; y < h; y++ {
		yrow := img.Y[y*img.YStride:]
		crow := (y / vy) * img.CStride
		drow := dst[y*stride:]
		for x := 0; x < pw; x += 2 {
			cx := crow + x/hx
			drow[x*2+0] = img.Cb[cx]
			drow[x*2+1] = yrow[x]
			drow[x*2+2] = img.Cr[cx]
			drow[x*2+3] = yrow[x+1]
		}
	}
	return nil
}

// ---------------------------------------------------------------- synthetic

// SyntheticSource generates UYVY colour bars with a bar that steps across the
// frame, so a receiver can tell "still sending" from "frozen on the last
// frame" at a glance. It exists so the 30-minute question and the SpeedHQ
// throughput ceiling can both be measured with no camera attached.
type SyntheticSource struct {
	w, h  int
	base  []byte
	buf   []byte
	frame Frame
	n     int
}

func NewSyntheticSource(w, h int) *SyntheticSource {
	s := &SyntheticSource{w: w, h: h}
	s.base = make([]byte, w*h*2)
	s.buf = make([]byte, w*h*2)

	// 75% colour bars, YUV: white, yellow, cyan, green, magenta, red, blue, black
	bars := [8][3]byte{
		{235, 128, 128}, {210, 16, 146}, {170, 166, 16}, {145, 54, 34},
		{106, 202, 222}, {81, 90, 240}, {41, 240, 110}, {16, 128, 128},
	}
	row := make([]byte, w*2)
	for x := 0; x < w; x += 2 {
		b := bars[(x*8)/w]
		row[x*2+0] = b[1] // U
		row[x*2+1] = b[0] // Y
		row[x*2+2] = b[2] // V
		row[x*2+3] = b[0] // Y
	}
	for y := 0; y < h; y++ {
		copy(s.base[y*w*2:], row)
	}
	return s
}

func (s *SyntheticSource) Next() *Frame {
	copy(s.buf, s.base)
	// A 32-pixel white bar that advances one step per frame and wraps.
	steps := s.w / 32
	if steps < 1 {
		steps = 1
	}
	x0 := (s.n % steps) * 32
	for y := 0; y < s.h; y++ {
		r := s.buf[y*s.w*2:]
		// Per pixel, byte 0 is U on even x and V on odd x — both 128 for
		// neutral chroma — and byte 1 is that pixel's luma.
		for x := x0; x < x0+32 && x < s.w; x++ {
			r[x*2+0] = 128
			r[x*2+1] = 235
		}
	}
	s.n++
	s.frame = Frame{Width: s.w, Height: s.h, FourCC: fourCCUYVY, Stride: s.w * 2, Data: s.buf}
	return &s.frame
}
