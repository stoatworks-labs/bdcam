package main

// Runtime bindings to libndi's *send* API. As in tools/bdkvm we dlopen rather
// than link, so this binary carries no NDI code and runs against whatever
// libndi the device already has. That is the whole point: no SDK in this repo,
// and nothing NDI-derived in anything we hand out.
//
// Everything below is free-SDK surface. PLAY's libndi happens to be an Advanced
// build — it exports send_compressed_video and codec_h264_hevc_alpha — but
// nothing here depends on that. The compressed path is a separate milestone;
// see README.
//
// Struct layouts are aarch64 LP64, read from the SDK headers:
//   NDIlib_send_create_t      24 bytes
//   NDIlib_video_frame_v2_t   72 bytes

import (
	"fmt"
	"math"
	"unsafe"

	"github.com/ebitengine/purego"
)

// NDI_LIB_FOURCC packs little-endian, so 'U','Y','V','Y' is 0x59565955.
func fourCC(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

var (
	fourCCUYVY = fourCC('U', 'Y', 'V', 'Y')
	fourCCI420 = fourCC('I', '4', '2', '0')
	fourCCNV12 = fourCC('N', 'V', '1', '2')
	fourCCBGRA = fourCC('B', 'G', 'R', 'A')
)

func fourCCName(f uint32) string {
	return string([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}

const (
	frameFormatProgressive = 1
	// NDIlib_send_timecode_synthesize — let the library assign timecodes.
	timecodeSynthesize = int64(math.MaxInt64)
)

// NDIlib_source_t — only the name pointer is used here.
type ndiSource struct {
	pNDIName *byte
	pURL     *byte
}

type ndiSendCreate struct {
	pNDIName   *byte
	pGroups    *byte
	clockVideo bool
	clockAudio bool
	_          [6]byte
}

type ndiVideoFrameV2 struct {
	xres, yres      int32
	fourCC          uint32
	frameRateN      int32
	frameRateD      int32
	pictureAspect   float32
	frameFormatType int32
	_               [4]byte
	timecode        int64
	pData           *byte
	// Union: line stride for uncompressed, data size for compressed.
	strideOrSize int32
	_            [4]byte
	pMetadata    *byte
	timestamp    int64
}

type NDI struct {
	handle uintptr

	initialize func() bool
	destroy    func()
	version    func() *byte

	sendCreate    func(*ndiSendCreate) uintptr
	sendDestroy   func(uintptr)
	sendVideoV2   func(uintptr, *ndiVideoFrameV2)
	sendNumConns  func(uintptr, uint32) int32
	sendGetSource func(uintptr) *ndiSource
}

// candidate sonames, newest first — PLAY ships both
var ndiLibs = []string{
	"libndi.so.6", "libndi.so.6.0.1",
	"libndi.so.5", "libndi.so.5.5.2",
	"libndi.so",
}

// LoadNDI opens libndi. prefer, if set, is a soname or absolute path tried
// first — PLAY ships two copies and they need not be licensed alike, so being
// able to pin one is what makes the 30-minute test conclusive.
func LoadNDI(prefer string) (*NDI, error) {
	var h uintptr
	var err error
	var loaded string
	candidates := ndiLibs
	if prefer != "" {
		candidates = append([]string{prefer}, ndiLibs...)
	}
	for _, name := range candidates {
		h, err = purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil && h != 0 {
			loaded = name
			break
		}
	}
	if h == 0 {
		return nil, fmt.Errorf("could not dlopen libndi (tried %v): %v", candidates, err)
	}

	n := &NDI{handle: h}
	reg := func(ptr any, sym string) error {
		s, e := purego.Dlsym(h, sym)
		if e != nil || s == 0 {
			return fmt.Errorf("missing symbol %s", sym)
		}
		purego.RegisterFunc(ptr, s)
		return nil
	}
	for _, b := range []struct {
		p   any
		sym string
	}{
		{&n.initialize, "NDIlib_initialize"},
		{&n.destroy, "NDIlib_destroy"},
		{&n.version, "NDIlib_version"},
		{&n.sendCreate, "NDIlib_send_create"},
		{&n.sendDestroy, "NDIlib_send_destroy"},
		{&n.sendVideoV2, "NDIlib_send_send_video_v2"},
		{&n.sendNumConns, "NDIlib_send_get_no_connections"},
		{&n.sendGetSource, "NDIlib_send_get_source_name"},
	} {
		if e := reg(b.p, b.sym); e != nil {
			return nil, e
		}
	}
	if !n.initialize() {
		return nil, fmt.Errorf("NDIlib_initialize failed (unsupported CPU?)")
	}
	logf("loaded %s (%s)", loaded, goStr(n.version()))
	return n, nil
}

func (n *NDI) Close() {
	if n != nil && n.destroy != nil {
		n.destroy()
	}
}

type Sender struct {
	n    *NDI
	inst uintptr
	keep []*byte // keep C strings alive for the sender's lifetime
}

// NewSender creates a video sender. clockVideo makes libndi rate-limit
// send_video to the frame rate we declare, which is what you want in normal
// use. Turn it off to measure how fast the box can actually encode.
func (n *NDI) NewSender(name string, clockVideo bool) (*Sender, error) {
	namePtr := cstr(name)
	c := ndiSendCreate{pNDIName: namePtr, clockVideo: clockVideo}
	inst := n.sendCreate(&c)
	if inst == 0 {
		return nil, fmt.Errorf("NDIlib_send_create failed")
	}
	return &Sender{n: n, inst: inst, keep: []*byte{namePtr}}, nil
}

func (s *Sender) Close() {
	if s != nil && s.inst != 0 {
		s.n.sendDestroy(s.inst)
		s.inst = 0
	}
}

// Connections is the number of receivers currently attached. This is the signal
// we watch for the Advanced-SDK 30-minute development limit: if the library
// stops a stream, receivers drop off while we are still submitting frames.
func (s *Sender) Connections() int {
	return int(s.n.sendNumConns(s.inst, 0))
}

func (s *Sender) SourceName() string {
	src := s.n.sendGetSource(s.inst)
	if src == nil {
		return ""
	}
	return goStr(src.pNDIName)
}

// SourceURL is the "host:port" libndi is listening on. The PLAY's own finder
// hides local sources, so pointing its decoder at us needs an explicit address
// rather than a name lookup.
func (s *Sender) SourceURL() string {
	src := s.n.sendGetSource(s.inst)
	if src == nil {
		return ""
	}
	return goStr(src.pURL)
}

// SendVideo submits one uncompressed frame. stride is bytes per line for packed
// formats; for planar formats pass the luma stride and lay the planes out
// contiguously, which is what libndi expects.
func (s *Sender) SendVideo(f *Frame, rateN, rateD int) {
	if len(f.Data) == 0 {
		return
	}
	v := ndiVideoFrameV2{
		xres:            int32(f.Width),
		yres:            int32(f.Height),
		fourCC:          f.FourCC,
		frameRateN:      int32(rateN),
		frameRateD:      int32(rateD),
		pictureAspect:   0, // 0 == square pixels, derive from resolution
		frameFormatType: frameFormatProgressive,
		timecode:        timecodeSynthesize,
		pData:           &f.Data[0],
		strideOrSize:    int32(f.Stride),
	}
	s.n.sendVideoV2(s.inst, &v)
	// The call is synchronous: libndi has finished with the buffer by the time
	// it returns, so f.Data can be reused for the next frame.
}

func cstr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

func goStr(p *byte) string {
	if p == nil {
		return ""
	}
	var out []byte
	for i := 0; ; i++ {
		c := *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(i)))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return string(out)
}
