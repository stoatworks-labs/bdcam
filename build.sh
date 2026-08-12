#!/usr/bin/env bash
# Cross-compile bdcam for the PLAY (aarch64 Linux, Debian 10 / glibc 2.28).
#
# Same toolchain story as tools/bdkvm: purego needs cgo for dlopen on Linux, and
# macOS has no aarch64-linux gcc, so zig is the C compiler. Pinning the target to
# glibc 2.28 keeps the binary compatible with Debian 10 — without the pin, zig
# targets a newer glibc and the binary refuses to start on the device.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

command -v zig >/dev/null || { echo "error: zig not found (brew install zig)" >&2; exit 1; }
command -v go  >/dev/null || { echo "error: go not found" >&2; exit 1; }

mkdir -p dist
export CGO_ENABLED=1 GOOS=linux GOARCH=arm64
export CC="zig cc -target aarch64-linux-gnu.2.28"
export CXX="zig c++ -target aarch64-linux-gnu.2.28"

go build -trimpath -ldflags "-s -w" -o dist/bdcam-linux-arm64 .

echo "built: dist/bdcam-linux-arm64"
file dist/bdcam-linux-arm64
echo "size:  $(du -h dist/bdcam-linux-arm64 | cut -f1)"
echo "glibc: $(strings -a dist/bdcam-linux-arm64 | grep -oE 'GLIBC_2\.[0-9]+' | sort -uV | tail -1) (device has 2.28)"
