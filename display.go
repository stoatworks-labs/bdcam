package main

// Driving HDMI without paying for GStreamer's NV12 conversion.
//
// kmssink is the only way to the panel and it accepts NV12 only — it advertises
// I420 and Y42B in its caps, but both fail negotiation on this hardware.
// Unfortunately videoconvert's path to NV12 is the slow generic one: measured
// at 1.8 fps for 1080p on this device, against 24 fps to I420. Chaining
// Y42B -> I420 -> NV12 does not help, because the second hop is the slow one.
//
// So the pipeline stops at I420, which is cheap, and the pack to NV12 happens
// in Go, where it is a luma copy plus an interleave. The frames are then fed to
// a second GStreamer process that does nothing but scan out.

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

type DisplaySink struct {
	cmd *exec.Cmd
	in  io.WriteCloser

	once sync.Once
	err  error
}

// StartDisplaySink launches the scan-out pipeline. blocksize is set to exactly
// one frame so fdsrc hands whole frames downstream rather than arbitrary reads,
// and sync is off because raw frames off a pipe carry no timestamps to sync to.
func StartDisplaySink(width, height, fps, connector int) (*DisplaySink, error) {
	frame := width * height * 3 / 2
	// rawvideoparse, not a bare capsfilter. Bytes off a pipe carry no frame
	// structure, and fdsrc with caps alone fails to negotiate — "Internal data
	// stream error", then "pipeline doesn't want to preroll". rawvideoparse is
	// what turns the stream back into frames.
	//
	// It goes straight to kmssink with no conversion: the frames are already
	// NV12, which is the one format the sink accepts here.
	args := []string{
		"-q",
		"fdsrc", "fd=0", fmt.Sprintf("blocksize=%d", frame),
		"!", "rawvideoparse", "format=nv12",
		fmt.Sprintf("width=%d", width), fmt.Sprintf("height=%d", height),
		fmt.Sprintf("framerate=%d/1", fps),
		"!", "queue", "max-size-buffers=3", "leaky=downstream",
		"!", "kmssink", "force-modesetting=true", "sync=false",
	}
	if connector > 0 {
		args = append(args, fmt.Sprintf("connector-id=%d", connector))
	}
	logf("display: gst-launch-1.0 %s", strings.Join(args[1:], " "))

	cmd := exec.Command("gst-launch-1.0", args...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start display pipeline: %w", err)
	}
	go relayLines("display", stderr)
	return &DisplaySink{cmd: cmd, in: in}, nil
}

func (d *DisplaySink) Write(p []byte) (int, error) {
	if d == nil {
		return len(p), nil
	}
	return d.in.Write(p)
}

func (d *DisplaySink) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		_ = d.in.Close()
		if d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
		}
		_ = d.cmd.Wait()
	})
	return d.err
}
