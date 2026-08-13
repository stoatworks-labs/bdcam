package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeUSB builds a sysfs-shaped tree so the scan can be tested without a camera.
type fakeIface struct {
	name     string // e.g. "4-1:1.7"
	class    string
	protocol string
	bound    bool
}

func fakeUSB(t *testing.T, vendor, product string, ifaces []fakeIface) string {
	t.Helper()
	root := t.TempDir()
	devices := map[string]bool{}
	for _, i := range ifaces {
		dev := i.name[:len(i.name)-len(filepath.Ext(i.name))]
		if c := indexColon(i.name); c >= 0 {
			dev = i.name[:c]
		}
		if !devices[dev] {
			d := filepath.Join(root, dev)
			mustMkdir(t, d)
			mustWrite(t, filepath.Join(d, "idVendor"), vendor)
			mustWrite(t, filepath.Join(d, "idProduct"), product)
			devices[dev] = true
		}
		p := filepath.Join(root, i.name)
		mustMkdir(t, p)
		mustWrite(t, filepath.Join(p, "bInterfaceClass"), i.class)
		mustWrite(t, filepath.Join(p, "bInterfaceProtocol"), i.protocol)
		if i.bound {
			// A bound interface has a "driver" symlink.
			if err := os.Symlink(filepath.Join(root, "somedriver"), filepath.Join(p, "driver")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func indexColon(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The case this exists for: an ATEM-shaped device whose video interfaces
// declare UVC 1.5 and which no driver has claimed.
func TestFindsUnboundUVC15(t *testing.T) {
	root := fakeUSB(t, "1edb", "be83", []fakeIface{
		{"4-1:1.5", "02", "0d", true}, // cdc_ncm, bound
		{"4-1:1.7", "0e", "01", false},
		{"4-1:1.8", "0e", "02", false},
		{"4-1:1.9", "01", "01", true}, // audio, bound
	})
	got := unboundUVCDevices(root)
	if len(got) != 1 {
		t.Fatalf("got %d ids, want 1: %+v", len(got), got)
	}
	if got[0] != (usbID{"1edb", "be83"}) {
		t.Errorf("got %+v, want 1edb:be83", got[0])
	}
}

// A camera the kernel already drives must be left alone.
func TestIgnoresBoundCamera(t *testing.T) {
	root := fakeUSB(t, "046d", "0825", []fakeIface{
		{"1-1:1.0", "0e", "01", true},
		{"1-1:1.1", "0e", "02", true},
	})
	if got := unboundUVCDevices(root); len(got) != 0 {
		t.Errorf("a bound camera should be ignored, got %+v", got)
	}
}

// Protocol 00 is what this kernel already matches, so an unbound one has some
// other problem and a dynamic ID would not help.
func TestIgnoresProtocolZero(t *testing.T) {
	root := fakeUSB(t, "046d", "0825", []fakeIface{
		{"1-1:1.0", "0e", "00", false},
	})
	if got := unboundUVCDevices(root); len(got) != 0 {
		t.Errorf("protocol 00 should be left alone, got %+v", got)
	}
}

func TestIgnoresNonVideoInterfaces(t *testing.T) {
	root := fakeUSB(t, "413c", "2113", []fakeIface{
		{"1-1:1.0", "03", "01", false}, // HID, unbound
		{"1-1:1.1", "08", "06", false}, // mass storage, unbound
	})
	if got := unboundUVCDevices(root); len(got) != 0 {
		t.Errorf("non-video interfaces should be ignored, got %+v", got)
	}
}

// Both video interfaces of one camera must yield a single id, not two.
func TestDeduplicatesPerDevice(t *testing.T) {
	root := fakeUSB(t, "1edb", "be83", []fakeIface{
		{"4-1:1.7", "0e", "01", false},
		{"4-1:1.8", "0e", "02", false},
	})
	if got := unboundUVCDevices(root); len(got) != 1 {
		t.Errorf("expected one id for one camera, got %+v", got)
	}
}

func TestMissingSysfsIsHarmless(t *testing.T) {
	if got := unboundUVCDevices(filepath.Join(t.TempDir(), "nope")); got != nil {
		t.Errorf("a missing sysfs path should yield nothing, got %+v", got)
	}
}
