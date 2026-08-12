package main

// SRT output. The PLAY has libsrt only statically linked inside PPApp, so
// there is nothing to dlopen — this uses a pure-Go SRT implementation instead,
// which also keeps the cross-compile free of C dependencies.
//
// Caller mode only for now: we dial out to a receiver. That is the common
// direction for a camera feeding an ingest, and it avoids needing an inbound
// hole to the device.

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	srt "github.com/datarhei/gosrt"
)

// SRT carries MPEG-TS in payloads that are conventionally 7 packets of 188
// bytes — 1316 — which is the largest multiple of 188 that fits comfortably
// inside a normal MTU. Receivers are much happier with whole packets.
const tsPacketsPerDatagram = 7
const srtPayloadSize = tsPacketsPerDatagram * tsPacketSize

type SRTSender struct {
	conn    srt.Conn
	pending []byte
	target  string
}

// DialSRT connects to srt://host:port[?streamid=..&passphrase=..&latency=ms].
func DialSRT(raw string) (*SRTSender, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad SRT url %q: %w", raw, err)
	}
	if u.Scheme != "srt" {
		return nil, fmt.Errorf("SRT url must start with srt://, got %q", u.Scheme)
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("SRT url needs an explicit port: %q", raw)
	}

	cfg := srt.DefaultConfig()
	q := u.Query()
	if v := q.Get("streamid"); v != "" {
		cfg.StreamId = v
	}
	if v := q.Get("passphrase"); v != "" {
		cfg.Passphrase = v
	}
	if v := q.Get("latency"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("bad latency %q: %w", v, err)
		}
		cfg.Latency = time.Duration(ms) * time.Millisecond
	}
	cfg.PayloadSize = srtPayloadSize

	addr := u.Host
	conn, err := srt.Dial("srt", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("SRT dial %s: %w", addr, err)
	}
	logf("SRT connected to %s (streamid=%q, latency=%s)", addr, cfg.StreamId, cfg.Latency)
	return &SRTSender{conn: conn, target: addr, pending: make([]byte, 0, srtPayloadSize)}, nil
}

// Write accepts whole TS packets from the muxer and forwards them in
// payload-sized groups. Anything that is not a multiple of 188 is a muxer bug,
// so say so rather than quietly shifting the alignment.
func (s *SRTSender) Write(p []byte) (int, error) {
	if len(p)%tsPacketSize != 0 {
		return 0, fmt.Errorf("SRT got %d bytes, not a whole number of TS packets", len(p))
	}
	n := len(p)
	for len(p) > 0 {
		take := srtPayloadSize - len(s.pending)
		if take > len(p) {
			take = len(p)
		}
		s.pending = append(s.pending, p[:take]...)
		p = p[take:]
		if len(s.pending) == srtPayloadSize {
			if _, err := s.conn.Write(s.pending); err != nil {
				return 0, fmt.Errorf("SRT write: %w", err)
			}
			s.pending = s.pending[:0]
		}
	}
	return n, nil
}

func (s *SRTSender) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	if len(s.pending) > 0 {
		_, _ = s.conn.Write(s.pending)
		s.pending = s.pending[:0]
	}
	return s.conn.Close()
}
