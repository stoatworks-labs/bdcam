package main

// Minimal V4L2 capture, written directly against the ioctl interface so the
// binary has no dependency beyond purego. The device is a UVC webcam on the
// PLAY's USB-A port; `uvcvideo` is built into the stock kernel (verified in
// notes/03), so nothing needs installing for a camera to appear as /dev/videoN.
//
// Struct layouts and ioctl numbers are aarch64 LP64. The ioctl numbers encode
// the struct size, so a layout mistake shows up immediately as EINVAL/ENOTTY
// rather than as silent corruption — if you change a struct here, check the
// constant still matches.

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	vidiocQuerycap           = 0x80685600 // _IOR ('V',  0, v4l2_capability{104})
	vidiocEnumFmt            = 0xc0405602 // _IOWR('V',  2, v4l2_fmtdesc{64})
	vidiocSFmt               = 0xc0d05605 // _IOWR('V',  5, v4l2_format{208})
	vidiocReqbufs            = 0xc0145608 // _IOWR('V',  8, v4l2_requestbuffers{20})
	vidiocQuerybuf           = 0xc0585609 // _IOWR('V',  9, v4l2_buffer{88})
	vidiocQbuf               = 0xc058560f // _IOWR('V', 15, v4l2_buffer{88})
	vidiocDqbuf              = 0xc0585611 // _IOWR('V', 17, v4l2_buffer{88})
	vidiocStreamon           = 0x40045612 // _IOW ('V', 18, int)
	vidiocStreamoff          = 0x40045613 // _IOW ('V', 19, int)
	vidiocSParm              = 0xc0cc5616 // _IOWR('V', 22, v4l2_streamparm{204})
	vidiocEnumFramesizes     = 0xc02c564a // _IOWR('V', 74, v4l2_frmsizeenum{44})
	vidiocEnumFrameintervals = 0xc034564b // _IOWR('V', 75, v4l2_frmivalenum{52})

	bufTypeVideoCapture = 1
	memoryMMAP          = 1
	fieldNone           = 1

	capVideoCapture = 0x00000001
	capStreaming    = 0x04000000

	frmsizeTypeDiscrete = 1
	frmivalTypeDiscrete = 1
)

// V4L2 pixel formats we care about. These are the same FourCC packing as NDI.
var (
	pixYUYV = fourCC('Y', 'U', 'Y', 'V')
	pixUYVY = fourCC('U', 'Y', 'V', 'Y')
	pixMJPG = fourCC('M', 'J', 'P', 'G')
	pixNV12 = fourCC('N', 'V', '1', '2')
	pixH264 = fourCC('H', '2', '6', '4')
)

type v4l2Capability struct {
	Driver       [16]byte
	Card         [32]byte
	BusInfo      [32]byte
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2Fmtdesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]byte
	Pixelformat uint32
	Reserved    [4]uint32
}

// v4l2_format is a u32 type plus a 200-byte union. The union contains
// v4l2_window, which holds pointers, so it is 8-aligned and there are 4 bytes
// of padding after Type — hence 208 bytes, not 204.
type v4l2Format struct {
	Type uint32
	_    uint32
	Fmt  [200]byte
}

type v4l2PixFormat struct {
	Width        uint32
	Height       uint32
	Pixelformat  uint32
	Field        uint32
	BytesPerLine uint32
	SizeImage    uint32
	Colorspace   uint32
	Priv         uint32
	Flags        uint32
	YcbcrEnc     uint32
	Quantization uint32
	XferFunc     uint32
}

type v4l2Requestbuffers struct {
	Count        uint32
	Type         uint32
	Memory       uint32
	Capabilities uint32
	Flags        uint8
	Reserved     [3]uint8
}

type v4l2Buffer struct {
	Index     uint32
	Type      uint32
	BytesUsed uint32
	Flags     uint32
	Field     uint32
	_         uint32 // struct timeval is 8-aligned on arm64
	TvSec     int64
	TvUsec    int64
	Timecode  [16]byte
	Sequence  uint32
	Memory    uint32
	M         uint64 // union: offset / userptr / planes / fd
	Length    uint32
	Reserved2 uint32
	RequestFD int32
	_         uint32
}

// v4l2_streamparm's union has no pointer members, so unlike v4l2_format there
// is no padding after Type: 4 + 200 = 204.
type v4l2StreamParm struct {
	Type uint32
	Parm [200]byte
}

type v4l2CaptureParm struct {
	Capability    uint32
	CaptureMode   uint32
	TimePerFrameN uint32
	TimePerFrameD uint32
	ExtendedMode  uint32
	ReadBuffers   uint32
	Reserved      [4]uint32
}

type v4l2Frmsizeenum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	Width       uint32 // discrete
	Height      uint32 // discrete
	_           [4]uint32
	Reserved    [2]uint32
}

type v4l2Frmivalenum struct {
	Index       uint32
	PixelFormat uint32
	Width       uint32
	Height      uint32
	Type        uint32
	Numerator   uint32 // discrete
	Denominator uint32 // discrete
	_           [4]uint32
	Reserved    [2]uint32
}

// V4L2_CAP_DEVICE_CAPS means the per-node caps in DeviceCaps are the ones that
// matter — a UVC camera exposes both a capture node and a metadata node, and
// only the per-node value tells them apart.
func deviceCaps(qc *v4l2Capability) uint32 {
	if qc.Capabilities&0x80000000 != 0 {
		return qc.DeviceCaps
	}
	return qc.Capabilities
}

// IsCaptureDevice reports whether path is a streaming video capture node.
func IsCaptureDevice(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var qc v4l2Capability
	if err := ioctl(fd, vidiocQuerycap, unsafe.Pointer(&qc)); err != nil {
		return false
	}
	c := deviceCaps(&qc)
	return c&capVideoCapture != 0 && c&capStreaming != 0
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		if errno == 0 {
			return nil
		}
		if errno == syscall.EINTR {
			continue
		}
		return errno
	}
}

type Capture struct {
	fd     int
	Path   string
	Card   string
	Width  int
	Height int
	Pixfmt uint32
	Stride int
	bufs   [][]byte
	on     bool
}

// OpenCapture configures the device and maps buffers. pixfmt of 0 means "pick
// one": uncompressed is preferred over MJPEG because decoding JPEG on four A53s
// is the single most expensive thing this program could do (see README).
func OpenCapture(path string, width, height int, pixfmt uint32, fps int, nbufs int) (*Capture, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	c := &Capture{fd: fd, Path: path}

	var qc v4l2Capability
	if err := ioctl(fd, vidiocQuerycap, unsafe.Pointer(&qc)); err != nil {
		c.Close()
		return nil, fmt.Errorf("QUERYCAP: %w", err)
	}
	c.Card = cstring(qc.Card[:])
	caps := deviceCaps(&qc)
	if caps&capVideoCapture == 0 {
		c.Close()
		return nil, fmt.Errorf("%s (%s) is not a capture device", path, c.Card)
	}
	if caps&capStreaming == 0 {
		c.Close()
		return nil, fmt.Errorf("%s (%s) does not support streaming I/O", path, c.Card)
	}

	if pixfmt == 0 {
		pixfmt, err = c.pickFormat()
		if err != nil {
			c.Close()
			return nil, err
		}
	}

	var f v4l2Format
	f.Type = bufTypeVideoCapture
	pix := (*v4l2PixFormat)(unsafe.Pointer(&f.Fmt[0]))
	pix.Width = uint32(width)
	pix.Height = uint32(height)
	pix.Pixelformat = pixfmt
	pix.Field = fieldNone
	if err := ioctl(fd, vidiocSFmt, unsafe.Pointer(&f)); err != nil {
		c.Close()
		return nil, fmt.Errorf("S_FMT %s %dx%d: %w", fourCCName(pixfmt), width, height, err)
	}
	// The driver rewrites the struct with what it actually agreed to.
	c.Width, c.Height = int(pix.Width), int(pix.Height)
	c.Pixfmt, c.Stride = pix.Pixelformat, int(pix.BytesPerLine)
	if c.Width != width || c.Height != height {
		logf("note: driver adjusted geometry to %dx%d", c.Width, c.Height)
	}
	if c.Pixfmt != pixfmt {
		logf("note: driver adjusted format to %s", fourCCName(c.Pixfmt))
	}

	if fps > 0 {
		var sp v4l2StreamParm
		sp.Type = bufTypeVideoCapture
		cp := (*v4l2CaptureParm)(unsafe.Pointer(&sp.Parm[0]))
		cp.TimePerFrameN = 1
		cp.TimePerFrameD = uint32(fps)
		if err := ioctl(fd, vidiocSParm, unsafe.Pointer(&sp)); err != nil {
			logf("note: S_PARM %d fps rejected (%v) — using the driver default", fps, err)
		} else if cp.TimePerFrameN != 0 {
			logf("frame interval: %d/%d", cp.TimePerFrameN, cp.TimePerFrameD)
		}
	}

	var rb v4l2Requestbuffers
	rb.Count = uint32(nbufs)
	rb.Type = bufTypeVideoCapture
	rb.Memory = memoryMMAP
	if err := ioctl(fd, vidiocReqbufs, unsafe.Pointer(&rb)); err != nil {
		c.Close()
		return nil, fmt.Errorf("REQBUFS: %w", err)
	}
	if rb.Count < 2 {
		c.Close()
		return nil, fmt.Errorf("REQBUFS returned only %d buffers", rb.Count)
	}

	for i := 0; i < int(rb.Count); i++ {
		var b v4l2Buffer
		b.Index = uint32(i)
		b.Type = bufTypeVideoCapture
		b.Memory = memoryMMAP
		if err := ioctl(fd, vidiocQuerybuf, unsafe.Pointer(&b)); err != nil {
			c.Close()
			return nil, fmt.Errorf("QUERYBUF %d: %w", i, err)
		}
		mem, err := syscall.Mmap(fd, int64(uint32(b.M)), int(b.Length),
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("mmap buffer %d: %w", i, err)
		}
		c.bufs = append(c.bufs, mem)
	}
	return c, nil
}

// pickFormat prefers formats in order of what they cost us downstream:
// UYVY (nothing to do), YUYV (a byte swap), NV12 (a plane copy), then MJPEG.
func (c *Capture) pickFormat() (uint32, error) {
	avail := map[uint32]bool{}
	var order []uint32
	for i := 0; ; i++ {
		var d v4l2Fmtdesc
		d.Index = uint32(i)
		d.Type = bufTypeVideoCapture
		if err := ioctl(c.fd, vidiocEnumFmt, unsafe.Pointer(&d)); err != nil {
			break
		}
		avail[d.Pixelformat] = true
		order = append(order, d.Pixelformat)
	}
	for _, want := range []uint32{pixUYVY, pixYUYV, pixNV12, pixMJPG} {
		if avail[want] {
			return want, nil
		}
	}
	if len(order) > 0 {
		names := make([]string, len(order))
		for i, f := range order {
			names[i] = fourCCName(f)
		}
		return 0, fmt.Errorf("no supported format; device offers %v", names)
	}
	return 0, fmt.Errorf("device offers no capture formats at all")
}

func (c *Capture) Start() error {
	for i := range c.bufs {
		if err := c.queue(i); err != nil {
			return err
		}
	}
	t := int32(bufTypeVideoCapture)
	if err := ioctl(c.fd, vidiocStreamon, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("STREAMON: %w", err)
	}
	c.on = true
	return nil
}

func (c *Capture) queue(i int) error {
	var b v4l2Buffer
	b.Index = uint32(i)
	b.Type = bufTypeVideoCapture
	b.Memory = memoryMMAP
	if err := ioctl(c.fd, vidiocQbuf, unsafe.Pointer(&b)); err != nil {
		return fmt.Errorf("QBUF %d: %w", i, err)
	}
	return nil
}

// Grab blocks for the next frame and hands the mapped buffer to fn. The slice
// is only valid until fn returns — it goes straight back to the driver.
func (c *Capture) Grab(fn func(data []byte, seq uint32) error) error {
	var b v4l2Buffer
	b.Type = bufTypeVideoCapture
	b.Memory = memoryMMAP
	if err := ioctl(c.fd, vidiocDqbuf, unsafe.Pointer(&b)); err != nil {
		return fmt.Errorf("DQBUF: %w", err)
	}
	n := int(b.BytesUsed)
	if n > len(c.bufs[b.Index]) {
		n = len(c.bufs[b.Index])
	}
	err := fn(c.bufs[b.Index][:n], b.Sequence)
	if qerr := c.queue(int(b.Index)); qerr != nil && err == nil {
		err = qerr
	}
	return err
}

func (c *Capture) Close() {
	if c == nil || c.fd < 0 {
		return
	}
	if c.on {
		t := int32(bufTypeVideoCapture)
		_ = ioctl(c.fd, vidiocStreamoff, unsafe.Pointer(&t))
		c.on = false
	}
	for _, m := range c.bufs {
		_ = syscall.Munmap(m)
	}
	c.bufs = nil
	_ = syscall.Close(c.fd)
	c.fd = -1
}

// Describe enumerates formats, sizes and frame rates. This is what you run
// first on the device: it tells you whether the camera can give you something
// uncompressed, which decides the whole CPU budget.
func Describe(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer syscall.Close(fd)

	var qc v4l2Capability
	if err := ioctl(fd, vidiocQuerycap, unsafe.Pointer(&qc)); err != nil {
		return "", fmt.Errorf("QUERYCAP: %w", err)
	}
	if deviceCaps(&qc)&capVideoCapture == 0 {
		return "", fmt.Errorf("not a video capture device")
	}
	out := fmt.Sprintf("%s: %s (%s, bus %s)\n", path,
		cstring(qc.Card[:]), cstring(qc.Driver[:]), cstring(qc.BusInfo[:]))

	for i := 0; ; i++ {
		var d v4l2Fmtdesc
		d.Index = uint32(i)
		d.Type = bufTypeVideoCapture
		if err := ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)); err != nil {
			break
		}
		out += fmt.Sprintf("  %s  %s\n", fourCCName(d.Pixelformat), cstring(d.Description[:]))
		for j := 0; ; j++ {
			var fs v4l2Frmsizeenum
			fs.Index = uint32(j)
			fs.PixelFormat = d.Pixelformat
			if err := ioctl(fd, vidiocEnumFramesizes, unsafe.Pointer(&fs)); err != nil {
				break
			}
			if fs.Type != frmsizeTypeDiscrete {
				out += "    (stepwise/continuous sizes)\n"
				break
			}
			rates := ""
			for k := 0; ; k++ {
				var fi v4l2Frmivalenum
				fi.Index = uint32(k)
				fi.PixelFormat = d.Pixelformat
				fi.Width, fi.Height = fs.Width, fs.Height
				if err := ioctl(fd, vidiocEnumFrameintervals, unsafe.Pointer(&fi)); err != nil {
					break
				}
				if fi.Type != frmivalTypeDiscrete || fi.Numerator == 0 {
					break
				}
				rates += fmt.Sprintf(" %g", float64(fi.Denominator)/float64(fi.Numerator))
			}
			out += fmt.Sprintf("    %dx%d fps:%s\n", fs.Width, fs.Height, rates)
		}
	}
	return out, nil
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// EnumFormats lists the pixel formats a capture device offers, without
// configuring or streaming it.
func EnumFormats(path string) ([]uint32, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var out []uint32
	for i := 0; ; i++ {
		var d v4l2Fmtdesc
		d.Index = uint32(i)
		d.Type = bufTypeVideoCapture
		if err := ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)); err != nil {
			break
		}
		out = append(out, d.Pixelformat)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s offers no capture formats", path)
	}
	return out, nil
}

// SupportedRates lists the discrete frame rates a device offers for a format
// and size. Continuous or stepwise ranges return nil: the caller should then
// not pin a rate at all.
func SupportedRates(path string, pixfmt uint32, width, height int) ([]int, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	var out []int
	for i := 0; ; i++ {
		var fi v4l2Frmivalenum
		fi.Index = uint32(i)
		fi.PixelFormat = pixfmt
		fi.Width, fi.Height = uint32(width), uint32(height)
		if err := ioctl(fd, vidiocEnumFrameintervals, unsafe.Pointer(&fi)); err != nil {
			break
		}
		if fi.Type != frmivalTypeDiscrete || fi.Numerator == 0 {
			return nil, nil
		}
		out = append(out, int(fi.Denominator/fi.Numerator))
	}
	return out, nil
}
