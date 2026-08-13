package main

// bdcam — UVC capture to NDI on a BirdDog PLAY.
//
// Milestone 1 of the "webcam in, HDMI/NDI out" pathway: V4L2 capture from a USB
// camera on the PLAY's USB-A port, into an NDI sender backed by the device's
// own libndi. Uncompressed NDI only (libndi's SpeedHQ encoder, on the CPU) —
// the VEPU hardware encoder and the DRM/HDMI output are later milestones.
//
// It exists first because it answers the two questions that decide whether the
// rest is worth building:
//
//   1. Does the Advanced-SDK 30-minute development limit apply to the libndi
//      already on the device? Run --synthetic for 45 minutes and read the log.
//   2. What resolution and frame rate can four A53s actually sustain through
//      SpeedHQ? Run --synthetic --clock=false and read the fps.
//
// Neither needs a camera, so both can be answered before any hardware arrives.

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var startTime = time.Now()

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[%s +%s] %s\n",
		time.Now().Format("15:04:05"),
		time.Duration(time.Since(startTime)/time.Second)*time.Second,
		fmt.Sprintf(format, a...))
}

func main() {
	var (
		device    = flag.String("device", "", "V4L2 device (default: first /dev/video* that is a capture device)")
		list      = flag.Bool("list", false, "list capture devices and their formats, then exit")
		size      = flag.String("size", "1280x720", "capture size, WxH")
		fps       = flag.Int("fps", 30, "requested frame rate")
		format    = flag.String("format", "auto", "pixel format: auto|uyvy|yuyv|nv12|mjpeg")
		name      = flag.String("name", "", "NDI source name (default: <hostname> (Cam))")
		synthetic = flag.Bool("synthetic", false, "generate colour bars instead of capturing — no camera needed")
		clock     = flag.Bool("clock", true, "let libndi pace sending to the frame rate; false to measure the ceiling")
		statsEach = flag.Duration("stats", 30*time.Second, "how often to log a stats line")
		duration  = flag.Duration("duration", 0, "stop after this long (0 = run forever)")
		nbufs     = flag.Int("buffers", 4, "number of V4L2 capture buffers")
		ndiLib    = flag.String("ndi-lib", "", "soname or path of libndi to load first (default: newest found)")
		output    = flag.String("output", "ndi", "outputs: ndi, srt, hdmi (comma separated; srt+hdmi may be combined)")
		srtURL    = flag.String("srt-url", "", "srt://host:port[?streamid=..&passphrase=..&latency=ms]")
		connector = flag.Int("connector", 0, "DRM connector id for hdmi output (0 = let kmssink choose)")
		ndiFormat = flag.String("ndi-format", "uyvy", "pixel format handed to libndi: uyvy or nv12 (nv12 is what the PLAY's own decoder renders natively)")
		hdmiMode  = flag.String("hdmi-mode", "direct", "how to reach HDMI: decoder (point the PLAY's own decoder at our NDI) or direct (kmssink, takes the display)")
		serve     = flag.String("serve", "", "run the configuration API on this address (e.g. :8090) instead of streaming")
		confPath  = flag.String("config", "", "read settings from this JSON file; explicit flags still win")
		unit      = flag.String("unit", "bd-cam", "systemd unit the API restarts to apply settings")
		logPath   = flag.String("log", "", "log file the API exposes via /api/log")
		patchUI   = flag.String("patch-ui", "", "add the UVC Converter tab to the web UI in this directory (e.g. /srv/birddog-web-ui)")
		unpatchUI = flag.String("unpatch-ui", "", "remove the UVC Converter tab from the web UI in this directory")
	)
	flag.Parse()

	// Which flags the user actually typed, so config.json can fill in the rest
	// without silently overriding an explicit argument.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if *patchUI != "" {
		if err := PatchWebUI(*patchUI); err != nil {
			logf("FATAL: %v", err)
			os.Exit(1)
		}
		return
	}
	if *unpatchUI != "" {
		if err := UnpatchWebUI(*unpatchUI); err != nil {
			logf("FATAL: %v", err)
			os.Exit(1)
		}
		return
	}

	if *serve != "" {
		api := &APIServer{ConfigPath: *confPath, LogPath: *logPath, Unit: *unit}
		if api.ConfigPath == "" {
			api.ConfigPath = "config.json"
		}
		if err := api.ListenAndServe(*serve); err != nil {
			logf("FATAL: %v", err)
			os.Exit(1)
		}
		return
	}

	if *confPath != "" {
		c, err := LoadConfig(*confPath)
		if err != nil {
			logf("FATAL: %v", err)
			os.Exit(2)
		}
		if !c.Enabled {
			logf("converter is disabled in %s — nothing to do", *confPath)
			os.Exit(0)
		}
		if err := c.Validate(); err != nil {
			logf("FATAL: %s: %v", *confPath, err)
			os.Exit(2)
		}
		if !set["output"] {
			*output = c.Outputs
		}
		if !set["size"] {
			*size = fmt.Sprintf("%dx%d", c.Width, c.Height)
		}
		if !set["fps"] {
			*fps = c.FPS
		}
		if !set["format"] && c.Format != "" {
			*format = c.Format
		}
		if !set["device"] && c.Device != "" {
			*device = c.Device
		}
		if !set["name"] && c.NDIName != "" {
			*name = c.NDIName
		}
		if !set["srt-url"] && c.SRTURL != "" {
			*srtURL = c.SRTURL
		}
		if !set["connector"] && c.Connector > 0 {
			*connector = c.Connector
		}
		if !set["synthetic"] && c.Synthetic {
			*synthetic = true
		}
		if !set["hdmi-mode"] && c.HDMIMode != "" {
			*hdmiMode = c.HDMIMode
		}
		logf("loaded %s", *confPath)
	}

	if *list {
		EnsureUVCBound()
		for _, d := range findVideoDevices() {
			s, err := Describe(d)
			if err != nil {
				fmt.Printf("%s: %v\n", d, err)
				continue
			}
			fmt.Print(s)
		}
		return
	}

	w, h, err := parseSize(*size)
	if err != nil {
		logf("FATAL: %v", err)
		os.Exit(2)
	}

	outs, err := parseOutputs(*output)
	if err != nil {
		logf("FATAL: %v", err)
		os.Exit(2)
	}
	if outs.SRT && *srtURL == "" {
		logf("FATAL: --output srt needs --srt-url")
		os.Exit(2)
	}

	if *name == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "birddog"
		}
		*name = host + " (Cam)"
	}

	if err := run(runConfig{
		device:    *device,
		width:     w,
		height:    h,
		fps:       *fps,
		format:    *format,
		name:      *name,
		synthetic: *synthetic,
		clock:     *clock,
		statsEach: *statsEach,
		duration:  *duration,
		nbufs:     *nbufs,
		ndiLib:    *ndiLib,
		out:       outs,
		srtURL:    *srtURL,
		connector: *connector,
		hdmiMode:  *hdmiMode,
		ndiFormat: *ndiFormat,
	}); err != nil {
		logf("FATAL: %v", err)
		// Exit 1 so systemd's Restart=always retries. A missing camera is not
		// fatal in the same sense — see the exit 0 path in run().
		os.Exit(1)
	}
}

// sourceIsMJPEG reports whether the camera we would open only offers MJPEG.
func sourceIsMJPEG(cfg runConfig) bool {
	if want, err := parseFormat(cfg.format); err == nil && want != 0 {
		return want == pixMJPG
	}
	dev := cfg.device
	if dev == "" || !IsCaptureDevice(dev) {
		EnsureUVCBound()
		devs := findVideoDevices()
		if len(devs) == 0 {
			return false
		}
		dev = devs[0]
	}
	fs, err := EnumFormats(dev)
	if err != nil {
		return false
	}
	for _, f := range fs {
		if f == pixUYVY || f == pixYUYV || f == pixNV12 {
			return false // something cheaper is on offer
		}
	}
	return true
}

type runConfig struct {
	device    string
	width     int
	height    int
	fps       int
	format    string
	name      string
	synthetic bool
	clock     bool
	statsEach time.Duration
	duration  time.Duration
	nbufs     int
	ndiLib    string
	out       Outputs
	srtURL    string
	connector int
	hdmiMode  string
	ndiFormat string
}

func run(cfg runConfig) error {
	// The pipeline owns the camera whenever more than NDI is involved. NDI on
	// its own still uses the direct V4L2 path, which is zero-copy for an
	// uncompressed camera — except for MJPEG, where Go's decoder is about ten
	// times slower than the one GStreamer links.
	if cfg.out.SRT || cfg.out.HDMI {
		return runPipeline(cfg)
	}
	if cfg.out.NDI && !cfg.synthetic && sourceIsMJPEG(cfg) {
		logf("camera is MJPEG — capturing through GStreamer, whose JPEG decoder is far faster than ours")
		return runPipeline(cfg)
	}
	ndi, err := LoadNDI(cfg.ndiLib)
	if err != nil {
		return err
	}
	defer ndi.Close()

	var capt *Capture
	var conv *Converter
	var synth *SyntheticSource

	if cfg.synthetic {
		synth = NewSyntheticSource(cfg.width, cfg.height)
		logf("source: synthetic colour bars %dx%d", cfg.width, cfg.height)
	} else {
		dev := cfg.device
		if dev == "" || !IsCaptureDevice(dev) {
			EnsureUVCBound()
			devs := findVideoDevices()
			if len(devs) == 0 {
				// No camera attached is an expected state, not a failure: the
				// systemd unit restarts us on a timer, which is how hotplug is
				// handled without a udev rule (same pattern as bdkvm).
				logf("no /dev/video* capture device found — nothing to do")
				os.Exit(0)
			}
			if dev != "" {
				logf("configured device %s is not present; using %s", dev, devs[0])
			}
			dev = devs[0]
		}
		pixfmt, err := parseFormat(cfg.format)
		if err != nil {
			return err
		}
		capt, err = OpenCapture(dev, cfg.width, cfg.height, pixfmt, cfg.fps, cfg.nbufs)
		if err != nil {
			return err
		}
		defer capt.Close()
		conv, err = NewConverter(capt.Width, capt.Height, capt.Pixfmt)
		if err != nil {
			return err
		}
		logf("source: %s (%s) %dx%d %s", capt.Path, capt.Card, capt.Width, capt.Height, fourCCName(capt.Pixfmt))
		logf("path:   %s", conv.Describe())
		if err := capt.Start(); err != nil {
			return err
		}
		cfg.width, cfg.height = capt.Width, capt.Height
	}

	send, err := ndi.NewSender(cfg.name, cfg.clock)
	if err != nil {
		return err
	}
	defer send.Close()
	logf("sending as %q (clocked=%v, %d/1 fps declared)", send.SourceName(), cfg.clock, cfg.fps)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	st := &stats{interval: cfg.statsEach, last: time.Now(), start: time.Now()}
	var deadline time.Time
	if cfg.duration > 0 {
		deadline = time.Now().Add(cfg.duration)
		logf("will stop after %s", cfg.duration)
	}

	for {
		select {
		case s := <-sig:
			logf("caught %v, stopping", s)
			st.report(send, true)
			return nil
		default:
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			logf("duration reached, stopping")
			st.report(send, true)
			return nil
		}

		if synth != nil {
			send.SendVideo(synth.Next(), cfg.fps, 1)
			st.frames++
		} else {
			err := capt.Grab(func(data []byte, seq uint32) error {
				f, err := conv.Convert(data, capt.Stride)
				if err != nil {
					st.convErrs++
					return nil // a bad JPEG is not a reason to tear down the stream
				}
				send.SendVideo(f, cfg.fps, 1)
				st.frames++
				if st.lastSeq != 0 && seq > st.lastSeq+1 {
					st.dropped += int(seq - st.lastSeq - 1)
				}
				st.lastSeq = seq
				return nil
			})
			if err != nil {
				return fmt.Errorf("capture: %w", err)
			}
		}
		st.report(send, false)
	}
}

type stats struct {
	interval time.Duration
	start    time.Time
	last     time.Time
	frames   int
	atLast   int
	dropped  int
	convErrs int
	lastSeq  uint32

	// Detector for the Advanced-SDK 30-minute development limit. If libndi
	// stops the stream, receivers fall away while we are still submitting
	// frames — so we record when a connection first appeared and shout if they
	// all vanish later.
	firstConn  time.Time
	peakConns  int
	lostLogged bool
}

func (s *stats) report(send *Sender, force bool) {
	if !force && time.Since(s.last) < s.interval {
		return
	}
	conns := send.Connections()
	now := time.Now()

	if conns > 0 && s.firstConn.IsZero() {
		s.firstConn = now
		logf("first receiver connected")
	}
	if conns > s.peakConns {
		s.peakConns = conns
	}
	if conns == 0 && s.peakConns > 0 && !s.lostLogged {
		logf("*** ALL RECEIVERS DISCONNECTED after %s of connected streaming "+
			"(peak was %d). If this is ~30 minutes and nothing else changed, "+
			"that is the Advanced SDK development limit. ***",
			now.Sub(s.firstConn).Round(time.Second), s.peakConns)
		s.lostLogged = true
	}
	if conns > 0 && s.lostLogged {
		logf("receivers reconnected — the disconnect above was not a hard stop")
		s.lostLogged = false
	}

	window := now.Sub(s.last).Seconds()
	avg := float64(s.frames) / now.Sub(s.start).Seconds()
	// A forced report can land in the same instant as a periodic one — at the
	// duration deadline, say — leaving no window to measure and printing a
	// meaningless 0.00. Fall back to the average rather than lie.
	inst := avg
	if window > 0.5 {
		inst = float64(s.frames-s.atLast) / window
	}
	logf("frames=%d fps=%.2f (avg %.2f) conns=%d dropped=%d converr=%d",
		s.frames, inst, avg, conns, s.dropped, s.convErrs)

	s.last = now
	s.atLast = s.frames
}

func parseSize(s string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(s), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad --size %q, want WxH", s)
	}
	w, err1 := strconv.Atoi(parts[0])
	h, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad --size %q, want WxH", s)
	}
	return w, h, nil
}

func parseFormat(s string) (uint32, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return 0, nil
	case "uyvy":
		return pixUYVY, nil
	case "yuyv", "yuy2":
		return pixYUYV, nil
	case "nv12":
		return pixNV12, nil
	case "mjpeg", "mjpg":
		return pixMJPG, nil
	case "h264":
		// Accepted so --list users can try it, but there is no consumer for it
		// yet: passing H.264 to NDI needs the compressed send path.
		return pixH264, fmt.Errorf("h264 capture has no NDI path yet (see README)")
	}
	return 0, fmt.Errorf("unknown --format %q", s)
}

func findVideoDevices() []string {
	matches, _ := filepath.Glob("/dev/video*")
	var out []string
	for _, m := range matches {
		// A UVC camera exposes a capture node and usually a metadata node too;
		// only the per-node capabilities tell them apart.
		if IsCaptureDevice(m) {
			out = append(out, m)
		}
	}
	return out
}
