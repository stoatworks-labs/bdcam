package main

import (
	"html/template"
	"os"
	"strings"
	"testing"
)

// A miniature stand-in with the same shape as the parts of videoset.html the
// patch touches, including the Go template actions that make a careless edit
// dangerous.
const miniTemplate = `<html><body>
{{if eq .Mode "decode"}}
            <div class="tab">
                ` + anchorTabButton + `
            </div>
{{end}}
            <div class="div_box_content">
                        ` + anchorContent + `
                        <form class="dectabcontent" id="dec1_form">{{ .Decode_SourceName }}</form>
                        </div>
            </div>
</body></html>`

func TestPatchInsertsBothBlocks(t *testing.T) {
	out, err := PatchVideoset(miniTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `id="tab_uvc"`) {
		t.Error("tab button not inserted")
	}
	if !strings.Contains(out, `id="uvc_form"`) {
		t.Error("tab content not inserted")
	}
	if !strings.Contains(out, "/static/uvc-converter.js") {
		t.Error("script tag not inserted")
	}
	// The stock content must survive untouched.
	if !strings.Contains(out, `id="dec1_form"`) {
		t.Error("the patch removed the stock decode form")
	}
	if !strings.Contains(out, "{{ .Decode_SourceName }}") {
		t.Error("the patch damaged a template action")
	}
}

// The patched file still has to parse as a Go template — if it does not, the
// web UI will not start, and the firmware upload page goes with it.
func TestPatchedTemplateStillParses(t *testing.T) {
	out, err := PatchVideoset(miniTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := template.New("videoset").Parse(out); err != nil {
		t.Fatalf("patched template does not parse: %v", err)
	}
}

func TestPatchIsIdempotent(t *testing.T) {
	once, err := PatchVideoset(miniTemplate)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := PatchVideoset(once)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Error("patching twice changed the file again — a re-install would double the tab")
	}
	if strings.Count(twice, `id="tab_uvc"`) != 1 {
		t.Errorf("found %d tab buttons, want 1", strings.Count(twice, `id="tab_uvc"`))
	}
}

func TestUnpatchRestoresExactly(t *testing.T) {
	out, err := PatchVideoset(miniTemplate)
	if err != nil {
		t.Fatal(err)
	}
	back := UnpatchVideoset(out)
	if back != miniTemplate {
		t.Errorf("unpatch did not restore the original:\n--- got ---\n%s\n--- want ---\n%s", back, miniTemplate)
	}
}

func TestUnpatchOnCleanFileIsNoOp(t *testing.T) {
	if got := UnpatchVideoset(miniTemplate); got != miniTemplate {
		t.Error("unpatching an unpatched file changed it")
	}
}

// A firmware whose markup differs must be refused rather than patched blindly.
func TestPatchRefusesUnknownMarkup(t *testing.T) {
	if _, err := PatchVideoset("<html><body>nothing familiar here</body></html>"); err == nil {
		t.Fatal("expected a refusal when the anchors are missing")
	}
	// Two decode tabs would make the insertion point ambiguous.
	if _, err := PatchVideoset(miniTemplate + miniTemplate); err == nil {
		t.Fatal("expected a refusal when the anchor is ambiguous")
	}
}

func TestEmbeddedAssetIsPresent(t *testing.T) {
	if !strings.Contains(uvcConverterJS, "uvc_form") {
		t.Error("the embedded JS does not look like the converter page")
	}
	if len(uvcConverterJS) < 1000 {
		t.Errorf("embedded JS is only %d bytes; did the embed resolve?", len(uvcConverterJS))
	}
}

// Run against the real firmware file when it is available, which is the check
// that actually matters. Skips cleanly when it is not.
//
//	BDCAM_VIDESET=/path/to/videoset.html go test -run RealTemplate
func TestRealTemplateIfAvailable(t *testing.T) {
	path := os.Getenv("BDCAM_VIDEOSET")
	if path == "" {
		t.Skip("set BDCAM_VIDEOSET to the stock videoset.html to run this")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := PatchVideoset(string(src))
	if err != nil {
		t.Fatalf("patching the real template failed: %v", err)
	}
	if out == string(src) {
		t.Fatal("the real template was not modified")
	}
	if UnpatchVideoset(out) != string(src) {
		t.Error("unpatching the real template did not restore it byte for byte")
	}
}

func TestAssetURLIsVersioned(t *testing.T) {
	out, err := PatchVideoset(miniTemplate)
	if err != nil {
		t.Fatal(err)
	}
	// Without a versioned URL a JS change is invisible behind the browser cache.
	if !strings.Contains(out, "/static/uvc-converter.js?v=") {
		t.Errorf("script URL is not cache-busted: %s", out)
	}
	if len(assetVersion()) != 8 {
		t.Errorf("asset version %q should be 8 hex chars", assetVersion())
	}
}
