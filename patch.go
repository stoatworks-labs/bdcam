package main

// Adding the "UVC Converter" tab to the PLAY's stock web UI.
//
// videoset.html is a Go html/template read by birddog-web-ui at startup, so a
// malformed edit does not fail until the service is restarted — and if it fails
// then, the firmware upload page goes with it. That is the one thing
// birddog-re's notes say never to endanger. Hence:
//
//   * the patch lives here, in tested Go, rather than in sed in an installer;
//   * both insertions are wrapped in markers so it is exactly reversible;
//   * patching is idempotent, so a re-install cannot double up;
//   * the anchors were checked to be unique in the stock file;
//   * --patch-ui takes a backup before touching anything.
//
// All the behaviour lives in the static JS, which is served from /static/ and
// is outside the template system entirely. That keeps the template edit to two
// small blocks and means UI changes never risk the parse.

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed web/uvc-converter.js
var uvcConverterJS string

const (
	patchStart = "<!-- bdcam-uvc-tab:start -->"
	patchEnd   = "<!-- bdcam-uvc-tab:end -->"

	// Both verified to appear exactly once in the stock 1.0.34 videoset.html.
	anchorTabButton = `<button  id="tab1" class="dectablinks" onclick="opendecTab(event, 'dec1_form')">Decode Settings</button>`
	anchorContent   = `<div class="pl-3 pr-3 pb-3 pt-0" id="div_advanced_settings_content">`
)

// The tab button. Same classes as the stock one so opendecTab's show/hide and
// its active-state handling apply to ours for free.
const tabButtonHTML = patchStart +
	`<button id="tab_uvc" class="dectablinks" onclick="opendecTab(event, 'uvc_form')">UVC Converter</button>` +
	patchEnd

// The tab body. Empty on purpose — the script fills it in. It starts hidden so
// it does not appear alongside the decode settings on load; opendecTab reveals
// it, exactly as it does for dec1_form.
const tabContentHTML = patchStart +
	`<div class="dectabcontent" id="uvc_form" style="display:none;"></div>` +
	`<script src="/static/uvc-converter.js"></script>` +
	patchEnd

// IsPatched reports whether the tab has already been added.
func IsPatched(src string) bool { return strings.Contains(src, patchStart) }

// PatchVideoset inserts the tab. It is idempotent: patching twice is a no-op
// rather than an error, so re-running an installer is safe.
func PatchVideoset(src string) (string, error) {
	if IsPatched(src) {
		return src, nil
	}
	if n := strings.Count(src, anchorTabButton); n != 1 {
		return "", fmt.Errorf("expected exactly one decode-tab button to anchor to, found %d — this firmware's videoset.html differs from the one this patch was written for", n)
	}
	if n := strings.Count(src, anchorContent); n != 1 {
		return "", fmt.Errorf("expected exactly one settings-content div to anchor to, found %d — this firmware's videoset.html differs from the one this patch was written for", n)
	}
	out := strings.Replace(src, anchorTabButton, anchorTabButton+"\n\t\t\t\t"+tabButtonHTML, 1)
	out = strings.Replace(out, anchorContent, anchorContent+"\n\t\t\t\t\t\t"+tabContentHTML, 1)
	return out, nil
}

// UnpatchVideoset removes every marked block, returning the file to stock.
func UnpatchVideoset(src string) string {
	for {
		i := strings.Index(src, patchStart)
		if i < 0 {
			return src
		}
		j := strings.Index(src[i:], patchEnd)
		if j < 0 {
			return src // unterminated marker; leave it rather than eat the file
		}
		end := i + j + len(patchEnd)
		// Take the preceding whitespace we inserted along with it.
		start := i
		for start > 0 && (src[start-1] == '\t' || src[start-1] == ' ') {
			start--
		}
		if start > 0 && src[start-1] == '\n' {
			start--
		}
		src = src[:start] + src[end:]
	}
}

// PatchWebUI applies the patch to a live web UI directory, backing up first.
func PatchWebUI(dir string) error {
	tpl := filepath.Join(dir, "videoset.html")
	src, err := os.ReadFile(tpl)
	if err != nil {
		return fmt.Errorf("read %s: %w", tpl, err)
	}
	if !IsPatched(string(src)) {
		backup := filepath.Join(dir, "videoset.html.bdcam-orig")
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := os.WriteFile(backup, src, 0o644); err != nil {
				return fmt.Errorf("could not write backup %s: %w", backup, err)
			}
			logf("backed up stock template to %s", backup)
		}
	}
	out, err := PatchVideoset(string(src))
	if err != nil {
		return err
	}
	if err := writeAssets(dir); err != nil {
		return err
	}
	if out == string(src) {
		logf("videoset.html already carries the tab; assets refreshed")
		return nil
	}
	if err := os.WriteFile(tpl, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tpl, err)
	}
	logf("patched %s", tpl)
	logf("restart the web UI for it to take effect: systemctl restart birddog-web-ui")
	return nil
}

// UnpatchWebUI removes the tab and the asset.
func UnpatchWebUI(dir string) error {
	tpl := filepath.Join(dir, "videoset.html")
	src, err := os.ReadFile(tpl)
	if err != nil {
		return fmt.Errorf("read %s: %w", tpl, err)
	}
	out := UnpatchVideoset(string(src))
	if out != string(src) {
		if err := os.WriteFile(tpl, []byte(out), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", tpl, err)
		}
		logf("removed the tab from %s", tpl)
	} else {
		logf("%s carries no tab to remove", tpl)
	}
	_ = os.Remove(filepath.Join(dir, "static", "uvc-converter.js"))
	logf("restart the web UI for it to take effect: systemctl restart birddog-web-ui")
	return nil
}

func writeAssets(dir string) error {
	staticDir := filepath.Join(dir, "static")
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(staticDir, "uvc-converter.js")
	if err := os.WriteFile(dst, []byte(uvcConverterJS), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	logf("installed %s", dst)
	return nil
}
