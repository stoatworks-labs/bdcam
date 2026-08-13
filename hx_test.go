package main

import (
	"testing"
	"unsafe"
)

// The Advanced SDK's layout, reconstructed. If any of this is wrong the stream
// is silently unreadable, so state the expectation explicitly.
func TestCompressedPacketLayout(t *testing.T) {
	var p ndiCompressedPacket
	if got := unsafe.Sizeof(p); got != 48 {
		t.Errorf("NDIlib_compressed_packet_t is %d bytes, expected 48", got)
	}
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"version", unsafe.Offsetof(p.version), 0},
		{"fourCC", unsafe.Offsetof(p.fourCC), 4},
		{"pts", unsafe.Offsetof(p.pts), 8},
		{"dts", unsafe.Offsetof(p.dts), 16},
		{"reserved", unsafe.Offsetof(p.reserved), 24},
		{"flags", unsafe.Offsetof(p.flags), 32},
		{"data_size", unsafe.Offsetof(p.dataSize), 36},
		{"extra_data_size", unsafe.Offsetof(p.extraDataSize), 40},
	} {
		if c.got != c.want {
			t.Errorf("%s at offset %d, expected %d", c.name, c.got, c.want)
		}
	}
}

func TestH264FourCC(t *testing.T) {
	if fourCCName(fourCCH264) != "H264" {
		t.Errorf("H264 fourcc renders as %q", fourCCName(fourCCH264))
	}
	if fourCCName(fourCCHEVC) != "HEVC" {
		t.Errorf("HEVC fourcc renders as %q", fourCCName(fourCCHEVC))
	}
}
