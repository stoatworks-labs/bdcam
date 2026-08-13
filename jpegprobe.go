package main

// Deciding which JPEG decoder a camera needs.
//
// The RK3328's hardware JPEG decoder does not handle 4:2:2 MJPEG. Fed a 4:2:2
// frame, mpph264enc's sibling mppjpegdec aborts the process outright —
// "mpp_buffer_put invalid input: buffer (nil)" and then a heap error — rather
// than failing negotiation politely. The ATEM Mini Extreme ISO this was written
// against emits exactly that: 1080p MJPEG with luma sampling 0x21.
//
// Rather than discover it by crashing, grab one frame before the pipeline
// starts and read the sampling factors out of the JPEG header. 4:2:0 gets the
// hardware path; anything else falls back to software decode, which costs
// roughly a core at 1080p but works.

import (
	"fmt"
)

// Chroma names the subsampling of a JPEG, in the usual J:a:b shorthand.
type Chroma string

const (
	Chroma420     Chroma = "4:2:0"
	Chroma422     Chroma = "4:2:2"
	Chroma444     Chroma = "4:4:4"
	Chroma440     Chroma = "4:4:0"
	ChromaGrey    Chroma = "grayscale"
	ChromaUnknown Chroma = "unknown"
)

// HardwareDecodable reports whether the VPU's JPEG decoder can be trusted with
// this format. Only 4:2:0 is known good on this silicon.
func (c Chroma) HardwareDecodable() bool { return c == Chroma420 }

// jpegChroma reads the sampling factors from the frame header. It walks the
// marker segments rather than guessing at offsets, because the header length
// varies with the quantisation and Huffman tables the camera chooses.
func jpegChroma(d []byte) (Chroma, error) {
	if len(d) < 4 || d[0] != 0xFF || d[1] != 0xD8 {
		return ChromaUnknown, fmt.Errorf("not a JPEG (no SOI)")
	}
	i := 2
	for i+3 < len(d) {
		if d[i] != 0xFF {
			i++
			continue
		}
		m := d[i+1]
		// Standalone markers carry no length.
		if m == 0xD8 || m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			i += 2
			continue
		}
		if m == 0xD9 {
			break
		}
		if i+3 >= len(d) {
			break
		}
		seg := int(d[i+2])<<8 | int(d[i+3])
		// SOF0/1/2 (baseline, extended, progressive) carry the frame header.
		if m == 0xC0 || m == 0xC1 || m == 0xC2 {
			if i+9 >= len(d) {
				return ChromaUnknown, fmt.Errorf("truncated SOF segment")
			}
			nc := int(d[i+9])
			if nc == 1 {
				return ChromaGrey, nil
			}
			if nc < 1 || i+10+nc*3 > len(d) {
				return ChromaUnknown, fmt.Errorf("truncated component list")
			}
			// The luma component's sampling factors set the subsampling; the
			// chroma components are 1x1 in every format we care about.
			h := int(d[i+11] >> 4)
			v := int(d[i+11] & 0x0F)
			switch {
			case h == 2 && v == 2:
				return Chroma420, nil
			case h == 2 && v == 1:
				return Chroma422, nil
			case h == 1 && v == 1:
				return Chroma444, nil
			case h == 1 && v == 2:
				return Chroma440, nil
			}
			return ChromaUnknown, fmt.Errorf("unrecognised sampling factors %dx%d", h, v)
		}
		if m == 0xDA { // start of scan: the header is behind us
			break
		}
		if seg < 2 {
			return ChromaUnknown, fmt.Errorf("bad segment length %d", seg)
		}
		i += 2 + seg
	}
	return ChromaUnknown, fmt.Errorf("no SOF marker found")
}

// probeCameraChroma grabs a single MJPEG frame and reports its subsampling. The
// device is opened and closed again before the pipeline runs, so nothing is
// left holding it.
func probeCameraChroma(dev string, width, height, fps int) (Chroma, error) {
	c, err := OpenCapture(dev, width, height, pixMJPG, fps, 2)
	if err != nil {
		return ChromaUnknown, err
	}
	defer c.Close()
	if err := c.Start(); err != nil {
		return ChromaUnknown, err
	}
	var chroma Chroma
	var perr error
	// The first buffer off a freshly started stream is occasionally short;
	// a couple of attempts costs nothing.
	for attempt := 0; attempt < 3; attempt++ {
		err = c.Grab(func(data []byte, seq uint32) error {
			chroma, perr = jpegChroma(data)
			return nil
		})
		if err != nil {
			return ChromaUnknown, err
		}
		if perr == nil {
			return chroma, nil
		}
	}
	return ChromaUnknown, perr
}
