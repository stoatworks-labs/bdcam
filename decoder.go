package main

// Showing the camera on HDMI by way of the PLAY's own decoder.
//
// The obvious way to get HDMI out is kmssink, but that means taking DRM master
// from PPApp, which costs the OSD, the web UI's video integration, tally and
// CloudConnect for as long as the converter runs — and it needs a second colour
// conversion, which on this GStreamer is the most expensive step in the whole
// pipeline.
//
// The alternative is to leave PPApp doing what it is for. bdcam sends NDI; the
// decoder is pointed at that stream over loopback and puts it on HDMI itself.
// Nothing is stopped, nothing is taken, and the second conversion disappears.
// The cost is a round trip through NDI: an encode here and a decode there, plus
// the latency that implies.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// The device's own REST API. No authentication on this port at all — see
	// the note in serve.go.
	birddogAPI    = "http://127.0.0.1:8080"
	sourceNameFil = "/etc/birddog-source1-name"
)

// pointDecoderAt tells the PLAY to display the given NDI source, and returns a
// function that puts the previous selection back.
func pointDecoderAt(name, url string) func() {
	previous := strings.TrimSpace(readTrimmed(sourceNameFil))

	host, port := "127.0.0.1", "5961"
	if h, p, err := net.SplitHostPort(strings.TrimSpace(url)); err == nil && p != "" {
		// Prefer loopback over whatever address libndi advertises: the stream
		// never has to leave the box.
		if h != "" {
			host = "127.0.0.1"
		}
		port = p
	} else if url != "" {
		logf("could not read a port out of the NDI url %q — trying %s:%s", url, host, port)
	}

	if err := setDecoderSource(name, host, port); err != nil {
		logf("WARNING: could not point the decoder at %q (%v) — HDMI will keep showing whatever it had", name, err)
		return func() {}
	}
	// Writing the source file is not enough: PPApp reads it when it starts and
	// otherwise carries on with whatever it already had. Without this the
	// decoder sits there with the right address and never connects — the
	// symptom is a black picture and a sender that reports no receivers.
	if err := restartDecoder(); err != nil {
		logf("WARNING: could not restart the decoder (%v) — it may not pick up the new source", err)
	}
	logf("decoder pointed at %q (%s:%s) and restarted; HDMI is being driven by PPApp, not by us", name, host, port)
	if previous != "" && previous != name {
		logf("previous source was %q and will be restored on exit", previous)
	}

	return func() {
		if previous == "" || previous == name {
			return
		}
		logf("restoring the decoder source to %q", previous)
		if err := setDecoderSource(previous, "", ""); err != nil {
			logf("WARNING: could not restore the decoder source (%v)", err)
			return
		}
		// Same again: the decoder needs restarting to act on the change, or it
		// keeps trying to receive a sender that has just gone away.
		if err := restartDecoder(); err != nil {
			logf("WARNING: could not restart the decoder (%v)", err)
		}
	}
}

// restartDecoder asks PPApp to reload. It is the documented way to make the
// device act on a source change, and the SRT listener behaves the same way.
func restartDecoder() error {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, birddogAPI+"/restart", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("/restart returned %s", resp.Status)
	}
	// It takes a moment to come back and reconnect.
	time.Sleep(3 * time.Second)
	return nil
}

func setDecoderSource(name, ip, port string) error {
	body := map[string]any{"sourceName": name, "chNum": 1}
	if ip != "" && port != "" {
		body["connectToIp"] = ip
		body["port"] = port
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, birddogAPI+"/connectTo", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned %s", birddogAPI+"/connectTo", resp.Status)
	}
	// The decoder reads the file rather than the response, so make sure it took.
	for i := 0; i < 10; i++ {
		if strings.TrimSpace(readTrimmed(sourceNameFil)) == name {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(sourceNameFil); err != nil {
		return fmt.Errorf("%s never appeared", sourceNameFil)
	}
	return fmt.Errorf("%s did not change to %q", sourceNameFil, name)
}
