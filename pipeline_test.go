package main

import (
	"strings"
	"testing"
)

func TestParseOutputs(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
		err  bool
	}{
		{"ndi", "ndi", false},
		{"srt", "srt", false},
		{"hdmi", "hdmi", false},
		{"srt,hdmi", "srt+hdmi", false},
		{"SRT, HDMI", "srt+hdmi", false},
		{"ndi,srt", "ndi+srt", false},   // one capture, tee'd
		{"ndi,hdmi", "ndi+hdmi", false}, // the combination worth having
		{"ndi,srt,hdmi", "ndi+srt+hdmi", false},
		{"rtmp", "", true},
		{"", "", true},
	} {
		got, err := parseOutputs(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseOutputs(%q) should have failed", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseOutputs(%q): %v", c.in, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("parseOutputs(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func args(t *testing.T, c PipelineConfig) string {
	t.Helper()
	a, err := gstArgs(c)
	if err != nil {
		t.Fatalf("gstArgs: %v", err)
	}
	return strings.Join(a, " ")
}

func base() PipelineConfig {
	return PipelineConfig{Device: "/dev/video0", Width: 1280, Height: 720, FPS: 30,
		Pixfmt: pixNV12, Out: Outputs{SRT: true}}
}

// raw returns a config whose only branch is the raw one, as NDI or the display
// sink would ask for.
func rawOnly() PipelineConfig {
	c := base()
	c.Out = Outputs{}
	c.Raw = true
	return c
}

func TestMJPEGUsesHardwareDecode(t *testing.T) {
	c := base()
	c.Pixfmt = pixMJPG
	got := args(t, c)
	if !strings.Contains(got, "mppjpegdec") {
		t.Errorf("MJPEG should decode on the VPU, got: %s", got)
	}
	if strings.Contains(got, "videoconvert") {
		t.Errorf("MJPEG path should not need a CPU convert, got: %s", got)
	}
}

func TestNV12GoesStraightToEncoder(t *testing.T) {
	got := args(t, base())
	if strings.Contains(got, "videoconvert") || strings.Contains(got, "mppjpegdec") {
		t.Errorf("NV12 needs no conversion at all, got: %s", got)
	}
	if !strings.Contains(got, "mpph264enc") {
		t.Errorf("no encoder in the pipeline: %s", got)
	}
}

func TestPackedFormatsNeedConvert(t *testing.T) {
	for _, f := range []uint32{pixYUYV, pixUYVY} {
		c := base()
		c.Pixfmt = f
		got := args(t, c)
		if !strings.Contains(got, "videoconvert") {
			t.Errorf("%s must be converted for the encoder, got: %s", fourCCName(f), got)
		}
		if !strings.Contains(got, "format=NV12") {
			t.Errorf("%s should convert to NV12, got: %s", fourCCName(f), got)
		}
	}
}

func TestSRTBranchIsByteStreamAU(t *testing.T) {
	got := args(t, base())
	// The scanner splits on access unit delimiters, and config-interval=1 is
	// what repeats SPS/PPS so a receiver can join mid-stream.
	for _, want := range []string{"h264parse", "config-interval=1", "stream-format=byte-stream", "alignment=au", "fdsink"} {
		if !strings.Contains(got, want) {
			t.Errorf("SRT branch missing %q: %s", want, got)
		}
	}
}

func TestCombinedOutputsTee(t *testing.T) {
	c := base()
	c.Out = Outputs{SRT: true}
	c.Raw = true
	got := args(t, c)
	if !strings.Contains(got, "tee name=t") {
		t.Errorf("combined outputs need a tee: %s", got)
	}
	if strings.Count(got, "t.") < 2 {
		t.Errorf("tee should feed two branches: %s", got)
	}
	if !strings.Contains(got, "mpph264enc") || !strings.Contains(got, "format=I420") {
		t.Errorf("both branches should be present: %s", got)
	}
}

// kmssink lives in its own process now, fed NV12 we pack ourselves, because
// GStreamer's conversion to NV12 runs at 1.8 fps here against 24 fps to I420.
func TestMainPipelineHasNoDisplayBranch(t *testing.T) {
	got := args(t, rawOnly())
	if strings.Contains(got, "kmssink") {
		t.Errorf("kmssink belongs in the display process, not the capture pipeline: %s", got)
	}
	if !strings.Contains(got, "format=I420") {
		t.Errorf("the raw branch should stop at I420: %s", got)
	}
}

// The whole point of routing NDI through GStreamer: one camera, several
// outputs, which the direct V4L2 path could never do.
func TestAllThreeOutputs(t *testing.T) {
	c := base()
	c.Out = Outputs{NDI: true, SRT: true, HDMI: true}
	c.Raw = true
	got := args(t, c)
	// Two branches, not three: SRT's encoder, and one raw branch feeding both
	// NDI and the display.
	if strings.Count(got, "t.") != 2 {
		t.Errorf("expected two tee branches: %s", got)
	}
	// SRT keeps stdout, NDI has fd 3 — they must not share a descriptor.
	if !strings.Contains(got, "fd=1") || !strings.Contains(got, "fd=3") {
		t.Errorf("SRT and NDI need separate pipes: %s", got)
	}
}

func TestNDIAloneNeedsNoTee(t *testing.T) {
	c := rawOnly()
	got := args(t, c)
	if strings.Contains(got, "tee") {
		t.Errorf("a single output should not be tee'd: %s", got)
	}
	if !strings.Contains(got, "format=I420") {
		t.Errorf("NDI branch should emit I420: %s", got)
	}
}

func TestHDMIOnlyHasNoEncoder(t *testing.T) {
	got := args(t, rawOnly())
	if strings.Contains(got, "mpph264enc") {
		t.Errorf("HDMI alone should not encode: %s", got)
	}
}

// The encoder element's sink caps stop here; better a clear error than a caps
// negotiation failure buried in GStreamer's output.
func TestEncoderResolutionCeiling(t *testing.T) {
	c := base()
	c.Width, c.Height = 3840, 2160
	if _, err := gstArgs(c); err == nil {
		t.Fatal("4K should be rejected: mpph264enc caps stop at 1920x1088")
	}
	c.Width, c.Height = 1920, 1080
	if _, err := gstArgs(c); err != nil {
		t.Fatalf("1080p should be allowed: %v", err)
	}
}

func TestSyntheticNeedsNoCamera(t *testing.T) {
	c := base()
	c.Raw = true
	c.Synthetic = true
	got := args(t, c)
	if strings.Contains(got, "v4l2src") {
		t.Errorf("synthetic mode must not open a camera: %s", got)
	}
	if !strings.Contains(got, "videotestsrc") || !strings.Contains(got, "mpph264enc") {
		t.Errorf("synthetic mode should still exercise the encoder: %s", got)
	}
}

func TestNoOutputsIsAnError(t *testing.T) {
	c := base()
	c.Out = Outputs{}
	c.Raw = false
	if _, err := gstArgs(c); err == nil {
		t.Fatal("a pipeline with no outputs should be rejected")
	}
}
