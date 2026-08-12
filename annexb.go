package main

// Splitting an Annex-B byte stream into access units.
//
// h264parse hands us a byte stream over a pipe, which carries no framing, so
// the picture boundaries have to be recovered here. The VEPU emits an access
// unit delimiter (NAL type 9) at the head of every picture — visible as
// 00 00 00 01 09 at the start of any file it produces — so an AUD is the
// boundary. Keyframes are recognised by an IDR slice (type 5), which is also
// where h264parse has been asked to repeat SPS/PPS.

import (
	"fmt"
	"io"
)

const (
	nalTypeIDR = 5
	nalTypeSPS = 7
	nalTypeAUD = 9

	// A 1080p intra frame at a sane bitrate is far below this. The cap exists
	// so a desynchronised stream fails loudly instead of eating memory.
	maxAUBytes = 4 << 20
)

// AUScanner reads Annex-B and calls fn once per access unit.
type AUScanner struct {
	r   io.Reader
	buf []byte
	in  []byte
}

func NewAUScanner(r io.Reader) *AUScanner {
	return &AUScanner{r: r, in: make([]byte, 64<<10)}
}

// Scan blocks until the reader ends. fn receives a slice valid only for the
// duration of the call.
func (s *AUScanner) Scan(fn func(au []byte, keyframe bool) error) error {
	for {
		n, err := s.r.Read(s.in)
		if n > 0 {
			s.buf = append(s.buf, s.in[:n]...)
			if len(s.buf) > maxAUBytes {
				return fmt.Errorf("no access unit boundary within %d bytes — stream desynchronised", maxAUBytes)
			}
			if e := s.drain(fn, false); e != nil {
				return e
			}
		}
		if err != nil {
			if err == io.EOF {
				// flush whatever trailing picture is buffered
				return s.drain(fn, true)
			}
			return err
		}
	}
}

// drain emits every complete access unit in the buffer. A unit is complete
// once the *next* AUD has been seen, so the last one is held back until EOF.
func (s *AUScanner) drain(fn func(au []byte, keyframe bool) error, atEOF bool) error {
	for {
		start := findAUD(s.buf, 0)
		if start < 0 {
			return nil
		}
		next := findAUD(s.buf, start+4)
		if next < 0 {
			if !atEOF {
				return nil
			}
			next = len(s.buf)
		}
		au := s.buf[start:next]
		if len(au) > 0 {
			if err := fn(au, hasIDR(au)); err != nil {
				return err
			}
		}
		s.buf = append(s.buf[:0], s.buf[next:]...)
		if atEOF && findAUD(s.buf, 0) < 0 {
			return nil
		}
	}
}

// findAUD returns the index of the start code introducing the next access unit
// delimiter, or -1. The returned index points at the start code so the emitted
// unit is a well-formed Annex-B stream on its own.
func findAUD(b []byte, from int) int {
	for i := from; i+4 < len(b); i++ {
		if b[i] != 0x00 || b[i+1] != 0x00 {
			continue
		}
		switch {
		case b[i+2] == 0x01:
			if b[i+3]&0x1F == nalTypeAUD {
				return i
			}
		case b[i+2] == 0x00 && b[i+3] == 0x01:
			if i+4 < len(b) && b[i+4]&0x1F == nalTypeAUD {
				return i
			}
		}
	}
	return -1
}

// hasIDR reports whether the unit contains an IDR slice, i.e. whether a
// receiver can start decoding here. SPS alone is treated as a keyframe marker
// too, since h264parse config-interval=1 puts it only on random access points.
func hasIDR(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] != 0x00 || au[i+1] != 0x00 {
			continue
		}
		var t byte
		switch {
		case au[i+2] == 0x01:
			t = au[i+3] & 0x1F
		case au[i+2] == 0x00 && au[i+3] == 0x01 && i+4 < len(au):
			t = au[i+4] & 0x1F
		default:
			continue
		}
		if t == nalTypeIDR || t == nalTypeSPS {
			return true
		}
	}
	return false
}
