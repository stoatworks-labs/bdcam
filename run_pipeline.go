package main

// The SRT / HDMI run loop: GStreamer owns the camera and the hardware blocks,
// we own the transport. See pipeline.go for why it is split that way.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func runPipeline(cfg runConfig) error {
	var dev string
	var pixfmt uint32
	var softwareJPEG bool
	if cfg.synthetic {
		logf("source: synthetic %dx%d@%d -> %s", cfg.width, cfg.height, cfg.fps, cfg.out)
	} else {
		dev = cfg.device
		if dev == "" || !IsCaptureDevice(dev) {
			EnsureUVCBound()
			devs := findVideoDevices()
			if len(devs) == 0 {
				logf("no /dev/video* capture device found — nothing to do")
				os.Exit(0)
			}
			if dev != "" {
				logf("configured device %s is not present; using %s", dev, devs[0])
			}
			dev = devs[0]
		}
		var err error
		pixfmt, err = parseFormat(cfg.format)
		if err != nil {
			return err
		}
		if pixfmt == 0 {
			pixfmt, err = pickPipelineFormat(dev)
			if err != nil {
				return err
			}
		}
		// A rate the camera does not offer fails caps negotiation with an
		// opaque "not-negotiated", so snap to something it actually supports.
		if rates, err := SupportedRates(dev, pixfmt, cfg.width, cfg.height); err == nil && len(rates) > 0 {
			if best := nearestRate(rates, cfg.fps); best != cfg.fps {
				logf("camera does not offer %d fps at %dx%d (it offers %v) — using %d",
					cfg.fps, cfg.width, cfg.height, rates, best)
				cfg.fps = best
			}
		}

		// The VPU JPEG decoder aborts the process on anything but 4:2:0, so
		// find out before building a pipeline around it.
		if pixfmt == pixMJPG {
			if chroma, err := probeCameraChroma(dev, cfg.width, cfg.height, cfg.fps); err != nil {
				logf("could not read the camera's JPEG chroma (%v) — assuming software decode", err)
				softwareJPEG = true
			} else if !chroma.HardwareDecodable() {
				logf("camera emits %s MJPEG; the VPU decoder only handles 4:2:0, so decoding on the CPU (about a core at 1080p)", chroma)
				softwareJPEG = true
			} else {
				logf("camera emits %s MJPEG — hardware decode", chroma)
			}
		}

		logf("source: %s %dx%d@%d %s -> %s", dev, cfg.width, cfg.height, cfg.fps, fourCCName(pixfmt), cfg.out)
	}

	directHDMI := cfg.out.HDMI && cfg.hdmiMode != "decoder"
	// NDI|HX takes its frames from the encoder branch, the same one SRT uses,
	// so the VEPU compresses once and both consumers share the result.
	hx := cfg.out.NDI && cfg.ndiFormat == "h264"
	wantRaw := (cfg.out.NDI && !hx) || directHDMI
	wantEncode := cfg.out.SRT || hx

	// kmssink cannot set a mode while PPApp is DRM master, so taking the
	// display means stopping it. Warning about it is no use from a web page,
	// where there is no shell to run systemctl in.
	if directHDMI {
		restore := takeDisplay()
		defer restore()
	}

	// H.264 codes in 16-row macroblocks. At 1080 the decoder pads to 1088 and
	// the eight extra rows arrive uninitialised — a green band along the bottom
	// on any receiver that does not honour the SPS crop, which the PLAY's own
	// decoder does not. Trim to a whole number of macroblocks instead.
	cropBottom := 0
	if wantEncode {
		if rem := cfg.height % 16; rem != 0 {
			cropBottom = rem
			logf("cropping %d rows: %d is not a multiple of 16, and the padding shows as a green band on decoders that ignore the SPS crop — encoding %dx%d",
				rem, cfg.height, cfg.width, cfg.height-rem)
		}
	}
	// The camera is still asked for its own geometry; only what comes out of
	// the decoder is trimmed. Asking v4l2src for a size it does not offer fails
	// negotiation outright.
	sourceHeight := cfg.height

	args, err := gstArgs(PipelineConfig{
		CropBottom:   cropBottom,
		Encode:       wantEncode,
		Raw:          wantRaw,
		Synthetic:    cfg.synthetic,
		Device:       dev,
		Width:        cfg.width,
		Height:       sourceHeight,
		FPS:          cfg.fps,
		Pixfmt:       pixfmt,
		Out:          pipelineOutputs(cfg),
		ConnectorID:  cfg.connector,
		SoftwareJPEG: softwareJPEG,
	})
	if err != nil {
		return err
	}

	// Everything downstream — NDI frame geometry, the display sink — works in
	// the cropped height.
	cfg.height -= cropBottom

	pl, err := StartPipeline(args)
	if err != nil {
		return err
	}
	defer pl.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sig
		logf("caught %v, stopping pipeline", s)
		_ = pl.Close()
	}()

	// Nothing to read back at all: GStreamer is doing all the work.
	if !wantEncode && !wantRaw {
		logf("hdmi output running; nothing to read back")
		if cfg.duration > 0 {
			go func() {
				time.Sleep(cfg.duration)
				logf("duration reached, stopping")
				_ = pl.Close()
			}()
		}
		return pl.Wait()
	}

	// One reader takes the raw frames and feeds whichever consumers are on,
	// in its own goroutine so it runs alongside SRT rather than instead of it.
	var rawDone chan struct{}
	if wantRaw {
		var display *DisplaySink
		if directHDMI {
			display, err = StartDisplaySink(cfg.width, cfg.height, cfg.fps, cfg.connector)
			if err != nil {
				return err
			}
			defer display.Close()
		}
		rawDone = make(chan struct{})
		go func() {
			defer close(rawDone)
			if err := pumpRaw(cfg, pl.Raw, display); err != nil {
				logf("raw output stopped: %v", err)
			}
		}()
		if !wantEncode {
			// Nothing to read on stdout; wait for the pump or the pipeline.
			if cfg.duration > 0 {
				go func() {
					time.Sleep(cfg.duration)
					logf("duration reached, stopping")
					_ = pl.Close()
				}()
			}
			<-rawDone
			return pl.Wait()
		}
	}

	var sender *SRTSender
	var mux *TSMuxer
	if cfg.out.SRT {
		sender, err = DialSRT(cfg.srtURL)
		if err != nil {
			return err
		}
		defer sender.Close()
		mux = NewTSMuxer(sender)
	}

	// NDI|HX shares these access units with SRT rather than encoding twice.
	var hxSend *Sender
	var hxBuf []byte
	if hx {
		ndi, err := LoadNDI(cfg.ndiLib)
		if err != nil {
			return err
		}
		defer ndi.Close()
		hxSend, err = ndi.NewSender(cfg.name, cfg.clock)
		if err != nil {
			return err
		}
		defer hxSend.Close()
		hxBuf = make([]byte, CompressedHeaderSize()+maxAUBytes)
		logf("sending as %q (NDI|HX, H.264 from the VEPU, %d/1 fps declared)", hxSend.SourceName(), cfg.fps)
		if cfg.out.HDMI && cfg.hdmiMode == "decoder" {
			restore := pointDecoderAt(hxSend.SourceName(), hxSend.SourceURL())
			defer restore()
		}
	}
	var (
		frames   int
		bytesOut int64
		keyfr    int
		start    = time.Now()
		last     = start
		atLast   int
		byteLast int64
		deadline time.Time
	)
	if cfg.duration > 0 {
		deadline = start.Add(cfg.duration)
		logf("will stop after %s", cfg.duration)
	}

	sc := NewAUScanner(pl.Stdout)
	err = sc.Scan(func(au []byte, key bool) error {
		// The encoder gives us no timestamps over a pipe, so the frame index
		// against the declared rate is the clock. That is exact for a camera
		// running at its nominal rate, which is the case we build for.
		pts := int64(frames) * 90000 / int64(cfg.fps)
		if mux != nil {
			if err := mux.WriteAccessUnit(au, pts, key); err != nil {
				return err
			}
		}
		if hxSend != nil {
			hxSend.SendCompressed(hxBuf, au, pts, key, cfg.fps, 1, cfg.width, cfg.height)
		}
		frames++
		bytesOut += int64(len(au))
		if key {
			keyfr++
		}

		now := time.Now()
		if now.Sub(last) >= cfg.statsEach {
			w := now.Sub(last).Seconds()
			fps := float64(frames-atLast) / w
			mbps := float64(bytesOut-byteLast) * 8 / w / 1e6
			conns := 0
			if hxSend != nil {
				conns = hxSend.Connections()
			}
			logf("h264: frames=%d fps=%.2f bitrate=%.2f Mbps keyframes=%d conns=%d", frames, fps, mbps, keyfr, conns)
			last, atLast, byteLast = now, frames, bytesOut
		}
		if !deadline.IsZero() && now.After(deadline) {
			logf("duration reached, stopping")
			return errStop
		}
		return nil
	})
	if err == errStop {
		err = nil
	}
	el := time.Since(start).Seconds()
	logf("stopped: %d frames in %.1fs (avg %.2f fps, %.2f Mbps, %d keyframes)",
		frames, el, float64(frames)/el, float64(bytesOut)*8/el/1e6, keyfr)
	return err
}

// pipelineOutputs drops HDMI from the GStreamer graph when the decoder is doing
// the display: there is no kmssink branch in that mode, only the NDI feed the
// decoder receives.
func pipelineOutputs(cfg runConfig) Outputs {
	o := cfg.out
	o.HDMI = false // the display is fed from our side, never by the capture pipeline
	return o
}

var errStop = fmt.Errorf("stop requested")

// nearestRate picks the supported frame rate closest to what was asked for,
// preferring the higher one when a request sits exactly between two.
func nearestRate(rates []int, want int) int {
	best := rates[0]
	for _, r := range rates {
		if d, bd := abs(r-want), abs(best-want); d < bd || (d == bd && r > best) {
			best = r
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pumpRaw reads whole I420 frames from the pipeline and feeds them to whichever
// consumers are enabled. The frames are fixed size, so the framing is just
// "read exactly this many bytes" — no parsing.
//
// Both conversions live here rather than in GStreamer because its NV12 and UYVY
// paths are the slow generic ones on this hardware; from I420 they are a copy
// and an interleave.
func pumpRaw(cfg runConfig, r io.Reader, display *DisplaySink) error {
	frameSize := cfg.width * cfg.height * 3 / 2
	buf := make([]byte, frameSize)

	var send *Sender
	var packed []byte
	var pack func() error
	var f Frame

	if cfg.out.NDI {
		ndi, err := LoadNDI(cfg.ndiLib)
		if err != nil {
			return err
		}
		defer ndi.Close()
		send, err = ndi.NewSender(cfg.name, cfg.clock)
		if err != nil {
			return err
		}
		defer send.Close()

		f = Frame{Width: cfg.width, Height: cfg.height}
		switch cfg.ndiFormat {
		case "nv12":
			packed = make([]byte, frameSize)
			f.FourCC, f.Stride, f.Data = fourCCNV12, cfg.width, packed
			pack = func() error { return i420ToNV12(buf, packed, cfg.width, cfg.height) }
		default:
			packed = make([]byte, cfg.width*cfg.height*2)
			f.FourCC, f.Stride, f.Data = fourCCUYVY, cfg.width*2, packed
			pack = func() error { return i420ToUYVY(buf, packed, cfg.width, cfg.height) }
		}
		logf("sending as %q (from the shared capture, %d/1 fps declared, %s)",
			send.SourceName(), cfg.fps, fourCCName(f.FourCC))

		if cfg.out.HDMI && cfg.hdmiMode == "decoder" {
			restore := pointDecoderAt(send.SourceName(), send.SourceURL())
			defer restore()
		}
	}

	// The display sink wants NV12, which may or may not be what NDI asked for.
	// When it is, the same buffer serves both and the frame is packed once.
	var nv12 []byte
	shareWithNDI := false
	if display != nil {
		if packed != nil && f.FourCC == fourCCNV12 {
			nv12, shareWithNDI = packed, true
		} else {
			nv12 = make([]byte, frameSize)
		}
	}

	var frames, dropped int
	start := time.Now()
	last := start
	atLast := 0
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				logf("raw frame pipe closed after %d frames", frames)
				return nil
			}
			return err
		}
		if send != nil {
			if err := pack(); err != nil {
				return err
			}
			send.SendVideo(&f, cfg.fps, 1)
		}
		if display != nil {
			if !shareWithNDI {
				if err := i420ToNV12(buf, nv12, cfg.width, cfg.height); err != nil {
					return err
				}
			}
			if _, err := display.Write(nv12); err != nil {
				// A dead display sink should not take the whole converter down.
				dropped++
				if dropped == 1 {
					logf("display sink stopped accepting frames: %v", err)
				}
				display = nil
			}
		}
		frames++
		if now := time.Now(); now.Sub(last) >= cfg.statsEach {
			w := now.Sub(last).Seconds()
			conns := 0
			if send != nil {
				conns = send.Connections()
			}
			logf("raw: frames=%d fps=%.2f conns=%d", frames, float64(frames-atLast)/w, conns)
			last, atLast = now, frames
		}
	}
}

// pickPipelineFormat prefers what costs least downstream. Unlike the NDI path
// this ranks NV12 first (the encoder takes it directly) and MJPEG second (the
// hardware decodes it), leaving the packed 4:2:2 formats last because they
// force a CPU videoconvert.
func pickPipelineFormat(dev string) (uint32, error) {
	have, err := EnumFormats(dev)
	if err != nil {
		return 0, err
	}
	set := map[uint32]bool{}
	for _, f := range have {
		set[f] = true
	}
	for _, want := range []uint32{pixNV12, pixMJPG, pixYUYV, pixUYVY} {
		if set[want] {
			return want, nil
		}
	}
	names := make([]string, len(have))
	for i, f := range have {
		names[i] = fourCCName(f)
	}
	return 0, fmt.Errorf("camera offers no usable format for the pipeline: %v", names)
}

// takeDisplay stops BirdDogRunner so kmssink can become DRM master on card0,
// and returns a function that puts it back. PPApp does not share the display,
// so this is the price of HDMI output — the normal decoder output stops for as
// long as the converter is running.
//
// The restore runs on every ordinary exit including SIGTERM, so switching the
// converter off through the web UI hands the display back. It cannot run if the
// process is killed outright, so the unit also restarts BirdDogRunner in
// ExecStopPost.
func takeDisplay() func() {
	out, _ := exec.Command("systemctl", "is-active", "BirdDogRunner").Output()
	if strings.TrimSpace(string(out)) != "active" {
		logf("BirdDogRunner is not running; taking the display without stopping anything")
		return func() {}
	}
	logf("stopping BirdDogRunner to take DRM master — the normal PLAY output stops until the converter is switched off")
	if err := exec.Command("systemctl", "stop", "BirdDogRunner").Run(); err != nil {
		logf("WARNING: could not stop BirdDogRunner (%v); kmssink will not get the display", err)
		return func() {}
	}
	// A marker so the unit can put the display back if we are killed outright
	// and never reach the restore below. It is deliberately specific: without
	// it an ExecStopPost would fire on every restart of a disabled service and
	// could fight someone who stopped BirdDogRunner on purpose.
	if err := os.WriteFile(displayTakenMarker, []byte("bdcam\n"), 0o644); err != nil {
		logf("note: could not write %s (%v); a hard kill will leave the display with the converter", displayTakenMarker, err)
	}
	// Give PPApp a moment to release card0 before kmssink asks for it.
	time.Sleep(1500 * time.Millisecond)
	return func() {
		logf("restoring BirdDogRunner")
		if err := exec.Command("systemctl", "start", "BirdDogRunner").Run(); err != nil {
			logf("WARNING: could not restart BirdDogRunner (%v) — HDMI stays dark until it is started", err)
		}
		_ = os.Remove(displayTakenMarker)
	}
}

// displayTakenMarker lives on tmpfs, so a reboot clears it — which is right,
// since BirdDogRunner starts normally on boot anyway.
const displayTakenMarker = "/run/bdcam-took-display"
