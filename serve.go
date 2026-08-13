package main

// The configuration API behind the "UVC Converter" tab in the PLAY's web UI.
//
// This runs as its own process (bdcam --serve) under its own systemd unit, kept
// separate from the streaming process on purpose: the streamer exits when there
// is no camera, and a settings page that disappears whenever the camera is
// unplugged would be useless. The API writes config.json and restarts the
// streamer to apply it.
//
// Access control is the same as the rest of the device: none. The PLAY's own
// REST API on :8080 has no authentication and sets Access-Control-Allow-Origin
// to *, so this matches what is already there rather than adding a new class of
// exposure — but it does mean the tailnet or the LAN boundary is the only
// control, exactly as the Tailscale note in birddog-re says. Do not put this
// device on an untrusted network.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type APIServer struct {
	ConfigPath string
	LogPath    string
	Unit       string // systemd unit for the streaming process
	NoRestart  bool   // tests set this so they do not shell out
}

func (a *APIServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", a.handleConfig)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/devices", a.handleDevices)
	mux.HandleFunc("/api/capabilities", a.handleCapabilities)
	mux.HandleFunc("/api/log", a.handleLog)
	return cors(mux)
}

// cors mirrors what the device's own API already does, and answers preflight so
// a POST from the web UI's origin works.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (a *APIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		c, err := LoadConfig(a.ConfigPath)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, c)

	case http.MethodPost:
		var c Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("could not read config: %w", err))
			return
		}
		if err := c.Validate(); err != nil {
			// 422 rather than 400: the JSON parsed, the settings are the problem.
			writeErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		if err := c.Save(a.ConfigPath); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		logf("config saved: %s", strings.Join(c.ToArgs(), " "))
		applied, msg := a.apply(c)
		writeJSON(w, http.StatusOK, map[string]any{
			"config":  c,
			"applied": applied,
			"message": msg,
			"args":    c.ToArgs(),
		})

	default:
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("use GET or POST"))
	}
}

// apply restarts the streaming unit so the new settings take effect, or stops
// it when the converter has been switched off.
func (a *APIServer) apply(c Config) (bool, string) {
	if a.NoRestart || a.Unit == "" {
		return false, "saved; restart the streaming service to apply"
	}
	verb := "restart"
	if !c.Enabled {
		verb = "stop"
	}
	out, err := exec.Command("systemctl", verb, a.Unit).CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("saved, but systemctl %s %s failed: %v: %s",
			verb, a.Unit, err, strings.TrimSpace(string(out)))
	}
	return true, fmt.Sprintf("saved and %sed %s", verb, a.Unit)
}

type statusResponse struct {
	Service      string   `json:"service"`
	ServiceState string   `json:"service_state"`
	CameraFound  bool     `json:"camera_found"`
	Devices      []string `json:"devices"`
	Config       Config   `json:"config"`
	LastLog      []string `json:"last_log"`
	DRMHeldBy    string   `json:"drm_held_by"`
	Now          string   `json:"now"`
}

func (a *APIServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	c, _ := LoadConfig(a.ConfigPath)
	EnsureUVCBound()
	devs := findVideoDevices()
	writeJSON(w, http.StatusOK, statusResponse{
		Service:      a.Unit,
		ServiceState: a.unitState(),
		CameraFound:  len(devs) > 0,
		Devices:      devs,
		Config:       c,
		LastLog:      tailLines(a.LogPath, 12),
		DRMHeldBy:    drmHolder(),
		Now:          time.Now().Format(time.RFC3339),
	})
}

func (a *APIServer) unitState() string {
	if a.Unit == "" {
		return "unknown"
	}
	out, _ := exec.Command("systemctl", "is-active", a.Unit).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}

type deviceInfo struct {
	Path    string   `json:"path"`
	Formats []string `json:"formats"`
	Detail  string   `json:"detail"`
}

func (a *APIServer) handleDevices(w http.ResponseWriter, r *http.Request) {
	// Pressing DETECT should fix the common case, not just report it: a UVC 1.5
	// camera is invisible to this kernel until it is given a dynamic id, and
	// that is lost on every reboot.
	EnsureUVCBound()
	var out []deviceInfo
	for _, d := range findVideoDevices() {
		info := deviceInfo{Path: d}
		if fs, err := EnumFormats(d); err == nil {
			for _, f := range fs {
				info.Formats = append(info.Formats, fourCCName(f))
			}
		}
		if s, err := Describe(d); err == nil {
			info.Detail = s
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// handleCapabilities lets the page describe the hardware honestly instead of
// hard-coding limits that are really properties of this device's GStreamer.
func (a *APIServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"outputs":            []string{"ndi", "srt", "hdmi"},
		"outputs_combinable": true,
		"hdmi_modes":         []string{"decoder", "direct"},
		"formats":            []string{"auto", "nv12", "mjpeg", "yuyv", "uyvy"},
		"max_width":          1920,
		"max_height":         1088,
		"max_fps":            60,
		"bitrate_adjustable": false,
		"notes": []string{
			"SRT and HDMI encode on the VEPU via mpph264enc, which exposes no bitrate, GOP or rate-control setting in this firmware — it derives a bitrate from the resolution and frame rate.",
			"All outputs share one capture, so any combination can run at once.",
			"HDMI through the decoder points the PLAY's own decoder at the NDI stream, so nothing is taken from it — the OSD, web UI and tally keep working. It costs an encode and a decode of latency, and needs NDI switched on.",
			"Direct HDMI uses kmssink, which needs DRM master and therefore stops the decoder for as long as the converter runs.",
		},
	})
}

func (a *APIServer) handleLog(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 500 {
			n = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": tailLines(a.LogPath, n)})
}

// tailLines returns the last n lines of a file, reading only the tail of it —
// the streaming log is rotated at 1 MB but there is no reason to read even that.
func tailLines(path string, n int) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	const window = 64 << 10
	start := st.Size() - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && len(buf) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is probably truncated
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// drmHolder reports which process currently owns /dev/dri/card0, since that is
// the single most common reason the HDMI output will not start.
func drmHolder() string {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, p := range procs {
		if !p.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		fds, err := os.ReadDir("/proc/" + p.Name() + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + p.Name() + "/fd/" + fd.Name())
			if err == nil && strings.Contains(link, "dri/card0") {
				comm, _ := os.ReadFile("/proc/" + p.Name() + "/comm")
				return strings.TrimSpace(string(comm))
			}
		}
	}
	return ""
}

func (a *APIServer) ListenAndServe(addr string) error {
	logf("config API on %s (config=%s, unit=%s)", addr, a.ConfigPath, a.Unit)
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}
