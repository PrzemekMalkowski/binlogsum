// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// countingReader wraps an io.Reader and tracks how many bytes have been read so
// far, so a progress reporter can observe parsing throughput without the parser
// needing to know anything about it.
type countingReader struct {
	r io.Reader
	n int64 // bytes read so far (atomic)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(&c.n, int64(n))
	return n, err
}

func (c *countingReader) count() int64 { return atomic.LoadInt64(&c.n) }

// progress draws a single-line progress indicator to stderr while work runs.
// When total > 0 (a regular file) it shows a percentage bar; otherwise (a pipe)
// it shows bytes processed and throughput. It is a no-op when stderr is not a
// terminal, so redirected output is never polluted with carriage returns.
type progress struct {
	cr    *countingReader
	total int64
	start time.Time
	done  chan struct{}
	tty   bool
}

func startProgress(cr *countingReader, total int64) *progress {
	p := &progress{
		cr:    cr,
		total: total,
		start: time.Now(),
		done:  make(chan struct{}),
		tty:   isTerminal(os.Stderr),
	}
	if !p.tty {
		return p // no-op; Stop closes the channel harmlessly
	}
	go p.loop()
	return p
}

func (p *progress) loop() {
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
			p.render(false)
		}
	}
}

func (p *progress) render(final bool) {
	read := p.cr.count()
	elapsed := time.Since(p.start).Seconds()
	rate := float64(read)
	if elapsed > 0 {
		rate = float64(read) / elapsed
	}

	var line string
	if p.total > 0 {
		frac := float64(read) / float64(p.total)
		if frac > 1 {
			frac = 1
		}
		const w = 28
		filled := int(frac * w)
		bar := strings.Repeat("█", filled) + strings.Repeat("░", w-filled)
		line = fmt.Sprintf("  parsing [%s] %3.0f%%  %s / %s  %s/s",
			bar, frac*100, humanBytes(read), humanBytes(p.total), humanBytes(int64(rate)))
	} else {
		line = fmt.Sprintf("  parsing  %s  %s/s", humanBytes(read), humanBytes(int64(rate)))
	}
	// Pad to overwrite any longer previous line, then carriage-return.
	fmt.Fprintf(os.Stderr, "\r%-72s", line)
	if final {
		fmt.Fprint(os.Stderr, "\n")
	}
}

// Stop ends the reporter and clears/finalizes the line.
func (p *progress) Stop() {
	close(p.done)
	if p.tty {
		p.render(true)
	}
}

// humanBytes mirrors the reporter's formatting for progress output.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// isTerminal reports whether f is attached to a character device (a TTY),
// using only the standard library.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
