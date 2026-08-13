package main

// Working around a kernel that is older than UVC 1.5.
//
// The PLAY runs kernel 4.4.194 with "USB Video Class driver (1.1.1)", whose
// device table only matches interfaces with bInterfaceProtocol 0x00 — UVC 1.0
// and 1.1. A UVC 1.5 camera declares protocol 0x01, so the driver never even
// probes it: no /dev/video node, and, confusingly, no error anywhere in the
// kernel log. A manual bind fails just as silently.
//
// The fix is to hand uvcvideo a dynamic device ID for the camera, which makes
// it probe regardless of the protocol byte. It then drives the device happily —
// the ATEM Mini Extreme ISO this was written for reports "Found UVC 1.50
// device" and streams MJPEG at 1080p.
//
// Dynamic IDs live in the driver, so they are lost on every reboot and have to
// be re-applied. That is why this runs at startup and behind the DETECT button
// rather than being a one-off command someone has to remember.

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	usbDevicesPath  = "/sys/bus/usb/devices"
	uvcNewIDPath    = "/sys/bus/usb/drivers/uvcvideo/new_id"
	usbClassVideo   = "0e"
	uvcSettleWindow = 1500 * time.Millisecond
)

// usbID is a vendor/product pair as sysfs spells it: lowercase hex, no prefix.
type usbID struct{ vendor, product string }

// unboundUVCDevices finds video-class interfaces that no driver has claimed and
// whose protocol byte is the reason. Anything already bound is ignored, so this
// is a no-op on a camera the kernel handles natively.
func unboundUVCDevices(devicesPath string) []usbID {
	entries, err := os.ReadDir(devicesPath)
	if err != nil {
		return nil
	}
	seen := map[usbID]bool{}
	var out []usbID
	for _, e := range entries {
		name := e.Name()
		// Interface directories are "<device>:<config>.<interface>".
		colon := strings.Index(name, ":")
		if colon < 0 {
			continue
		}
		iface := filepath.Join(devicesPath, name)
		if readTrimmed(filepath.Join(iface, "bInterfaceClass")) != usbClassVideo {
			continue
		}
		// Protocol 00 is what this kernel already matches; if such an interface
		// is unbound the problem is something else and a dynamic ID will not
		// help.
		if readTrimmed(filepath.Join(iface, "bInterfaceProtocol")) == "00" {
			continue
		}
		if _, err := os.Readlink(filepath.Join(iface, "driver")); err == nil {
			continue // already claimed
		}
		parent := filepath.Join(devicesPath, name[:colon])
		id := usbID{
			vendor:  readTrimmed(filepath.Join(parent, "idVendor")),
			product: readTrimmed(filepath.Join(parent, "idProduct")),
		}
		if id.vendor == "" || id.product == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// EnsureUVCBound gives uvcvideo a dynamic ID for any UVC 1.5 camera it is
// ignoring, and waits briefly for the device nodes to appear. It returns the
// number of cameras it acted on, and is safe to call when there is nothing to
// do.
func EnsureUVCBound() int {
	ids := unboundUVCDevices(usbDevicesPath)
	if len(ids) == 0 {
		return 0
	}
	n := 0
	for _, id := range ids {
		line := id.vendor + " " + id.product
		if err := os.WriteFile(uvcNewIDPath, []byte(line), 0o200); err != nil {
			logf("could not register %s with uvcvideo: %v", line, err)
			continue
		}
		logf("registered %s with uvcvideo — a UVC 1.5 camera this kernel would otherwise ignore", line)
		n++
	}
	if n > 0 {
		// Probing and the device node appearing are not instant.
		deadline := time.Now().Add(uvcSettleWindow)
		for time.Now().Before(deadline) {
			if len(findVideoDevices()) > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return n
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
