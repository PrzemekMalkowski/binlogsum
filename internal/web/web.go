// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"binlogsum/internal/model"
)

//go:embed index.html
var assets embed.FS

// snapshotMarker is the placeholder line in index.html that BuildSnapshot
// rewrites to embed the summary data for a self-contained file.
const snapshotMarker = "const SNAPSHOT = null; /*BINLOGSUM_SNAPSHOT*/"

// BuildSnapshot returns a single self-contained HTML document with the summary
// embedded, so it renders the same interactive UI offline with no server. The
// JSON is marshalled with HTML escaping on, so any "</script>" inside string
// values is neutralised.
func BuildSnapshot(s *model.Summary) ([]byte, error) {
	tmpl, err := assets.ReadFile("index.html")
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	repl := "const SNAPSHOT = " + string(data) + "; /*BINLOGSUM_SNAPSHOT*/"
	out := bytes.Replace(tmpl, []byte(snapshotMarker), []byte(repl), 1)
	if bytes.Equal(out, tmpl) {
		return nil, fmt.Errorf("snapshot marker not found in embedded UI template " +
			"(internal/web/index.html is out of date — rebuild with the updated template)")
	}
	return out, nil
}

// SnapshotFilename derives a download filename from the decoded-log source: the
// base name (path stripped), with a .sql/.log/.txt extension replaced by .html.
func SnapshotFilename(source string) string {
	base := source
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "stdin" {
		return "binlogsum-snapshot.html"
	}
	low := strings.ToLower(base)
	for _, ext := range []string{".sql", ".log", ".txt"} {
		if strings.HasSuffix(low, ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	return base + ".html"
}

// Serve starts a blocking HTTP server on addr exposing the UI, the
// /api/summary endpoint, and a downloadable /snapshot.
func Serve(addr string, s *model.Summary) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := assets.ReadFile("index.html")
		if err != nil {
			http.Error(w, "ui asset missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		b, err := BuildSnapshot(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition",
			`attachment; filename="`+SnapshotFilename(s.Source)+`"`)
		w.Write(b)
	})

	fmt.Printf("binlogsum web UI on http://%s  (Ctrl-C to stop)\n", addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	return srv.ListenAndServe()
}
