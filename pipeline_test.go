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
		Pixfmt: pixNV12, Out: Outputs{SRT: true}, Encode: true}
}

// raw returns a config whose only branch is the raw one, as NDI or the display
// sink would ask for.
func rawOnly() PipelineConfig {
	c := base()
	c.Out = Outputs{}
	c.Encode = false
	c.Raw = true
	return c
}

func TestMJPEGUsesHardwareDecodeWhenItCan(t *testing.T) {
	c := base()
	c.Pixfmt = pixMJPG
	got := args(t, c)
	if !strings.Contains(got, "mppjpegdec") {
		t.Errorf("MJPEG should decode on the VPU when the chroma allows, got: %s", got)
	}
	c.SoftwareJPEG = true
	got = args(t, c)
	if !strings.Contains(got, "jpegdec") || strings.Contains(got, "mppjpegdec") {
		t.Errorf("a 4:2:2 camera must fall back to software decode, got: %s", got)
	}
}

// Every source is normalised to I420 once, before the tee, so the conversion
// is paid for a single time however many outputs are on.
func TestOneConversionBeforeTheTee(t *testing.T) {
	for _, f := range []uint32{pixNV12, pixYUYV, pixUYVY, pixMJPG} {
		c := base()
		c.Pixfmt = f
		c.Raw = true
		got := args(t, c)
		if n := strings.Count(got, "videoconvert"); n != 1 {
			t.Errorf("%s: expected exactly one conversion, got %d: %s", fourCCName(f), n, got)
		}
		if strings.Index(got, "videoconvert") > strings.Index(got, "tee") {
			t.Errorf("%s: the conversion must come before the tee: %s", fourCCName(f), got)
		}
	}
}

func TestPackedFormatsAreNormalised(t *testing.T) {
	for _, f := range []uint32{pixYUYV, pixUYVY} {
		c := base()
		c.Pixfmt = f
		got := args(t, c)
		if !strings.Contains(got, "format=I420") {
			t.Errorf("%s should be normalised to I420, got: %s", fourCCName(f), got)
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

// NDI|HX and SRT both consume the encoder branch, so the VEPU compresses once.
func TestHXSharesTheEncoderBranch(t *testing.T) {
	c := base()
	c.Out = Outputs{NDI: true, SRT: true}
	got := args(t, c)
	if strings.Count(got, "mpph264enc") != 1 {
		t.Errorf("the encoder should appear once, not per consumer: %s", got)
	}
	if strings.Contains(got, "tee") {
		t.Errorf("one branch serves both, so no tee: %s", got)
	}
}

func TestNoOutputsIsAnError(t *testing.T) {
	c := base()
	c.Out = Outputs{}
	c.Encode = false
	c.Raw = false
	if _, err := gstArgs(c); err == nil {
		t.Fatal("a pipeline with no outputs should be rejected")
	}
}

// 1080 is not a multiple of 16, so H.264 pads it to 1088 and the extra rows
// arrive uninitialised — the green band. Trim rather than rely on the SPS crop
// being honoured.
func TestCropsToWholeMacroblocks(t *testing.T) {
	c := base()
	c.Height, c.CropBottom = 1080, 8
	got := args(t, c)
	if !strings.Contains(got, "videocrop bottom=8") {
		t.Errorf("expected a crop before the encoder: %s", got)
	}
	// videocrop cannot handle jpegdec's planar 4:2:2, so the conversion has to
	// come first or the whole pipeline fails to negotiate.
	if strings.Index(got, "videoconvert") > strings.Index(got, "videocrop") {
		t.Errorf("the conversion must precede the crop: %s", got)
	}
	if strings.Index(got, "videocrop") > strings.Index(got, "mpph264enc") {
		t.Errorf("the crop must come before the encoder: %s", got)
	}
	// A height already on a macroblock boundary needs no crop at all.
	c.Height, c.CropBottom = 720, 0
	if strings.Contains(args(t, c), "videocrop") {
		t.Error("720 is a multiple of 16 and should not be cropped")
	}
}

// The camera keeps its own geometry; only the decoded frames are trimmed.
// Asking v4l2src for a height it does not offer fails negotiation outright.
func TestCropDoesNotChangeSourceCaps(t *testing.T) {
	c := base()
	c.Pixfmt = pixMJPG
	c.Height, c.CropBottom = 1080, 8
	got := args(t, c)
	if !strings.Contains(got, "height=1080") {
		t.Errorf("the source must still be asked for 1080: %s", got)
	}
	if strings.Contains(got, "height=1072") {
		t.Errorf("the crop must not reach the source caps: %s", got)
	}
}
