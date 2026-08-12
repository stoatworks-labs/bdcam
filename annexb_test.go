package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// nal builds a start-code-prefixed NAL of the given type.
func nal(t byte, payload ...byte) []byte {
	return append([]byte{0x00, 0x00, 0x00, 0x01, t & 0x1F}, payload...)
}

func TestScannerSplitsOnAUD(t *testing.T) {
	var s []byte
	// three pictures: IDR, then two non-IDR
	s = append(s, nal(nalTypeAUD)...)
	s = append(s, nal(nalTypeSPS, 0x11)...)
	s = append(s, nal(nalTypeIDR, 0x22)...)
	s = append(s, nal(nalTypeAUD)...)
	s = append(s, nal(1, 0x33)...)
	s = append(s, nal(nalTypeAUD)...)
	s = append(s, nal(1, 0x44)...)

	var got []bool
	sc := NewAUScanner(bytes.NewReader(s))
	if err := sc.Scan(func(au []byte, key bool) error {
		got = append(got, key)
		if au[0] != 0 || au[1] != 0 {
			t.Errorf("unit does not begin with a start code: % X", au[:4])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d access units, want 3", len(got))
	}
	if !got[0] {
		t.Error("first unit should be a keyframe (contains SPS + IDR)")
	}
	if got[1] || got[2] {
		t.Error("later units are not keyframes")
	}
}

// The scanner must not depend on reads landing on unit boundaries — over a
// pipe they never do.
func TestScannerHandlesSplitReads(t *testing.T) {
	var s []byte
	for i := 0; i < 5; i++ {
		s = append(s, nal(nalTypeAUD)...)
		s = append(s, nal(1, byte(i))...)
	}
	for _, chunk := range []int{1, 3, 7, 64} {
		n := 0
		sc := NewAUScanner(&chunkReader{data: s, chunk: chunk})
		if err := sc.Scan(func(au []byte, key bool) error { n++; return nil }); err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		if n != 5 {
			t.Errorf("chunk %d: got %d units, want 5", chunk, n)
		}
	}
}

func TestScannerRejectsDesync(t *testing.T) {
	// No AUD anywhere, more than the cap — should fail rather than grow.
	junk := bytes.Repeat([]byte{0x55}, maxAUBytes+1024)
	sc := NewAUScanner(bytes.NewReader(junk))
	err := sc.Scan(func(au []byte, key bool) error { return nil })
	if err == nil {
		t.Fatal("expected a desynchronisation error")
	}
	if !strings.Contains(err.Error(), "desynchronised") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestThreeByteStartCodes(t *testing.T) {
	// h264parse may emit 3-byte start codes; both forms must be recognised.
	s := []byte{0x00, 0x00, 0x01, nalTypeAUD}
	s = append(s, 0x00, 0x00, 0x01, nalTypeIDR, 0x01)
	s = append(s, 0x00, 0x00, 0x01, nalTypeAUD)
	s = append(s, 0x00, 0x00, 0x01, 0x01, 0x02)
	n := 0
	sc := NewAUScanner(bytes.NewReader(s))
	if err := sc.Scan(func(au []byte, key bool) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("got %d units from 3-byte start codes, want 2", n)
	}
}

// chunkReader hands back data in fixed-size pieces, so the scanner is exercised
// against reads that land mid-NAL — which over a pipe is the normal case.
type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}
