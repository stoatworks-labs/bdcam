package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	c := DefaultConfig()
	c.Enabled = true
	return c
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		bad  string // substring expected in the error, "" means it should pass
	}{
		{"defaults are valid", func(c *Config) {}, ""},
		{"srt needs a url", func(c *Config) { c.Outputs = "srt" }, "needs an srt:// url"},
		{"srt with url", func(c *Config) { c.Outputs = "srt"; c.SRTURL = "srt://host:9000" }, ""},
		{"srt url needs a port", func(c *Config) { c.Outputs = "srt"; c.SRTURL = "srt://host" }, "explicit port"},
		{"srt url needs srt scheme", func(c *Config) { c.Outputs = "srt"; c.SRTURL = "udp://host:9000" }, "must start with srt://"},
		{"ndi cannot pair with hdmi", func(c *Config) { c.Outputs = "ndi,hdmi" }, "cannot be combined"},
		{"unknown output", func(c *Config) { c.Outputs = "rtmp" }, "unknown output"},
		{"odd size", func(c *Config) { c.Width = 1281 }, "must be even"},
		{"zero size", func(c *Config) { c.Width = 0 }, "must be positive"},
		{"fps too high", func(c *Config) { c.FPS = 120 }, "between 1 and 60"},
		{"fps zero", func(c *Config) { c.FPS = 0 }, "between 1 and 60"},
		{"bad format", func(c *Config) { c.Format = "rgb" }, "unknown --format"},
		// The encoder ceiling only applies to the paths that go through it.
		{"4K over srt is rejected", func(c *Config) {
			c.Outputs, c.SRTURL, c.Width, c.Height = "srt", "srt://h:9000", 3840, 2160
		}, "1920x1088"},
		{"4K over ndi is allowed", func(c *Config) { c.Width, c.Height = 3840, 2160 }, ""},
	} {
		c := validConfig()
		tc.mut(&c)
		err := c.Validate()
		if tc.bad == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected an error containing %q", tc.name, tc.bad)
			continue
		}
		if !strings.Contains(err.Error(), tc.bad) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.bad)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// A missing file is the first-run case, not an error.
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("missing file should load defaults: %v", err)
	}
	if c.Enabled {
		t.Error("a fresh install should default to disabled")
	}

	c.Enabled = true
	c.Outputs = "srt"
	c.SRTURL = "srt://example:9000?streamid=x"
	c.Width, c.Height, c.FPS = 1920, 1080, 25
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if back != c {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", back, c)
	}
	// Saving must not leave the temp file behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("Save left its temporary file behind")
	}
}

func TestLoadConfigRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a corrupt config should be an error, not silently ignored")
	}
}

func TestToArgs(t *testing.T) {
	c := validConfig()
	c.Outputs = "srt,hdmi"
	c.SRTURL = "srt://h:9000"
	c.Width, c.Height, c.FPS = 1920, 1080, 25
	got := strings.Join(c.ToArgs(), " ")
	for _, want := range []string{"--output srt,hdmi", "--size 1920x1080", "--fps 25", "--srt-url srt://h:9000"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
	// "auto" is the default and should not be passed explicitly.
	if strings.Contains(got, "--format") {
		t.Errorf("format auto should be omitted: %s", got)
	}
	if strings.Contains(got, "--synthetic") {
		t.Errorf("synthetic should be omitted when false: %s", got)
	}
}

// --- API ---

func testAPI(t *testing.T) (*APIServer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	return &APIServer{ConfigPath: path, LogPath: filepath.Join(dir, "bdcam.log"), NoRestart: true}, path
}

func TestAPIGetConfigDefaults(t *testing.T) {
	api, _ := testAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var c Config
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Outputs != "ndi" || c.Width != 1280 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestAPIPostSavesAndRejects(t *testing.T) {
	api, path := testAPI(t)

	// A bad config must not be written to disk.
	bad := validConfig()
	bad.Outputs = "srt" // no url
	body, _ := json.Marshal(bad)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an invalid config, got %d", rec.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an invalid config was written to disk")
	}

	good := validConfig()
	good.Outputs = "srt"
	good.SRTURL = "srt://example:9000"
	body, _ = json.Marshal(good)
	rec = httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	saved, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.SRTURL != good.SRTURL {
		t.Errorf("saved config did not round trip: %+v", saved)
	}
}

func TestAPIRejectsBadJSON(t *testing.T) {
	api, _ := testAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader("{oops")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d", rec.Code)
	}
}

func TestAPICORSPreflight(t *testing.T) {
	api, _ := testAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/config", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d", rec.Code)
	}
	// The page is served from port 80 and the API from another port, so without
	// this the browser refuses the POST.
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS origin header")
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("preflight does not permit POST")
	}
}

func TestAPICapabilitiesTellTheTruth(t *testing.T) {
	api, _ := testAPI(t)
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var caps map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps["bitrate_adjustable"] != false {
		t.Error("mpph264enc exposes no bitrate control; capabilities must say so")
	}
	if caps["max_height"].(float64) != 1088 {
		t.Errorf("max_height = %v, want 1088", caps["max_height"])
	}
}

func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", 20))
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := tailLines(path, 10)
	if len(got) != 10 {
		t.Fatalf("got %d lines, want 10", len(got))
	}
	if tailLines(filepath.Join(t.TempDir(), "nope"), 10) != nil {
		t.Error("a missing log should yield nil, not an error")
	}
}
