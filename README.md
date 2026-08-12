# bdcam — UVC camera to NDI on a BirdDog PLAY

Milestone 1 of the "webcam in, HDMI/NDI out" pathway. V4L2 capture from a USB
camera on the PLAY's USB-A port into an NDI sender backed by **the device's own
libndi**, loaded with `dlopen` — the same approach as the `bdkvm` NDI KVM
endpoint in the (local-only) `birddog-re` research repo.

Uncompressed NDI only — that is libndi's SpeedHQ encoder, running on the CPU.
The VEPU hardware encoder, the compressed/HX send path and the DRM HDMI output
are later milestones (see [Roadmap](#roadmap)).

## Why this first

Two unknowns decide whether the rest of the pathway is worth building, and both
can be answered by this binary **with no camera attached**:

1. **Does the Advanced-SDK 30-minute development limit apply to the libndi
   already on the device?** Both shipped copies (`libndi.so.5.5.2` and
   `libndi.so.6.0.1`) contain the string *"designed for development use and will
   run on a stream for 30 minutes"*. It is present in every build of the
   library, so it cannot tell us statically whether BirdDog's copy is licensed —
   and it may never have mattered to them, because PLAY only ever receives.
2. **What can four A53s at 1.392 GHz actually sustain through SpeedHQ?**
   The `birddog-re` analysis puts RK3328 at ~0.6× RK3566 on the CPU codec path,
   which puts
   1080p30 at or beyond the whole SoC. That is an estimate. This makes it a
   number.

## Build

```bash
./build.sh
```

Go + [purego](https://github.com/ebitengine/purego) + `zig cc` targeting
`aarch64-linux-gnu.2.28`, same as `bdkvm`: purego needs cgo for `dlopen`, macOS
has no aarch64-linux gcc, and the glibc pin keeps the binary compatible with the
PLAY's Debian 10. Output is `dist/bdcam-linux-arm64` (~2 MB).

Always build through `build.sh` or with `GOOS=linux` set — this is Linux-only
code.

```bash
go test ./...        # struct layouts and colour conversion, runs on the host
```

The layout tests are worth keeping green. Every V4L2 ioctl number encodes the
size of the struct it carries, so a wrong field makes the *kernel* reject the
call; the NDI structs have no such check on the other side at all, and a mistake
there is a garbled frame or a crash inside libndi.

## Install

Through the normal patching path — nothing NDI-derived travels in the package:

```bash
./build.sh && ~/Projects/birddog-re/tools/fwbuild/build.sh --tag cam --no-tailscale --with-cam
```

Upload the resulting `.fw` through the PLAY's stock web UI. It installs to
`/userdata/bd-cam/` under `bd-cam.service`, following the same rules as
`bd-kvm`: everything under `/userdata`, self-supervised, nothing on the recovery
path touched. `bdcam` exits 0 when no camera is present and the unit is
`Restart=always`, so the service is its own hotplug poller and needs no udev
rule.

Runtime arguments live in `/userdata/bd-cam/bdcam.conf`, not in the unit, so the
device can be retuned without repackaging:

```bash
ssh -p 9031 root@<play> 'echo BDCAM_ARGS=\"--size 1920x1080 --fps 30\" > /userdata/bd-cam/bdcam.conf; systemctl restart bd-cam'
```

Logs go to `/userdata/bd-cam/bdcam.log` (rotated at 1 MB).

## The two experiments

### 1. The 30-minute question

```bash
/userdata/bd-cam/bdcam --synthetic --size 1280x720 --fps 30 --stats 60s --duration 50m
```

Connect a receiver (Studio Monitor, `ndi-record`, anything) and **leave it
connected**. `bdcam` logs a stats line every minute including the live receiver
count, and shouts if every receiver disappears while it is still submitting
frames:

```
*** ALL RECEIVERS DISCONNECTED after 30m02s of connected streaming ... ***
```

That message with a ~30 minute figure is the development limit. Anything else —
no message at all across 50 minutes, or a disconnect at an unrelated time that
recovers — is not. The synthetic source steps a white bar across the frame every
frame, so you can also tell "still sending" from "frozen on the last frame" by
eye at the receiver.

Run it against **both** libraries, because they need not be licensed alike.
`--ndi-lib` pins which one is loaded (`LD_PRELOAD` will not do it — `dlopen` of
the other soname still finds the other file):

```bash
/userdata/bd-cam/bdcam --ndi-lib /usr/lib/aarch64-linux-gnu/libndi.so.5.5.2 --synthetic ...
/userdata/bd-cam/bdcam --ndi-lib /usr/lib/aarch64-linux-gnu/libndi.so.6.0.1 --synthetic ...
```

Left unset, `bdcam` prefers `.6` and falls back to `.5`. Either way the loaded
soname and the version string it reports are the first line in the log — check
it says what you intended before trusting a 50-minute result.

`PPApp` links `libndi.so.5`, so `.5.5.2` is the copy that is actually in service
on a stock unit; `.6.0.1` ships alongside it and may just be unused.

### 2. The SpeedHQ ceiling

```bash
/userdata/bd-cam/bdcam --synthetic --size 1920x1080 --clock=false --stats 10s --duration 3m
```

`--clock=false` removes libndi's rate limiting, so the logged fps is what the
box can encode, not what we asked for. Repeat at `1280x720` and `960x540`. Watch
`top` alongside — the interesting figure is fps *and* how many of the four cores
it took to get there.

With `--clock` left on (the default), the same numbers tell you whether the
declared rate is actually being met in normal operation.

## Capture formats, and why the choice matters

`--format auto` (the default) picks in this order, which is strictly an order of
what each costs us downstream:

| Camera gives | What bdcam does | Cost |
|---|---|---|
| UYVY | hands libndi the driver's mmap'd buffer | nothing |
| YUYV | byte swap in place | negligible |
| NV12 | hands libndi the buffer, NDI takes NV12 directly | nothing |
| MJPEG | **software JPEG decode** (Go's `image/jpeg`) then pack to UYVY | very expensive |

The MJPEG path is a placeholder and it is deliberately the last choice. On four
A53s a 1080p JPEG decode in software is roughly a core on its own, taken from
the same budget SpeedHQ is already straining. It is there so any camera works on
day one, not because it is the answer. The real fix is MPP's JPEG decoder on the
vdpu — see the roadmap.

So the first thing to run on the device is:

```bash
/userdata/bd-cam/bdcam --list
```

If the camera offers YUYV or UYVY at the resolution you want, the CPU budget is
about SpeedHQ alone. If it is MJPEG-only, subtract a core before you start.
(The install-time probe report captures this too, at
`http://<play-ip>/static/bd-probe.txt`, so you get it without an SSH session.)

## Roadmap

Ordered by what unblocks the most:

1. **MPP JPEG decode** — replace `image/jpeg` with the vdpu via
   `librockchip_mpp.so.1`, which is already on the device. Removes the MJPEG
   penalty entirely.
2. **DRM/HDMI output** — `PPApp` is DRM master on `card0`, so this means
   stopping it and owning the display, MPP→DRM with no RGA (the path found in
   `PPApp` itself). Costs the OSD, web UI integration, tally and CloudConnect.
3. **VEPU H.264 encode** — `vepu@ff340000` is enabled with the `rk_vcodec`
   driver in-kernel. Hantro-class: H.264 only, ~1080p30, no HEVC, no B-frames.
   Feeds both of the next two.
4. **SRT out** — libsrt is already linked into `PPApp` and therefore already on
   the box. Licence-free, and the cheapest way to get encoded video off the
   device.
5. **NDI|HX (compressed send)** — the device's libndi exports
   `send_compressed_video` and `codec_h264_hevc_alpha`, so VEPU H.264 → NDI at
   near-zero CPU is technically available. Blocked on the 30-minute answer above
   and on reconstructing the Advanced structs without headers.
6. **Audio** — UAC capture from the camera into `NDIlib_send_send_audio_v3`.
   `AlsaAudioSink` in `PPApp` shows the device side works.

Also still open, and answerable only with a unit in hand: whether the USB-A port
is on the dwc3 (USB 3.0) or the EHCI (USB 2.0), which decides whether
uncompressed capture above 720p is possible at all, and whether the port will
power a camera rather than just a keyboard. `bdcam --list` plus `lsusb -t`
answers it.

## On the NDI SDK

This binary contains no NDI code. It `dlopen`s whatever libndi is already
installed on the device and calls it through symbols resolved at runtime, so
nothing NDI-derived is in this repo or in anything the packaging tool builds.
The struct layouts in `ndi.go` were read from SDK headers held elsewhere and are
free-SDK surface only.

That covers distribution. It does not by itself settle *use* — running your own
sender on a runtime licensed to someone else is a different question from
shipping the library, and anything published would want its own Advanced SDK
agreement. Experiment 1 above is partly how you find out which conversation you
are in.
