# Notes

Working notes for this repo: status, decisions, and the traps that have actually bitten.
Migrated out of Claude Code's memory on 2026-08-24, so they are written in the first
person and dated by when each thing was learned — that date is usually the useful part.

Cross-cutting notes that are not specific to this repo live in
[fleet-notes](https://github.com/stoatworks-labs/fleet-notes).

*bdcam — UVC camera to NDI/SRT/HDMI for the BirdDog PLAY; PUBLIC sibling repo (was private until 2026-08), adds the UVC Converter tab to birdUI, working on hardware*

`~/Projects/bdcam` — **PUBLIC** (github.com/stoatworks-labs/bdcam), turns a USB
UVC camera plugged into a PLAY into an NDI/SRT sender, or straight to HDMI.
Sibling checkout beside [birddog re](https://github.com/stoatworks-labs/birddog-re/blob/main/docs/NOTES.md) (`birddog-re`), which packages it via
`fwbuild --with-cam` (binary at `../bdcam/dist/bdcam-linux-arm64`, override
`BDCAM=`).

**Visibility corrected 2026-08-15**: this note previously recorded bdcam as
private (on the grounds that it documents an unauthenticated REST API, the SSH
port and the update path on a shipping commercial product). It is in fact
public, and that is **intentional** — confirmed by Allan: bdcam is a patch for
the PLAY firmware and carries no actual NDI code, so the "never vendor the NDI
SDK" rule below is what keeps it publishable. Unlike
[birddog re](https://github.com/stoatworks-labs/birddog-re/blob/main/docs/NOTES.md) (`birddog-re`), which stays local-only for the AES key.

- **Never vendor the NDI SDK** — not headers, not a struct definition lifted
  verbatim. It `dlopen`s whatever libndi is already on the device and resolves
  symbols at runtime, so nothing NDI-derived ships. Struct layouts are recorded
  as sizes/offsets with tests asserting them.
- Go + **purego + `zig cc`**, pinned `aarch64-linux-gnu.2.28`: purego needs cgo
  for `dlopen` on Linux and macOS has no aarch64-linux gcc. Without the pin zig
  targets a newer glibc and the binary will not start on the device's Debian 10.
  ([bdplay](https://github.com/stoatworks-labs/bd-play-usb-player/blob/main/docs/NOTES.md) (`bd-play-usb-player`) and [bdts](https://github.com/stoatworks-labs/bdts/blob/main/docs/NOTES.md) (`bdts`) need none of this — they dlopen
  nothing, so `CGO_ENABLED=0` is enough.)
- **`--with-cam-ui` adds a "UVC Converter" tab to birdUI** by patching
  `videoset.html`. The patch lives in bdcam where it is unit tested, refuses
  unfamiliar markup and is exactly reversible; the installer adds a health check
  with automatic rollback. **The settings API is a SEPARATE unit**
  (`bd-cam-api` on **:8090**) from the streamer on purpose — the streamer exits
  whenever no camera is attached, and the settings page has to stay answerable.
- **The web UI unit is `BirdDogWebUI`; `birddog-web-ui` is the binary.**
  Restarting the wrong name is a silent no-op. **Consequently an HTTP 200 after
  the restart proves nothing** — it is answered by the process that never
  reloaded, so the check must also require the pid to have changed.
- Verified end to end on hardware: patched, restarted (pid 5180 → 25439), the
  tab served, and the API answering over the tailnet with CORS headers.

Its `videoset.html` observation contradicts [bdplay](https://github.com/stoatworks-labs/bd-play-usb-player/blob/main/docs/NOTES.md) (`bd-play-usb-player`)'s — see the
disputed note there before assuming either.

Related: [bdts](https://github.com/stoatworks-labs/bdts/blob/main/docs/NOTES.md) (`bdts`), [birddog play patcher](https://github.com/stoatworks-labs/birddog-play-patcher/blob/main/docs/NOTES.md) (`birddog-play-patcher`).
