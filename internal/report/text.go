// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"binlogsum/internal/model"
)

// palette holds the ANSI escape codes used by the text reporter. When color is
// disabled every field is the empty string, so the same template prints plain.
type palette struct {
	reset, dim, bold              string
	accent, good, warn, bad, head string
	ins, upd, del, bytes          string
}

func newPalette(color bool) palette {
	if !color {
		return palette{}
	}
	return palette{
		reset:  "\x1b[0m",
		dim:    "\x1b[38;5;245m",
		bold:   "\x1b[1m",
		accent: "\x1b[38;5;81m",  // cyan
		good:   "\x1b[38;5;114m", // green
		warn:   "\x1b[38;5;221m", // amber
		bad:    "\x1b[38;5;203m", // red
		head:   "\x1b[38;5;111m", // periwinkle
		ins:    "\x1b[38;5;114m", // green  (inserts)
		upd:    "\x1b[38;5;221m", // amber  (updates)
		del:    "\x1b[38;5;203m", // red    (deletes)
		bytes:  "\x1b[38;5;141m", // violet (size)
	}
}

const ruleWidth = 78

// Text writes a full human-readable report for s to w.
func Text(w io.Writer, s *model.Summary, buckets []Bucket, topTables int, color bool) {
	p := newPalette(color)
	writeHeader(w, p, s)
	writeOverview(w, p, s)
	writeQueryCounts(w, p, s)
	writeTopTables(w, p, s, topTables)
	writeHistogram(w, p, s, buckets)
	fmt.Fprintln(w)
}

func writeHeader(w io.Writer, p palette, s *model.Summary) {
	title := fmt.Sprintf(" binlogsum %s ", s.ToolVersion)
	bar := strings.Repeat("━", ruleWidth)
	fmt.Fprintf(w, "%s%s%s\n", p.accent, bar, p.reset)
	fmt.Fprintf(w, "%s%s%s%s  %ssource: %s%s\n",
		p.bold, p.head, title, p.reset, p.dim, s.Source, p.reset)
	fmt.Fprintf(w, "%s%s%s\n", p.accent, bar, p.reset)
}

func writeOverview(w io.Writer, p palette, s *model.Summary) {
	row := func(label, value string) {
		fmt.Fprintf(w, "  %s%-16s%s %s\n", p.dim, label, p.reset, value)
	}
	timeframe := "n/a"
	if s.HasTime {
		dur := s.LastTime.Sub(s.FirstTime)
		timeframe = fmt.Sprintf("%s  →  %s  %s(%s)%s",
			s.FirstTime.Format("2006-01-02 15:04:05"),
			s.LastTime.Format("2006-01-02 15:04:05"),
			p.dim, humanDuration(dur), p.reset)
	}
	row("time frame", timeframe)

	flavor := s.Flavor
	if s.ServerVersion != "" {
		flavor = fmt.Sprintf("%s %s%s%s", s.Flavor, p.bold, s.ServerVersion, p.reset)
	}
	row("server", flavor)
	row("events", fmt.Sprintf("%s%d%s total", p.bold, s.TotalEvents, p.reset))
	row("transactions", fmt.Sprintf("%s%d%s", p.bold, len(s.Transactions), p.reset))

	row("server ids", formatServerIDs(p, s.ServerIDs))

	if s.GTIDFirst != "" {
		g := s.GTIDFirst
		if s.GTIDLast != "" && s.GTIDLast != s.GTIDFirst {
			g = fmt.Sprintf("%s  →  %s", s.GTIDFirst, s.GTIDLast)
		}
		row("gtid range", g)
	}
	if s.HasXid {
		row("xid range", fmt.Sprintf("%d  →  %d", s.XidMin, s.XidMax))
	}
	row("rows changed", fmt.Sprintf("%s%s%s", p.bold, comma(s.TotalRows()), p.reset))
	if b := s.TotalBytes(); b > 0 {
		row("row-image size", fmt.Sprintf("%s%s%s", p.bytes, humanBytes(b), p.reset))
	}
	fmt.Fprintln(w)
}

func formatServerIDs(p palette, ids map[int]int) string {
	if len(ids) == 0 {
		return "n/a"
	}
	keys := make([]int, 0, len(ids))
	for k := range ids {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%d%s %s(%d ev)%s",
			p.bold, k, p.reset, p.dim, ids[k], p.reset))
	}
	return strings.Join(parts, "   ")
}

func writeQueryCounts(w io.Writer, p palette, s *model.Summary) {
	if len(s.QueryCounts) == 0 {
		return
	}
	section(w, p, "events by type")
	// Stable order: DML first in a fixed order, then any DDL alphabetically.
	order := []string{"INSERT", "UPDATE", "DELETE"}
	seen := map[string]bool{}
	var rest []string
	for k := range s.QueryCounts {
		if k != "INSERT" && k != "UPDATE" && k != "DELETE" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)

	max := 0
	for _, v := range s.QueryCounts {
		if v > max {
			max = v
		}
	}
	for _, k := range order {
		v, ok := s.QueryCounts[k]
		if !ok || seen[k] {
			continue
		}
		seen[k] = true
		color := p.accent
		switch k {
		case "INSERT":
			color = p.ins
		case "UPDATE":
			color = p.upd
		case "DELETE":
			color = p.del
		}
		bar := miniBar(v, max, 28)
		fmt.Fprintf(w, "  %s%-9s%s %s%s%s %s%s%s\n",
			p.dim, strings.ToLower(k), p.reset, color, bar, p.reset,
			p.bold, comma(v), p.reset)
	}
	fmt.Fprintln(w)
}

func writeTopTables(w io.Writer, p palette, s *model.Summary, top int) {
	if len(s.Tables) == 0 {
		return
	}
	section(w, p, "most-updated tables")
	if top <= 0 || top > len(s.Tables) {
		top = len(s.Tables)
	}

	// Size the name column to the longest displayed name, clamped to a sane
	// range; names longer than the cap wrap onto indented continuation lines.
	const nameCap = 64
	nameW := len("table")
	for i := 0; i < top; i++ {
		if l := len([]rune(s.Tables[i].Table)); l > nameW {
			nameW = l
		}
	}
	if nameW > nameCap {
		nameW = nameCap
	}

	fmt.Fprintf(w, "  %s%-*s %9s %9s %9s %9s%s\n",
		p.dim, nameW, "table", "ins", "upd", "del", "ddl", p.reset)
	for i := 0; i < top; i++ {
		t := s.Tables[i]
		lines := wrapRunes(t.Table, nameW)
		fmt.Fprintf(w, "  %-*s %s%9s%s %s%9s%s %s%9s%s %s%9s%s\n",
			nameW, lines[0],
			p.ins, comma(t.Inserted), p.reset,
			p.upd, comma(t.Updated), p.reset,
			p.del, comma(t.Deleted), p.reset,
			p.dim, comma(t.DDL), p.reset)
		for _, cont := range lines[1:] {
			fmt.Fprintf(w, "  %s%s%s\n", p.dim, cont, p.reset)
		}
	}
	fmt.Fprintln(w)
}

// wrapRunes splits s into chunks of at most width runes. A short string yields
// a single element.
func wrapRunes(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	r := []rune(s)
	if len(r) <= width {
		return []string{s}
	}
	var out []string
	for len(r) > width {
		out = append(out, string(r[:width]))
		r = r[width:]
	}
	return append(out, string(r))
}

var blocks = []rune{' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

func writeHistogram(w io.Writer, p palette, s *model.Summary, buckets []Bucket) {
	if len(buckets) == 0 {
		return
	}
	section(w, p, "writes over time")

	maxRows := 0
	var maxBytes int64
	for _, b := range buckets {
		if b.Rows > maxRows {
			maxRows = b.Rows
		}
		if b.Bytes > maxBytes {
			maxBytes = b.Bytes
		}
	}

	rowsLine := sparkline(buckets, func(b Bucket) float64 { return float64(b.Rows) }, maxRows == 0, float64(maxRows))
	bytesLine := sparkline(buckets, func(b Bucket) float64 { return float64(b.Bytes) }, maxBytes == 0, float64(maxBytes))

	fmt.Fprintf(w, "  %srows %s %s%s  %speak %s%s\n",
		p.dim, p.reset, p.good, rowsLine, p.dim, comma(maxRows), p.reset)
	fmt.Fprintf(w, "  %ssize %s %s%s  %speak %s%s\n",
		p.dim, p.reset, p.bytes, bytesLine, p.dim, humanBytes(maxBytes), p.reset)

	if s.HasTime {
		left := s.FirstTime.Format("15:04:05")
		right := s.LastTime.Format("15:04:05")
		width := len(buckets)
		gap := width - len(left) - len(right)
		if gap < 1 {
			gap = 1
		}
		fmt.Fprintf(w, "       %s%s%s%s%s\n",
			p.dim, left, strings.Repeat(" ", gap), right, p.reset)
	}
	fmt.Fprintf(w, "  %s%d buckets across the log window%s\n", p.dim, len(buckets), p.reset)
}

func sparkline(buckets []Bucket, val func(Bucket) float64, empty bool, max float64) string {
	var sb strings.Builder
	for _, b := range buckets {
		if empty || max == 0 {
			sb.WriteRune(blocks[0])
			continue
		}
		frac := val(b) / max
		idx := int(frac*float64(len(blocks)-1) + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		sb.WriteRune(blocks[idx])
	}
	return sb.String()
}

func miniBar(v, max, width int) string {
	if max == 0 {
		return strings.Repeat("·", 1)
	}
	n := int(float64(v) / float64(max) * float64(width))
	if n < 1 && v > 0 {
		n = 1
	}
	return strings.Repeat("█", n) + strings.Repeat("·", width-n)
}

func section(w io.Writer, p palette, title string) {
	fmt.Fprintf(w, "%s%s┃%s %s%s%s\n",
		p.bold, p.accent, p.reset, p.bold, title, p.reset)
}

// ---- formatting helpers ----

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func comma(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

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

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}
