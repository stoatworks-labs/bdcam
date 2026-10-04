# Attributions

bdcam is built on other people's work. This file lists what that work is, who did
it, and what it is doing here.

It is generated — the master lists live in the `stoatworks-backend` repo and are
pushed out by `scripts/sync-attributions.py`. Edit it there, not here.

## Code we derived from other people's work

Someone else solved this first, and this project would not exist in its current form without their work.

### gosrt

<https://github.com/datarhei/gosrt>

srt.go's SRT output uses this pure-Go SRT implementation (github.com/datarhei/gosrt in go.mod). The PLAY has libsrt only statically linked inside PPApp, so there is nothing to dlopen, and a pure-Go implementation also keeps the cross-compile free of C dependencies. Caller mode only.

## Third-party code this project uses

Libraries, SDKs and frameworks the project is built on or bundles.

### purego

<https://github.com/ebitengine/purego>  
Licence: Apache-2.0  
Copyright: The Ebitengine Authors

A Go module dependency of the on-device agent.

Calls into a C shared library from Go without cgo, which is how the agent dlopens whatever libndi the device already has instead of linking against a redistributable copy.

## Work we checked ourselves against

No code was taken from these — but they were how we knew we had it right, and that is worth saying out loud.

### The PLAY's own GStreamer: tsdemux, h264parse and mppvideodec

The MPEG-TS muxer's output was round-tripped on the device through tsdemux ! h264parse ! mppvideodec, an independent demuxer and the hardware decoder, to EOS with no errors, and SRT out was validated by an independent demuxer.

## Standards and published specifications

What the implementation is measured against.

- **ISO/IEC 13818-1** — mpegts.go is a minimal MPEG-TS muxer for one H.264 elementary stream, written to the standard because the PLAY has no muxer and SRT receivers expect MPEG-TS. The layout notes on each field are there so the next person need not re-derive them from the spec.
- **H.264 Annex B byte stream** — annexb.go recovers access units from the encoder's Annex-B stream, splitting on access unit delimiters (NAL type 9) and recognising keyframes by IDR slices (type 5).

## Getting this wrong

If your work is here and the description is inaccurate, the licence is wrong, or you would rather not be listed — open an issue and it will be fixed.
