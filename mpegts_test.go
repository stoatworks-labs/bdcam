package main

import (
	"bytes"
	"testing"
)

func TestCRC32MPEG(t *testing.T) {
	// The canonical check value for CRC-32/MPEG-2.
	if got := crc32MPEG([]byte("123456789")); got != 0x0376E6E7 {
		t.Errorf("crc32MPEG(123456789) = 0x%08X, want 0x0376E6E7", got)
	}
}

func TestEncodePTS(t *testing.T) {
	// Marker bits: low bit of bytes 0, 2 and 4 must be set, and the top nibble
	// of byte 0 carries the prefix.
	b := encodePTS(0, 0x02)
	if len(b) != 5 {
		t.Fatalf("PTS is %d bytes, want 5", len(b))
	}
	if b[0]>>4 != 0x02 {
		t.Errorf("prefix nibble = 0x%X, want 0x2", b[0]>>4)
	}
	for _, i := range []int{0, 2, 4} {
		if b[i]&0x01 == 0 {
			t.Errorf("marker bit missing in byte %d (0x%02X)", i, b[i])
		}
	}
	// A known value: decode it back out of the marker interleave.
	const pts = int64(90000)
	b = encodePTS(pts, 0x02)
	got := int64(b[0]>>1&0x07)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
	if got != pts {
		t.Errorf("PTS round trip = %d, want %d", got, pts)
	}
}

func muxOne(t *testing.T, au []byte, key bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	if err := m.WriteAccessUnit(au, 90000, key); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTSPacketFraming(t *testing.T) {
	out := muxOne(t, bytes.Repeat([]byte{0xAA}, 500), true)
	if len(out)%tsPacketSize != 0 {
		t.Fatalf("output is %d bytes, not a multiple of 188", len(out))
	}
	for i := 0; i < len(out); i += tsPacketSize {
		if out[i] != tsSync {
			t.Fatalf("packet at %d does not start with 0x47", i)
		}
	}
}

func TestTSCarriesPATAndPMT(t *testing.T) {
	out := muxOne(t, bytes.Repeat([]byte{0xAA}, 200), true)
	seen := map[uint16]bool{}
	for i := 0; i < len(out); i += tsPacketSize {
		pid := uint16(out[i+1]&0x1F)<<8 | uint16(out[i+2])
		seen[pid] = true
	}
	for _, pid := range []uint16{pidPAT, pidPMT, pidVid} {
		if !seen[pid] {
			t.Errorf("no packet on PID 0x%04X", pid)
		}
	}
}

func TestPSISectionCRCsVerify(t *testing.T) {
	out := muxOne(t, []byte{0x01, 0x02, 0x03}, true)
	checked := 0
	for i := 0; i < len(out); i += tsPacketSize {
		pid := uint16(out[i+1]&0x1F)<<8 | uint16(out[i+2])
		if pid != pidPAT && pid != pidPMT {
			continue
		}
		p := out[i : i+tsPacketSize]
		ptr := int(p[4])
		sec := p[5+ptr:]
		length := int(sec[1]&0x0F)<<8 | int(sec[2])
		full := sec[:3+length]
		// The trailing 4 bytes are the CRC over everything before them.
		want := uint32(full[len(full)-4])<<24 | uint32(full[len(full)-3])<<16 |
			uint32(full[len(full)-2])<<8 | uint32(full[len(full)-1])
		if got := crc32MPEG(full[:len(full)-4]); got != want {
			t.Errorf("PID 0x%04X section CRC = 0x%08X, want 0x%08X", pid, got, want)
		}
		checked++
	}
	if checked < 2 {
		t.Fatalf("only checked %d PSI sections, expected PAT and PMT", checked)
	}
}

func TestContinuityCountersIncrement(t *testing.T) {
	var buf bytes.Buffer
	m := NewTSMuxer(&buf)
	for i := 0; i < 4; i++ {
		if err := m.WriteAccessUnit(bytes.Repeat([]byte{0xAA}, 900), int64(i)*3000, i == 0); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.Bytes()
	last := map[uint16]int{}
	for i := 0; i < len(out); i += tsPacketSize {
		pid := uint16(out[i+1]&0x1F)<<8 | uint16(out[i+2])
		cc := int(out[i+3] & 0x0F)
		if prev, ok := last[pid]; ok {
			if want := (prev + 1) & 0x0F; cc != want {
				t.Fatalf("PID 0x%04X continuity jumped %d -> %d", pid, prev, cc)
			}
		}
		last[pid] = cc
	}
}

func TestKeyframeCarriesPCR(t *testing.T) {
	out := muxOne(t, bytes.Repeat([]byte{0xAA}, 300), true)
	found := false
	for i := 0; i < len(out); i += tsPacketSize {
		pid := uint16(out[i+1]&0x1F)<<8 | uint16(out[i+2])
		afc := out[i+3] >> 4 & 0x03
		if pid == pidVid && afc&0x02 != 0 && out[i+4] >= 7 && out[i+5]&0x10 != 0 {
			found = true
		}
	}
	if !found {
		t.Error("no PCR in the adaptation field of a keyframe — receivers cannot lock the clock")
	}
}

func TestPESHeaderShape(t *testing.T) {
	pes := buildPES([]byte{0xDE, 0xAD}, 90000)
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		t.Fatalf("missing PES start code: % X", pes[:3])
	}
	if pes[3] != streamIDVideo {
		t.Errorf("stream_id = 0x%02X, want 0x%02X", pes[3], streamIDVideo)
	}
	if pes[4] != 0 || pes[5] != 0 {
		t.Errorf("PES_packet_length should be 0 (unbounded) for video, got % X", pes[4:6])
	}
	if pes[7] != 0x80 {
		t.Errorf("PTS_DTS_flags = 0x%02X, want 0x80 (PTS only)", pes[7])
	}
	if pes[8] != 5 {
		t.Errorf("PES_header_data_length = %d, want 5", pes[8])
	}
	if !bytes.Equal(pes[len(pes)-2:], []byte{0xDE, 0xAD}) {
		t.Error("payload did not survive into the PES packet")
	}
}
