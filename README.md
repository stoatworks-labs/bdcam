# bdcam — UVC camera to NDI on a BirdDog PLAY

The "webcam in, HDMI/NDI/SRT out" pathway. V4L2 capture from a USB camera on the
PLAY's USB-A port, out as NDI, SRT or HDMI. NDI is backed by **the device's own
libndi**, loaded with `dlopen` — the same approach as the `bdkvm` NDI KVM
endpoint in the (local-only) `birddog-re` research repo.

Outputs are **NDI**, **SRT** and **HDMI**. NDI is uncompressed (libndi's SpeedHQ
encoder, on the CPU); SRT and HDMI run on the device's own GStreamer, using the
VEPU hardware H.264 encoder and `kmssink`. The compressed NDI|HX send path is
still a later milestone (see [Roadmap](#roadmap)).

**Verified on hardware, 2026-08-12**, on a PLAY running 1.0.30: NDI send, SRT
out (validated by an independent demuxer) and HDMI via `kmssink` all work. The
camera itself is still untested — no UVC device has yet enumerated on the unit,
so every result so far used `--synthetic`.

## What the measurements said

Both questions this was built to answer have now been answered on hardware,
using `--synthetic` with no camera attached.

**1. The 30-minute development limit does not apply.** Both shipped copies of
libndi (`libndi.so.5.5.2` and `libndi.so.6.0.1`) contain the string *"designed
for development use and will run on a stream for 30 minutes"* — but it is in
every build of the library and proves nothing on its own. A sender was run
against `libndi.so.5.5.2` for **41.5 minutes of continuously connected
streaming: two receivers, zero disconnect events, a flat 29.99 fps, no dropped
frames**. There is no cutoff.

**2. SpeedHQ costs about 1.18 cores at 720p30.** Measured with a receiver
attached and actually decoding: `bdcam` at 117.6% CPU, `PPApp` taking another
29.4% to decode it back, the box still 57% idle. Scaling by pixel count puts
1080p30 near 2.6 cores — inside the 2.5–5.0 the `birddog-re` analysis predicted,
at the optimistic end.

For comparison, the hardware codecs: **decode runs at 218 fps at 720p** and is
essentially free; **the VEPU encode is the limiter at roughly 31 fps at 720p**,
though that figure was taken with ~1.2 cores of other load on the box.

## Build

```bash
./build.sh
```

Go + [purego](https://github.com/ebitengine/purego) + `zig cc` targeting
`aarch64-linux-gnu.2.28`, same as `bdkvm`: purego needs cgo for `dlopen`, macOS
has no aarch64-linux gcc, and the glibc pin keeps the binary compatible with the
PLAY's Debian 10. Output is `dist/bdcam-linux-arm64` (~3.4 MB).

Always build through `build.sh` or with `GOOS=linux` set — this is Linux-only
code.

```bash
go test ./...        # layouts, colour conversion, TS muxing, pipeline shape
```

The layout tests are worth keeping green. Every V4L2 ioctl number encodes the
size of the struct it carries, so a wrong field makes the *kernel* reject the
call; the NDI structs have no such check on the other side at all, and a mistake
there is a garbled frame or a crash inside libndi. The MPEG-TS tests are the
same idea one layer up — a receiver will simply refuse a stream whose PSI CRCs
or continuity counters are wrong, and say nothing useful about why.

Beyond the host tests, the muxer output has been round-tripped on the device
through `tsdemux ! h264parse ! mppvideodec` — an independent demuxer and the
hardware decoder — to EOS with no errors.

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

## Reproducing the measurements

Both of these are settled (see above); this is how to re-run them, on another
unit or after a firmware change.

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

## Outputs

```bash
bdcam --output ndi                                    # SpeedHQ, on the CPU
bdcam --output srt  --srt-url srt://host:9000?streamid=cam
bdcam --output hdmi                                   # kmssink, straight to the panel
bdcam --output srt,hdmi --srt-url srt://host:9000     # one capture, tee'd to both
```

SRT and HDMI are built on the device's own GStreamer, which carries exactly the
elements needed: `mpph264enc` (the VEPU), `mppjpegdec` (hardware JPEG, so an
MJPEG camera costs no CPU) and `kmssink`. Encoded H.264 comes back over a pipe,
gets split into access units and muxed to MPEG-TS here — the device has no muxer
at all, and its libsrt exists only statically linked inside `PPApp`, so neither
could be borrowed.

Three limits worth knowing before designing around this, all measured:

- **`mpph264enc` has no tunable properties in this build.** No bitrate, no GOP,
  no rate-control mode; it derives a bitrate from the caps (3.456 Mbps at
  720p30). A bitrate control means replacing the element.
- **Its sink caps stop at 1920×1088 and 60/1**, so 1080p is the ceiling here
  whatever the camera can do. `bdcam` rejects anything larger with a clear
  message rather than letting caps negotiation fail obscurely.
- **`--output ndi` cannot be combined with `srt` or `hdmi` yet.** The NDI path
  does its own V4L2 capture while GStreamer owns the camera for the others, and
  one device cannot have two owners.

### HDMI has two possible routes

`--output hdmi` uses `kmssink`, which needs DRM master — and `PPApp` holds it:

```bash
systemctl stop BirdDogRunner      # remember to start it again afterwards
```

While it is stopped the HDMI output is dark, which looks like a brick and is
not. `bdcam` checks and warns before GStreamer hits the failure.

The alternative is to leave `PPApp` running and let *it* do the display: send
SRT to `127.0.0.1` and point the decoder at it. That costs an encode/decode
round trip, but both ends are hardware (VEPU out, rkvdec back in at 218 fps), and
it keeps the OSD, web UI, tally and CloudConnect intact. For a device that
should still behave like a PLAY, that is usually the better trade.

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

Done, and verified on hardware: NDI send, SRT out, HDMI via `kmssink`, hardware
JPEG decode on the SRT/HDMI path.

Still open, roughly in order of value:

1. **A real camera.** Everything so far is `--synthetic`; no UVC device has yet
   enumerated on the test unit. The first real frame is the outstanding unknown.
2. **One capture owner**, so `ndi` can be combined with `srt`/`hdmi`. Either
   move NDI onto the GStreamer pipeline via a second `tee` branch, or have
   GStreamer feed frames back over the existing pipe.
3. **Hardware JPEG decode on the NDI path too.** The pipeline path already gets
   it from `mppjpegdec`; the NDI path still uses Go's `image/jpeg`, which is
   roughly a core at 1080p.
4. **Bitrate control**, which means replacing `mpph264enc` — most likely driving
   MPP directly, since the element exposes nothing.
5. **NDI|HX (compressed send).** The device's libndi exports
   `send_compressed_video` and `codec_h264_hevc_alpha`, so VEPU H.264 → NDI at
   near-zero CPU is available. Now unblocked by the 30-minute answer above, but
   still needs the Advanced structs reconstructed without headers.
6. **Audio** — UAC capture into `NDIlib_send_send_audio_v3`, and into the TS for
   SRT.

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
