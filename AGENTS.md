# AGENTS.md — bdcam

UVC camera → NDI sender for the **BirdDog PLAY** (Rockchip RK3328, quad A53,
Debian 10 aarch64). Go, cross-compiled for the device. **Private repo.**

Start with [`README.md`](README.md) — it carries the design, the two
measurements this exists to make, and the roadmap. This file is the operating
rules.

## Where this sits

`bdcam` is the canonical, standalone home of this program. The reverse
engineering that justifies every claim in the README lives in
`~/Projects/birddog-re` — **local-only, no remote, and it must stay that way**:
its history contains a recovered vendor AES key and a decrypted firmware blob.
Do not push `birddog-re` anywhere, and do not copy key material or vendor
firmware into this repo.

`birddog-re`'s `tools/fwbuild` builds the installable `.fw` and finds this
repo's binary at `../bdcam/dist/bdcam-linux-arm64` (override with `BDCAM=`).
So the two repos are expected to be **sibling checkouts** under `~/Projects`.

## Hard rules

1. **Never vendor the NDI SDK — not headers, not the library, not a copy of a
   struct definition lifted verbatim.** The whole design is that `bdcam`
   `dlopen`s whatever libndi is already on the device and resolves symbols at
   runtime, so nothing NDI-derived ships. The layouts in `ndi.go` are recorded
   as sizes and offsets with tests asserting them; keep it that way.
2. **Build only through `./build.sh`**, or with `GOOS=linux` set. This is
   Linux-only code and the glibc pin matters — zig defaults to a newer glibc and
   the binary then refuses to start on the device's Debian 10.
3. **Keep the layout tests green.** Every V4L2 ioctl number encodes the size of
   the struct it carries, so a wrong field means the kernel rejects the call;
   the NDI frame struct has no check on the far side at all, and a mistake there
   is a garbled frame or a crash inside libndi. `TestIoctlStructSizes` and
   `TestNDIStructSizes` are the cheap version of finding out.
4. **This repo is private and should stay private** unless someone deliberately
   reviews it first. It documents an unauthenticated REST API, the SSH port and
   the update path on a shipping commercial product.

## Toolchain

Go + [purego](https://github.com/ebitengine/purego) + `zig cc`:

```bash
brew install zig      # go is assumed
./build.sh            # -> dist/bdcam-linux-arm64
go test ./...         # layout + colour conversion, runs on the host
GOOS=linux GOARCH=arm64 go vet ./...
```

purego needs cgo for `dlopen` on Linux, and macOS has no aarch64-linux gcc —
hence zig as the C compiler, pinned to `aarch64-linux-gnu.2.28`.

## Layout

```
main.go          CLI, NDI run loop, stats, the receiver-disconnect detector
ndi.go           runtime bindings to libndi's send API (dlopen, free-SDK surface)
v4l2.go          capture, written straight against the ioctl interface
frame.go         format conversion + the synthetic colour-bar source
pipeline.go      GStreamer pipeline construction for SRT and HDMI
run_pipeline.go  the SRT/HDMI run loop
annexb.go        recovering access units from the encoder's byte stream
mpegts.go        MPEG-TS muxer (the device has none)
srt.go           SRT transport, pure Go
build.sh         cross-compile
```

## The SRT and HDMI paths depend on the device's GStreamer

`mpph264enc`, `mppjpegdec` and `kmssink` are all present on stock firmware and
do the heavy lifting; `bdcam` shells out to `gst-launch-1.0` and owns only the
transport. That is a deliberate trade — the vendor elements are tested against
this silicon and hand-written MPP/DRM bindings are not — but it does mean those
outputs are only as good as what the device's plugins expose. In this build
`mpph264enc` exposes no properties at all, so bitrate is not adjustable without
replacing it.

## Things that have bitten

- **`LD_PRELOAD` does not pin which libndi is used** — `dlopen` of the other
  soname still finds the other file. That is what `--ndi-lib` is for, and it
  matters because the device ships two copies which need not be licensed alike.
- **`PPApp` is DRM master on `card0`.** Anything that wants the HDMI output has
  to stop it first; there is no sharing.
- **The MJPEG path is a placeholder on the NDI side only.** Software JPEG decode
  is roughly a core at 1080p on this SoC. The SRT/HDMI pipeline gets hardware
  decode from `mppjpegdec`; the NDI path still does not. Check `--list` before
  assuming a camera is cheap to ingest.
- **Stopping `PPApp` to free DRM leaves the HDMI output dark.** It looks like a
  brick and is not. Start `BirdDogRunner` again when you are done.
