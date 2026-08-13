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

	// kmssink cannot set a mode while PPApp is DRM master, so taking the
	// display means stopping it. Warning about it is no use from a web page,
	// where there is no shell to run systemctl in.
	if cfg.out.HDMI && cfg.hdmiMode == "direct" {
		restore := takeDisplay()
		defer restore()
	}

	args, err := gstArgs(PipelineConfig{
		Synthetic:    cfg.synthetic,
		Device:       dev,
		Width:        cfg.width,
		Height:       cfg.height,
		FPS:          cfg.fps,
		Pixfmt:       pixfmt,
		Out:          pipelineOutputs(cfg),
		ConnectorID:  cfg.connector,
		SoftwareJPEG: softwareJPEG,
	})
	if err != nil {
		return err
	}

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

	// HDMI alone means nothing comes back to us — GStreamer is doing all of the
	// work and we are only supervising it.
	if !cfg.out.SRT && !cfg.out.NDI {
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

	// NDI reads raw UYVY off the second pipe, in its own goroutine so it runs
	// alongside SRT rather than instead of it.
	var ndiDone chan struct{}
	if cfg.out.NDI {
		ndiDone = make(chan struct{})
		go func() {
			defer close(ndiDone)
			if err := pumpNDI(cfg, pl.Raw); err != nil {
				logf("NDI output stopped: %v", err)
			}
		}()
		if !cfg.out.SRT {
			// Nothing to read on stdout; wait for the NDI pump or the pipeline.
			if cfg.duration > 0 {
				go func() {
					time.Sleep(cfg.duration)
					logf("duration reached, stopping")
					_ = pl.Close()
				}()
			}
			<-ndiDone
			return pl.Wait()
		}
	}

	sender, err := DialSRT(cfg.srtURL)
	if err != nil {
		return err
	}
	defer sender.Close()

	mux := NewTSMuxer(sender)
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
		if err := mux.WriteAccessUnit(au, pts, key); err != nil {
			return err
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
			logf("frames=%d fps=%.2f bitrate=%.2f Mbps keyframes=%d", frames, fps, mbps, keyfr)
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
	if o.HDMI && cfg.hdmiMode != "direct" {
		o.HDMI = false
	}
	return o
}

var errStop = fmt.Errorf("stop requested")

// pumpNDI reads whole raw frames from the pipeline and sends them as NDI. The
// frames are fixed size, so the framing is just "read exactly this many bytes"
// — no parsing, and no copy beyond the read itself, since UYVY is what libndi
// takes directly.
func pumpNDI(cfg runConfig, r io.Reader) error {
	ndi, err := LoadNDI(cfg.ndiLib)
	if err != nil {
		return err
	}
	defer ndi.Close()
	send, err := ndi.NewSender(cfg.name, cfg.clock)
	if err != nil {
		return err
	}
	defer send.Close()
	logf("sending as %q (from the shared capture, %d/1 fps declared)", send.SourceName(), cfg.fps)

	if cfg.out.HDMI && cfg.hdmiMode != "direct" {
		restore := pointDecoderAt(send.SourceName(), send.SourceURL())
		defer restore()
	}

	// I420: a full-size luma plane followed by two half-size chroma planes,
	// contiguous, which is exactly how libndi wants it. Stride is the luma
	// stride; NDI derives the chroma strides from it.
	frameSize := cfg.width * cfg.height * 3 / 2
	buf := make([]byte, frameSize)
	f := Frame{Width: cfg.width, Height: cfg.height, FourCC: fourCCI420, Stride: cfg.width, Data: buf}

	var frames int
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
		send.SendVideo(&f, cfg.fps, 1)
		frames++
		if now := time.Now(); now.Sub(last) >= cfg.statsEach {
			w := now.Sub(last).Seconds()
			logf("ndi: frames=%d fps=%.2f conns=%d", frames, float64(frames-atLast)/w, send.Connections())
			last, atLast = now, frames
		}
	}
}

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
