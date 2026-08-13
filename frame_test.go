package main

import (
	"image"
	"testing"
	"unsafe"
)

// The V4L2 ioctl request number encodes the size of the struct it carries, so
// the kernel rejects a mismatched layout with EINVAL/ENOTTY on the device. That
// is a slow way to find out. These check the two against each other locally.
func TestIoctlStructSizes(t *testing.T) {
	size := func(req uintptr) uintptr { return (req >> 16) & 0x3fff }
	for _, c := range []struct {
		name string
		req  uintptr
		got  uintptr
	}{
		{"v4l2_capability", vidiocQuerycap, unsafe.Sizeof(v4l2Capability{})},
		{"v4l2_fmtdesc", vidiocEnumFmt, unsafe.Sizeof(v4l2Fmtdesc{})},
		{"v4l2_format", vidiocSFmt, unsafe.Sizeof(v4l2Format{})},
		{"v4l2_requestbuffers", vidiocReqbufs, unsafe.Sizeof(v4l2Requestbuffers{})},
		{"v4l2_buffer", vidiocQuerybuf, unsafe.Sizeof(v4l2Buffer{})},
		{"v4l2_buffer(qbuf)", vidiocQbuf, unsafe.Sizeof(v4l2Buffer{})},
		{"v4l2_buffer(dqbuf)", vidiocDqbuf, unsafe.Sizeof(v4l2Buffer{})},
		{"v4l2_streamparm", vidiocSParm, unsafe.Sizeof(v4l2StreamParm{})},
		{"v4l2_frmsizeenum", vidiocEnumFramesizes, unsafe.Sizeof(v4l2Frmsizeenum{})},
		{"v4l2_frmivalenum", vidiocEnumFrameintervals, unsafe.Sizeof(v4l2Frmivalenum{})},
	} {
		if want := size(c.req); want != c.got {
			t.Errorf("%s: ioctl 0x%x encodes %d bytes, Go struct is %d",
				c.name, c.req, want, c.got)
		}
	}
	// v4l2_pix_format has no ioctl of its own; it lives inside the union.
	if got := unsafe.Sizeof(v4l2PixFormat{}); got != 48 {
		t.Errorf("v4l2_pix_format is %d bytes, want 48", got)
	}
	if got := unsafe.Sizeof(v4l2CaptureParm{}); got != 40 {
		t.Errorf("v4l2_captureparm is %d bytes, want 40", got)
	}
}

// Same idea for the NDI structs — these are read from the SDK headers and there
// is no runtime check on the other side at all, so a mistake here is a garbled
// frame or a crash inside libndi.
func TestNDIStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(ndiSendCreate{}); got != 24 {
		t.Errorf("NDIlib_send_create_t is %d bytes, want 24", got)
	}
	if got := unsafe.Sizeof(ndiVideoFrameV2{}); got != 72 {
		t.Errorf("NDIlib_video_frame_v2_t is %d bytes, want 72", got)
	}
	if got := unsafe.Sizeof(ndiSource{}); got != 16 {
		t.Errorf("NDIlib_source_t is %d bytes, want 16", got)
	}
	// Field offsets matter as much as the total.
	var v ndiVideoFrameV2
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"FourCC", unsafe.Offsetof(v.fourCC), 8},
		{"timecode", unsafe.Offsetof(v.timecode), 32},
		{"p_data", unsafe.Offsetof(v.pData), 40},
		{"line_stride", unsafe.Offsetof(v.strideOrSize), 48},
		{"p_metadata", unsafe.Offsetof(v.pMetadata), 56},
		{"timestamp", unsafe.Offsetof(v.timestamp), 64},
	} {
		if c.got != c.want {
			t.Errorf("NDIlib_video_frame_v2_t.%s at offset %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestFourCC(t *testing.T) {
	// NDI_LIB_FOURCC is little-endian packed: 'U'|'Y'<<8|'V'<<16|'Y'<<24.
	if got := fourCCUYVY; got != 0x59565955 {
		t.Errorf("UYVY fourcc = 0x%08x, want 0x59565955", got)
	}
	if got := fourCCName(fourCCUYVY); got != "UYVY" {
		t.Errorf("fourCCName round trip = %q", got)
	}
	if got := fourCCName(pixMJPG); got != "MJPG" {
		t.Errorf("MJPG round trip = %q", got)
	}
}

func TestYUYVToUYVY(t *testing.T) {
	// Two pixels: Y0=10 U=20 Y1=30 V=40
	src := []byte{10, 20, 30, 40}
	dst := make([]byte, 4)
	yuyvToUYVY(src, dst)
	want := []byte{20, 10, 40, 30} // U Y0 V Y1
	for i := range want {
		if dst[i] != want[i] {
			t.Fatalf("got %v, want %v", dst, want)
		}
	}
}

func TestYUYVToUYVYShortDst(t *testing.T) {
	// Must not panic or run past the end when the destination is smaller.
	src := make([]byte, 64)
	dst := make([]byte, 8)
	yuyvToUYVY(src, dst)
}

func TestYCbCrToUYVY(t *testing.T) {
	const w, h = 4, 2
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = byte(100 + i)
	}
	for i := range img.Cb {
		img.Cb[i] = byte(10 + i)
		img.Cr[i] = byte(200 + i)
	}
	dst := make([]byte, w*h*2)
	if err := ycbcrToUYVY(img, dst, w, h); err != nil {
		t.Fatal(err)
	}
	// Row 0, first pixel pair: U=Cb[0], Y0=Y[0], V=Cr[0], Y1=Y[1]
	if dst[0] != img.Cb[0] || dst[1] != img.Y[0] || dst[2] != img.Cr[0] || dst[3] != img.Y[1] {
		t.Errorf("row 0 pair 0 = %v", dst[0:4])
	}
	// 4:2:0 — row 1 shares chroma with row 0, luma comes from its own row.
	r1 := w * 2
	if dst[r1+0] != img.Cb[0] || dst[r1+1] != img.Y[img.YStride] {
		t.Errorf("row 1 pair 0 = %v (chroma should be shared with row 0)", dst[r1:r1+4])
	}
	// Second pair of row 0 uses the next chroma sample.
	if dst[4] != img.Cb[1] || dst[5] != img.Y[2] {
		t.Errorf("row 0 pair 1 = %v", dst[4:8])
	}
}

func TestYCbCrToUYVY422(t *testing.T) {
	const w, h = 4, 2
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio422)
	for i := range img.Y {
		img.Y[i] = byte(i)
	}
	for i := range img.Cb {
		img.Cb[i] = byte(i)
		img.Cr[i] = byte(100 + i)
	}
	dst := make([]byte, w*h*2)
	if err := ycbcrToUYVY(img, dst, w, h); err != nil {
		t.Fatal(err)
	}
	// 4:2:2 — row 1 has its own chroma row.
	r1 := w * 2
	if dst[r1+0] != img.Cb[img.CStride] {
		t.Errorf("4:2:2 row 1 chroma = %d, want %d", dst[r1+0], img.Cb[img.CStride])
	}
}

func TestYCbCrTooSmall(t *testing.T) {
	img := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	dst := make([]byte, 4*2*2)
	if err := ycbcrToUYVY(img, dst, 4, 2); err == nil {
		t.Fatal("expected an error when the JPEG is smaller than the declared size")
	}
}

func TestSyntheticMoves(t *testing.T) {
	s := NewSyntheticSource(64, 4)
	f1 := s.Next()
	if f1.Width != 64 || f1.Height != 4 || f1.FourCC != fourCCUYVY || f1.Stride != 128 {
		t.Fatalf("unexpected frame geometry: %+v", *f1)
	}
	first := append([]byte(nil), f1.Data...)
	f2 := s.Next()
	same := true
	for i := range first {
		if first[i] != f2.Data[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("consecutive synthetic frames are identical — a receiver could not tell a freeze from live video")
	}
}

func TestConverterRejectsUnknown(t *testing.T) {
	if _, err := NewConverter(16, 16, pixH264); err == nil {
		t.Fatal("expected H264 to be rejected: there is no NDI path for it yet")
	}
}

func TestParseSize(t *testing.T) {
	if w, h, err := parseSize("1920x1080"); err != nil || w != 1920 || h != 1080 {
		t.Errorf("parseSize(1920x1080) = %d,%d,%v", w, h, err)
	}
	if _, _, err := parseSize("1920"); err == nil {
		t.Error("expected an error for a malformed size")
	}
	if _, _, err := parseSize("0x1080"); err == nil {
		t.Error("expected an error for a zero dimension")
	}
}

func TestI420ToUYVY(t *testing.T) {
	const w, h = 4, 4
	src := make([]byte, w*h*3/2)
	for i := 0; i < w*h; i++ {
		src[i] = byte(i) // luma 0..15
	}
	u := src[w*h:]
	v := src[w*h+(w/2)*(h/2):]
	for i := 0; i < (w/2)*(h/2); i++ {
		u[i] = byte(100 + i)
		v[i] = byte(200 + i)
	}
	dst := make([]byte, w*h*2)
	if err := i420ToUYVY(src, dst, w, h); err != nil {
		t.Fatal(err)
	}
	// Row 0, first pair: U, Y0, V, Y1
	if dst[0] != 100 || dst[1] != 0 || dst[2] != 200 || dst[3] != 1 {
		t.Errorf("row 0 pair 0 = %v, want [100 0 200 1]", dst[0:4])
	}
	// 4:2:0 -> 4:2:2 duplicates chroma down: row 1 shares row 0's chroma but
	// has its own luma.
	r1 := w * 2
	if dst[r1+0] != 100 || dst[r1+1] != byte(w) {
		t.Errorf("row 1 pair 0 = %v, want chroma 100 and luma %d", dst[r1:r1+4], w)
	}
	// Row 2 moves to the next chroma row.
	r2 := 2 * w * 2
	if dst[r2+0] != byte(100+w/2) {
		t.Errorf("row 2 chroma = %d, want %d", dst[r2+0], 100+w/2)
	}
}

func TestI420ToUYVYRejectsShortBuffers(t *testing.T) {
	if err := i420ToUYVY(make([]byte, 10), make([]byte, 4*4*2), 4, 4); err == nil {
		t.Error("a short source should be refused, not read past the end")
	}
	if err := i420ToUYVY(make([]byte, 4*4*3/2), make([]byte, 10), 4, 4); err == nil {
		t.Error("a short destination should be refused")
	}
}

func TestI420ToNV12(t *testing.T) {
	const w, h = 4, 4
	src := make([]byte, w*h*3/2)
	for i := 0; i < w*h; i++ {
		src[i] = byte(i)
	}
	cs := (w / 2) * (h / 2)
	for i := 0; i < cs; i++ {
		src[w*h+i] = byte(100 + i)
		src[w*h+cs+i] = byte(200 + i)
	}
	dst := make([]byte, w*h*3/2)
	if err := i420ToNV12(src, dst, w, h); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < w*h; i++ {
		if dst[i] != byte(i) {
			t.Fatalf("luma changed at %d: %d", i, dst[i])
		}
	}
	// Chroma interleaved as Cb, Cr pairs.
	for i := 0; i < cs; i++ {
		if dst[w*h+i*2] != byte(100+i) || dst[w*h+i*2+1] != byte(200+i) {
			t.Fatalf("chroma pair %d = %d,%d", i, dst[w*h+i*2], dst[w*h+i*2+1])
		}
	}
}
