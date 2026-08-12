package main

// A minimal MPEG-TS muxer for a single H.264 elementary stream.
//
// It exists because the PLAY has no muxer at all — GStreamer there ships
// tsdemux and tsparse but nothing that writes TS — and SRT receivers expect
// MPEG-TS. Scope is deliberately the narrow case we actually generate: one
// video PID, no audio yet, no B-frames (the VEPU does not produce them), so
// DTS always equals PTS and the PES header carries PTS alone.
//
// Everything here is ISO/IEC 13818-1. The layout notes on each field are so
// the next person does not have to re-derive them from the spec.

import (
	"fmt"
	"io"
)

const (
	tsPacketSize = 188
	tsSync       = 0x47

	pidPAT = 0x0000
	pidPMT = 0x1000
	pidVid = 0x0100

	streamTypeH264 = 0x1B
	streamIDVideo  = 0xE0

	// PSI is cheap and receivers that join late need it, so repeat it often.
	psiIntervalPackets = 40
)

type TSMuxer struct {
	w io.Writer

	ccPAT, ccPMT, ccVid byte
	sincePSI            int
	pktBuf              [tsPacketSize]byte
	wrote               bool
}

func NewTSMuxer(w io.Writer) *TSMuxer { return &TSMuxer{w: w} }

// WriteAccessUnit emits one coded picture. pts is in the 90 kHz clock. A
// keyframe additionally carries PCR and the random-access indicator, which is
// what lets a receiver join mid-stream.
func (m *TSMuxer) WriteAccessUnit(au []byte, pts int64, keyframe bool) error {
	if len(au) == 0 {
		return nil
	}
	if !m.wrote || m.sincePSI >= psiIntervalPackets || keyframe {
		if err := m.writePAT(); err != nil {
			return err
		}
		if err := m.writePMT(); err != nil {
			return err
		}
		m.sincePSI = 0
		m.wrote = true
	}
	pes := buildPES(au, pts)
	return m.writePayload(pidVid, pes, &m.ccVid, pts, keyframe)
}

// writePayload splits a PES packet across TS packets. The first gets
// payload_unit_start_indicator; the last is padded with an adaptation field
// rather than by lying about the length.
func (m *TSMuxer) writePayload(pid uint16, payload []byte, cc *byte, pcr int64, keyframe bool) error {
	first := true
	for len(payload) > 0 {
		p := m.pktBuf[:]
		for i := range p {
			p[i] = 0xFF
		}
		p[0] = tsSync
		p[1] = byte(pid >> 8)
		if first {
			p[1] |= 0x40 // payload_unit_start_indicator
		}
		p[2] = byte(pid & 0xFF)

		// Adaptation field is needed for PCR on the first packet of a
		// keyframe, and for stuffing when the remainder is short.
		wantPCR := first && keyframe
		space := tsPacketSize - 4
		afLen := 0
		if wantPCR {
			afLen = 8 // length byte + flags + 6 byte PCR
		} else if len(payload) < space {
			afLen = space - len(payload)
			if afLen == 1 {
				afLen = 1 // a lone length byte of 0 is legal
			}
		}

		if afLen > 0 {
			p[3] = 0x30 | (*cc & 0x0F) // adaptation field + payload
			p[4] = byte(afLen - 1)
			if afLen >= 2 {
				flags := byte(0)
				if wantPCR {
					flags |= 0x10 // PCR_flag
				}
				if keyframe && first {
					flags |= 0x40 // random_access_indicator
				}
				p[5] = flags
				if wantPCR {
					writePCR(p[6:12], pcr)
				}
				// remaining adaptation bytes stay 0xFF stuffing
			}
		} else {
			p[3] = 0x10 | (*cc & 0x0F) // payload only
		}
		*cc = (*cc + 1) & 0x0F

		off := 4 + afLen
		n := copy(p[off:], payload)
		payload = payload[n:]
		if _, err := m.w.Write(p); err != nil {
			return err
		}
		m.sincePSI++
		first = false
	}
	return nil
}

// buildPES wraps an access unit in a PES packet. PES_packet_length is left at
// 0, which is legal (and normal) for video and means "until the next start".
func buildPES(au []byte, pts int64) []byte {
	out := make([]byte, 0, len(au)+19)
	out = append(out, 0x00, 0x00, 0x01, streamIDVideo)
	out = append(out, 0x00, 0x00) // PES_packet_length = 0, unbounded
	out = append(out, 0x80)       // '10' marker, no scrambling, not priority
	out = append(out, 0x80)       // PTS present, DTS absent
	out = append(out, 0x05)       // PES_header_data_length
	out = append(out, encodePTS(pts, 0x02)...)
	return append(out, au...)
}

// encodePTS is the 5-byte marker-interleaved timestamp from 13818-1 2.4.3.7.
// prefix is 0x02 for a PTS-only header.
func encodePTS(pts int64, prefix byte) []byte {
	v := uint64(pts) & 0x1FFFFFFFF
	return []byte{
		prefix<<4 | byte(v>>29)&0x0E | 0x01,
		byte(v >> 22),
		byte(v>>14) | 0x01,
		byte(v >> 7),
		byte(v<<1) | 0x01,
	}
}

// writePCR encodes the 33-bit base plus 9-bit extension across 6 bytes. We run
// the extension at zero: a 90 kHz-accurate PCR is plenty for a stream whose
// timestamps came from the same clock.
func writePCR(dst []byte, pts int64) {
	base := uint64(pts) & 0x1FFFFFFFF
	dst[0] = byte(base >> 25)
	dst[1] = byte(base >> 17)
	dst[2] = byte(base >> 9)
	dst[3] = byte(base >> 1)
	dst[4] = byte(base<<7) | 0x7E // 6 reserved bits set, ext high bit 0
	dst[5] = 0x00
}

func (m *TSMuxer) writePAT() error {
	// program_number 1 -> PMT PID
	body := []byte{0x00, 0x01, 0xE0 | byte(pidPMT>>8), byte(pidPMT & 0xFF)}
	sec := buildSection(0x00, 0x0001, body)
	return m.writePSI(pidPAT, sec, &m.ccPAT)
}

func (m *TSMuxer) writePMT() error {
	body := []byte{
		0xE0 | byte(pidVid>>8), byte(pidVid & 0xFF), // PCR_PID
		0xF0, 0x00, // program_info_length = 0
		streamTypeH264,
		0xE0 | byte(pidVid>>8), byte(pidVid & 0xFF),
		0xF0, 0x00, // ES_info_length = 0
	}
	sec := buildSection(0x02, 0x0001, body)
	return m.writePSI(pidPMT, sec, &m.ccPMT)
}

// buildSection assembles a PSI section including its CRC32.
func buildSection(tableID byte, extension uint16, body []byte) []byte {
	// section_length covers everything after the length field, including CRC.
	length := 5 + len(body) + 4
	s := make([]byte, 0, 3+length)
	s = append(s, tableID)
	s = append(s, 0xB0|byte(length>>8), byte(length&0xFF)) // '1011' + length
	s = append(s, byte(extension>>8), byte(extension&0xFF))
	s = append(s, 0xC1) // version 0, current_next_indicator = 1
	s = append(s, 0x00) // section_number
	s = append(s, 0x00) // last_section_number
	s = append(s, body...)
	crc := crc32MPEG(s)
	return append(s, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// writePSI emits a section in a single TS packet, which is always enough for
// the small PAT and PMT we generate.
func (m *TSMuxer) writePSI(pid uint16, section []byte, cc *byte) error {
	if len(section)+1 > tsPacketSize-4 {
		return fmt.Errorf("PSI section of %d bytes does not fit one packet", len(section))
	}
	p := m.pktBuf[:]
	for i := range p {
		p[i] = 0xFF
	}
	p[0] = tsSync
	p[1] = 0x40 | byte(pid>>8) // payload_unit_start_indicator
	p[2] = byte(pid & 0xFF)
	p[3] = 0x10 | (*cc & 0x0F)
	*cc = (*cc + 1) & 0x0F
	p[4] = 0x00 // pointer_field
	copy(p[5:], section)
	_, err := m.w.Write(p)
	m.sincePSI++
	return err
}

// crc32MPEG is the MSB-first CRC-32/MPEG-2 used by PSI sections.
func crc32MPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, v := range b {
		crc ^= uint32(v) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
