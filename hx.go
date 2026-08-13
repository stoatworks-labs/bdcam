package main

// NDI|HX: sending H.264 straight from the VEPU instead of compressing on the
// CPU with SpeedHQ.
//
// There is no separate entry point for this. Compressed video goes through the
// ordinary NDIlib_send_send_video_v2, with a compressed FourCC and p_data
// pointing at an NDIlib_compressed_packet_t followed by the bitstream. The
// Advanced-SDK helpers the device's libndi exports — get_target_bit_rate,
// is_keyframe_required, wait_for_keyframe_request — are the give-away that this
// runtime supports it.
//
// The struct below is reconstructed from the Advanced SDK's documented layout,
// not from a header we hold, so it is the least certain code in this repo. If
// it is wrong the likely symptom is a receiver that connects and sees nothing,
// rather than a crash. TestCompressedPacketLayout pins the offsets so at least
// the reconstruction is explicit and checkable.

import "unsafe"

// H.264, highest bandwidth. The lowercase spelling is the low-bandwidth stream.
var (
	fourCCH264 = fourCC('H', '2', '6', '4')
	fourCCHEVC = fourCC('H', 'E', 'V', 'C')
)

// compressedKeyframe marks a packet a receiver can join on.
const compressedKeyframe = 1

// ndiCompressedPacket is NDIlib_compressed_packet_t. The bitstream follows it
// immediately, then any extra data (SPS/PPS), which we leave empty because
// h264parse config-interval=1 already repeats those inline on every keyframe.
type ndiCompressedPacket struct {
	version       uint32
	fourCC        uint32
	pts           int64
	dts           int64
	reserved      uint64
	flags         uint32
	dataSize      uint32
	extraDataSize uint32
	_             uint32 // tail padding to an 8-byte multiple
}

// SendCompressed submits one access unit as NDI|HX.
//
// pts arrives in the 90 kHz clock the TS muxer works in, and is converted to
// the 100 ns units the rest of the NDI API uses. Getting this wrong does not
// fail loudly — the receiver decodes the frames and then presents them wrongly,
// which looks like a picture fault rather than a timing one.
func (s *Sender) SendCompressed(buf []byte, au []byte, pts90k int64, keyframe bool, rateN, rateD, w, h int) {
	pts := pts90k * 10000000 / 90000
	const hdr = int(unsafe.Sizeof(ndiCompressedPacket{}))
	if len(buf) < hdr+len(au) {
		return
	}
	_ = pts90k
	p := (*ndiCompressedPacket)(unsafe.Pointer(&buf[0]))
	*p = ndiCompressedPacket{
		version:       uint32(hdr),
		fourCC:        fourCCH264,
		pts:           pts,
		dts:           pts, // the VEPU emits no B-frames, so these are equal
		dataSize:      uint32(len(au)),
		extraDataSize: 0,
	}
	if keyframe {
		p.flags = compressedKeyframe
	}
	copy(buf[hdr:], au)
	total := hdr + len(au)

	v := ndiVideoFrameV2{
		xres:            int32(w),
		yres:            int32(h),
		fourCC:          fourCCH264,
		frameRateN:      int32(rateN),
		frameRateD:      int32(rateD),
		frameFormatType: frameFormatProgressive,
		timecode:        timecodeSynthesize,
		pData:           &buf[0],
		// For a compressed FourCC this union member is the byte count, not a
		// stride.
		strideOrSize: int32(total),
	}
	s.n.sendVideoV2(s.inst, &v)
}

// CompressedHeaderSize is how much room to leave in front of an access unit.
func CompressedHeaderSize() int { return int(unsafe.Sizeof(ndiCompressedPacket{})) }
