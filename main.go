// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

// Command binlogsum produces a detailed summary of a MySQL/MariaDB binary log
// that has been decoded with:
//
//	mysqlbinlog --base64-output=DECODE-ROWS -v <binlog> | binlogsum
//	binlogsum --file decoded.log
//
// It reports header-level metadata (time frame, server flavor/version, server
// ids, GTID/Xid ranges, event-type counts, most-updated tables) and per
// transaction metadata, then renders a writes-over-time histogram either as a
// styled terminal report or an interactive web UI.
package main

import (
	"fmt"
	"io"
	"os"

	"binlogsum/internal/model"
	"binlogsum/internal/parser"
	"binlogsum/internal/report"
	"binlogsum/internal/web"
)

const version = "0.2.0"

type options struct {
	file    string
	mode    string
	addr    string
	buckets int
	top     int
	noColor bool
	version bool
	help    bool
	out     string
}

func parseFlags(args []string) (options, error) {
	o := options{mode: "text", addr: "127.0.0.1:8080", buckets: 60, top: 10}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--version" || a == "-v":
			o.version = true
		case a == "--help" || a == "-h":
			o.help = true
		case a == "--no-color":
			o.noColor = true
		case a == "--file" || a == "-f":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.file = v
		case a == "--mode" || a == "-m":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.mode = v
		case a == "--addr":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.addr = v
		case a == "--out" || a == "-o":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.out = v
		case a == "--buckets":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := atoi(v)
			if err != nil {
				return o, fmt.Errorf("--buckets: %v", err)
			}
			o.buckets = n
		case a == "--top":
			v, err := next()
			if err != nil {
				return o, err
			}
			n, err := atoi(v)
			if err != nil {
				return o, fmt.Errorf("--top: %v", err)
			}
			o.top = n
		default:
			return o, fmt.Errorf("unknown flag %q (try --help)", a)
		}
	}
	return o, nil
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "binlogsum:", err)
		os.Exit(2)
	}
	if o.version {
		fmt.Printf("binlogsum %s\n", version)
		return
	}
	if o.help {
		usage(os.Stdout)
		return
	}
	if o.mode != "text" && o.mode != "web" && o.mode != "snapshot" {
		fmt.Fprintf(os.Stderr, "binlogsum: --mode must be 'text', 'web' or 'snapshot', got %q\n", o.mode)
		os.Exit(2)
	}

	in, name, size, closeFn, err := openInput(o.file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "binlogsum:", err)
		os.Exit(1)
	}
	defer closeFn()

	cr := &countingReader{r: in}
	prog := startProgress(cr, size)
	sum, err := parser.Parse(cr, name, version)
	prog.Stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "binlogsum: parse error:", err)
		os.Exit(1)
	}

	switch o.mode {
	case "web":
		if err := web.Serve(o.addr, sum); err != nil {
			fmt.Fprintln(os.Stderr, "binlogsum:", err)
			os.Exit(1)
		}
	case "snapshot":
		if err := writeSnapshot(sum, o.out); err != nil {
			fmt.Fprintln(os.Stderr, "binlogsum:", err)
			os.Exit(1)
		}
	default:
		buckets := report.BuildBuckets(sum, o.buckets)
		report.Text(os.Stdout, sum, buckets, o.top, !o.noColor)
	}
}

// writeSnapshot renders a self-contained interactive HTML file. With an empty
// path it writes to stdout (so it can be piped or redirected).
func writeSnapshot(sum *model.Summary, out string) error {
	html, err := web.BuildSnapshot(sum)
	if err != nil {
		return err
	}
	if out == "" {
		_, err = os.Stdout.Write(html)
		return err
	}
	if err := os.WriteFile(out, html, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%s)\n", out, humanBytes(int64(len(html))))
	return nil
}

// openInput returns a reader for the decoded log: a file when --file is given,
// otherwise standard input (the streaming/pipe case). The returned size is the
// file size in bytes, or -1 when unknown (stdin).
func openInput(file string) (r io.Reader, name string, size int64, closeFn func(), err error) {
	if file == "" {
		return os.Stdin, "stdin", -1, func() {}, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, "", -1, func() {}, err
	}
	size = -1
	if fi, statErr := f.Stat(); statErr == nil {
		size = fi.Size()
	}
	return f, file, size, func() { f.Close() }, nil
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `binlogsum %s — detailed MySQL/MariaDB binary log summary

USAGE
  mysqlbinlog --base64-output=DECODE-ROWS -v mysql-bin.000123 | binlogsum
  binlogsum --file decoded.log --mode web
  binlogsum --file decoded.log --mode snapshot --out report.html

OPTIONS
  -f, --file PATH     read decoded binlog from PATH (default: stdin)
  -m, --mode MODE     output mode: text (default), web, or snapshot
      --addr ADDR     web mode listen address (default 127.0.0.1:8080)
  -o, --out PATH      snapshot mode output file (default: stdout)
      --buckets N     histogram buckets in text mode (default 60)
      --top N         number of tables in the "most-updated" table (default 10)
      --no-color      disable ANSI colors in text mode
  -v, --version       print version and exit
  -h, --help          show this help

MODES
  text      styled terminal report (default)
  web       interactive UI with a zoomable histogram; also offers a
            self-contained snapshot download
  snapshot  write a single self-contained interactive HTML file (same UI as
            web mode, no server needed) to --out or stdout

INPUT
  Feed the output of: mysqlbinlog --base64-output=DECODE-ROWS -v
  Works with ROW (FULL or MINIMAL image) and STATEMENT formats. For STATEMENT
  transactions, per-transaction byte size is not measured.
`, version)
}
