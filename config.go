package main

// Persistent configuration, so the converter can be driven from the PLAY's web
// UI instead of by editing a command line over SSH.
//
// The file lives beside the binary in /userdata/bd-cam/config.json. The
// streaming service reads it at startup; the API process writes it and restarts
// that service. Keeping the two as separate units means the API stays up and
// answerable even when there is no camera and the streamer has exited.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Enabled   bool   `json:"enabled"`
	Outputs   string `json:"outputs"`
	Device    string `json:"device"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	FPS       int    `json:"fps"`
	Format    string `json:"format"`
	NDIName   string `json:"ndi_name"`
	SRTURL    string `json:"srt_url"`
	Connector int    `json:"connector"`
	// "decoder" shows the camera on HDMI by pointing the PLAY's own decoder at
	// our NDI stream; "direct" drives the display with kmssink and takes it
	// away from PPApp. Empty means decoder, which is the friendlier default.
	HDMIMode  string `json:"hdmi_mode"`
	Synthetic bool   `json:"synthetic"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:  false,
		Outputs:  "ndi",
		Width:    1280,
		Height:   720,
		FPS:      30,
		Format:   "auto",
		HDMIMode: "decoder",
	}
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil // first run: defaults, disabled
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return DefaultConfig(), fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return c, nil
}

// Save writes atomically — a half-written config that the streamer then reads
// on restart would be a miserable failure to diagnose.
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

// Validate mirrors the constraints the hardware actually imposes, so the API
// refuses an impossible combination up front rather than letting GStreamer fail
// obscurely a second later.
func (c Config) Validate() error {
	outs, err := parseOutputs(c.Outputs)
	if err != nil {
		return err
	}
	if c.Width <= 0 || c.Height <= 0 {
		return fmt.Errorf("size must be positive, got %dx%d", c.Width, c.Height)
	}
	if c.Width%2 != 0 || c.Height%2 != 0 {
		return fmt.Errorf("size must be even, got %dx%d", c.Width, c.Height)
	}
	if c.FPS < 1 || c.FPS > 60 {
		return fmt.Errorf("fps must be between 1 and 60, got %d", c.FPS)
	}
	if (outs.SRT || outs.HDMI) && (c.Width > 1920 || c.Height > 1088) {
		return fmt.Errorf("srt and hdmi go through mpph264enc, whose caps stop at 1920x1088; asked for %dx%d", c.Width, c.Height)
	}
	if outs.SRT && strings.TrimSpace(c.SRTURL) == "" {
		return fmt.Errorf("srt output needs an srt:// url")
	}
	if outs.SRT {
		if _, _, err := parseSRTTarget(c.SRTURL); err != nil {
			return err
		}
	}
	if _, err := parseFormat(c.Format); err != nil {
		return err
	}
	switch c.HDMIMode {
	case "", "decoder", "direct":
	default:
		return fmt.Errorf("hdmi_mode must be decoder or direct, got %q", c.HDMIMode)
	}
	// The decoder can only show what it can receive, and that is our NDI feed.
	if outs.HDMI && c.HDMIMode != "direct" && !outs.NDI {
		return fmt.Errorf("HDMI through the decoder needs NDI switched on too — that is what the decoder displays")
	}
	return nil
}

// ToArgs renders the config as the argument list the streaming process runs
// with. Kept separate from Validate so the UI can preview exactly what it will
// cause to happen.
func (c Config) ToArgs() []string {
	a := []string{
		"--output", c.Outputs,
		"--size", fmt.Sprintf("%dx%d", c.Width, c.Height),
		"--fps", strconv.Itoa(c.FPS),
	}
	if c.Format != "" && c.Format != "auto" {
		a = append(a, "--format", c.Format)
	}
	if c.Device != "" {
		a = append(a, "--device", c.Device)
	}
	if c.NDIName != "" {
		a = append(a, "--name", c.NDIName)
	}
	if strings.TrimSpace(c.SRTURL) != "" {
		a = append(a, "--srt-url", c.SRTURL)
	}
	if c.Connector > 0 {
		a = append(a, "--connector", strconv.Itoa(c.Connector))
	}
	if c.Synthetic {
		a = append(a, "--synthetic")
	}
	if c.HDMIMode != "" {
		a = append(a, "--hdmi-mode", c.HDMIMode)
	}
	return a
}
