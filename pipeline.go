package main

// The SRT and HDMI outputs are built on the device's own GStreamer, which
// turned out to carry exactly the elements this needs:
//
//   mpph264enc   the VEPU H.264 encoder
//   mppjpegdec   hardware JPEG decode, so an MJPEG camera costs no CPU
//   kmssink      straight to DRM/KMS, i.e. HDMI, with no DRM code of our own
//
// Encoded H.264 comes back to us over a pipe as an Annex-B byte stream, gets
// split into access units and muxed to MPEG-TS here, because the device has no
// muxer at all.
//
// Two constraints worth knowing before reading further, both measured on the
// device rather than assumed:
//
//   * mpph264enc in this build has NO tunable properties — no bitrate, no GOP,
//     no rate-control mode. It derives a bitrate from the caps (3.456 Mbps at
//     720p30). Exposing a bitrate control means replacing this element.
//   * its sink caps stop at 1920x1088 and 60/1, so 1080p is the ceiling here
//     regardless of what the camera can produce.

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

type Outputs struct {
	NDI  bool
	SRT  bool
	HDMI bool
}

func (o Outputs) any() bool { return o.NDI || o.SRT || o.HDMI }

func (o Outputs) String() string {
	var p []string
	if o.NDI {
		p = append(p, "ndi")
	}
	if o.SRT {
		p = append(p, "srt")
	}
	if o.HDMI {
		p = append(p, "hdmi")
	}
	if len(p) == 0 {
		return "none"
	}
	return strings.Join(p, "+")
}

func parseOutputs(s string) (Outputs, error) {
	var o Outputs
	for _, part := range strings.Split(strings.ToLower(s), ",") {
		switch strings.TrimSpace(part) {
		case "", "none":
		case "ndi":
			o.NDI = true
		case "srt":
			o.SRT = true
		case "hdmi":
			o.HDMI = true
		default:
			return o, fmt.Errorf("unknown output %q (want ndi, srt or hdmi)", part)
		}
	}
	if !o.any() {
		return o, fmt.Errorf("no outputs selected")
	}
	// One process cannot open the camera twice, and the NDI path does its own
	// V4L2 capture while SRT and HDMI are fed by GStreamer. Combining them
	// needs a single owner of the device — see the README roadmap.
	if o.NDI && (o.SRT || o.HDMI) {
		return o, fmt.Errorf("ndi cannot be combined with srt or hdmi yet: both want to own the camera")
	}
	return o, nil
}

type PipelineConfig struct {
	Device      string
	Width       int
	Height      int
	FPS         int
	Pixfmt      uint32
	Out         Outputs
	ConnectorID int
	Synthetic   bool
	// SoftwareJPEG forces the CPU JPEG decoder. The VPU's decoder aborts the
	// process on anything that is not 4:2:0, so this is not optional for a
	// 4:2:2 camera — see jpegprobe.go.
	SoftwareJPEG bool
}

// gstArgs builds the gst-launch-1.0 argument list. Kept as a pure function so
// the shape of the pipeline is testable without a camera or a device.
func gstArgs(c PipelineConfig) ([]string, error) {
	if !c.Out.SRT && !c.Out.HDMI {
		return nil, fmt.Errorf("pipeline needs srt or hdmi")
	}
	if c.Width > 1920 || c.Height > 1088 {
		return nil, fmt.Errorf("mpph264enc caps stop at 1920x1088, asked for %dx%d", c.Width, c.Height)
	}

	var p []string
	if c.Synthetic {
		// Same escape hatch as the NDI path: exercise encode and transport
		// with no camera attached. is-live paces it to the frame rate so the
		// bitrate figures mean something.
		p = append(p, "videotestsrc", "is-live=true", "pattern=smpte")
		p = append(p, "!", fmt.Sprintf("video/x-raw,format=NV12,width=%d,height=%d,framerate=%d/1", c.Width, c.Height, c.FPS))
		return append(p, encodeAndDisplay(c)...), nil
	}
	p = append(p, "v4l2src", fmt.Sprintf("device=%s", c.Device))

	switch c.Pixfmt {
	case pixMJPG:
		p = append(p, "!", fmt.Sprintf("image/jpeg,width=%d,height=%d,framerate=%d/1", c.Width, c.Height, c.FPS))
		if c.SoftwareJPEG {
			// Costs roughly a core at 1080p, but mppjpegdec cannot do this
			// camera's chroma format and crashes rather than declining.
			p = append(p, "!", "jpegdec")
		} else {
			// Hardware JPEG decode — this is why an MJPEG camera is affordable.
			p = append(p, "!", "mppjpegdec")
		}
	case pixNV12:
		p = append(p, "!", fmt.Sprintf("video/x-raw,format=NV12,width=%d,height=%d,framerate=%d/1", c.Width, c.Height, c.FPS))
	case pixYUYV, pixUYVY:
		name := "YUY2"
		if c.Pixfmt == pixUYVY {
			name = "UYVY"
		}
		p = append(p, "!", fmt.Sprintf("video/x-raw,format=%s,width=%d,height=%d,framerate=%d/1", name, c.Width, c.Height, c.FPS))
		// The encoder wants NV12 or I420; this conversion is on the CPU and is
		// the expensive step in the whole pipeline. Prefer a camera that can
		// give NV12 or MJPEG directly.
		p = append(p, "!", "videoconvert", "!", "video/x-raw,format=NV12")
	default:
		return nil, fmt.Errorf("no pipeline for capture format %s", fourCCName(c.Pixfmt))
	}

	return append(p, encodeAndDisplay(c)...), nil
}

// encodeAndDisplay is the tail of the pipeline: the encoder branch, the HDMI
// branch, or a tee feeding both.
func encodeAndDisplay(c PipelineConfig) []string {
	var p []string
	encode := []string{
		"!", "queue", "max-size-buffers=3", "leaky=downstream",
		"!", "mpph264enc",
		"!", "h264parse", "config-interval=1",
		"!", "video/x-h264,stream-format=byte-stream,alignment=au",
		"!", "fdsink", "fd=1", "sync=false",
	}
	// The VOP scans out NV12; jpegdec hands back I420 or, from a 4:2:2 source,
	// Y42B, and kmssink takes neither. Without this the whole pipeline fails
	// negotiation right back at the source with a bare "not-negotiated".
	display := []string{
		"!", "queue", "max-size-buffers=3", "leaky=downstream",
		"!", "videoconvert",
		"!", "video/x-raw,format=NV12",
		"!", "kmssink", "force-modesetting=true",
	}
	if c.ConnectorID > 0 {
		display = append(display, fmt.Sprintf("connector-id=%d", c.ConnectorID))
	}

	switch {
	case c.Out.SRT && c.Out.HDMI:
		p = append(p, "!", "tee", "name=t")
		p = append(p, "t.")
		p = append(p, encode...)
		p = append(p, "t.")
		p = append(p, display...)
	case c.Out.SRT:
		p = append(p, encode...)
	case c.Out.HDMI:
		p = append(p, display...)
	}
	return p
}

type Pipeline struct {
	cmd    *exec.Cmd
	Stdout io.ReadCloser

	// A duration timer, a signal and the main path can all end up here at
	// once. Waiting twice on the same child returns "waitid: no child
	// processes", which then reads as a crash on a perfectly clean stop.
	waitOnce sync.Once
	waitErr  error
	killed   bool
}

// StartPipeline launches gst-launch-1.0. Its stderr is relayed into our log
// with a prefix, because when a pipeline fails to negotiate caps that message
// is the only thing that explains why.
func StartPipeline(args []string) (*Pipeline, error) {
	logf("pipeline: gst-launch-1.0 %s", strings.Join(args, " "))
	cmd := exec.Command("gst-launch-1.0", append([]string{"-q"}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start gst-launch-1.0 (is gstreamer installed?): %w", err)
	}
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logf("gst: %s", sc.Text())
		}
	}()
	return &Pipeline{cmd: cmd, Stdout: stdout}, nil
}

func (p *Pipeline) Close() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	p.killed = true
	_ = p.cmd.Process.Kill()
	return p.Wait()
}

// Wait is safe to call from several places and reports a deliberate kill as a
// clean stop rather than an error.
func (p *Pipeline) Wait() error {
	if p == nil || p.cmd == nil {
		return nil
	}
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	if p.killed {
		return nil
	}
	return p.waitErr
}
