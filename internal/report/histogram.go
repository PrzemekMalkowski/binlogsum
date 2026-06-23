// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

// Package report renders a model.Summary as text or feeds it to the web UI.
package report

import (
	"time"

	"binlogsum/internal/model"
)

// Bucket is one column of the writes-over-time histogram.
type Bucket struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Rows     int       `json:"rows"`
	Bytes    int64     `json:"bytes"`
	Txns     int       `json:"txns"`
	Inserted int       `json:"inserted"`
	Updated  int       `json:"updated"`
	Deleted  int       `json:"deleted"`
}

// BuildBuckets distributes transactions into n time buckets spanning the
// binlog's [FirstTime, LastTime] window. A transaction lands in the bucket of
// its start time. When the window has zero duration (or no timestamps), a
// single bucket holds everything.
func BuildBuckets(s *model.Summary, n int) []Bucket {
	if n < 1 {
		n = 1
	}
	if !s.HasTime {
		return oneBucket(s)
	}
	span := s.LastTime.Sub(s.FirstTime)
	if span <= 0 {
		return oneBucket(s)
	}

	buckets := make([]Bucket, n)
	step := span / time.Duration(n)
	for i := range buckets {
		buckets[i].Start = s.FirstTime.Add(time.Duration(i) * step)
		buckets[i].End = s.FirstTime.Add(time.Duration(i+1) * step)
	}
	buckets[n-1].End = s.LastTime

	for _, t := range s.Transactions {
		idx := int(t.StartTime.Sub(s.FirstTime) / step)
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		add(&buckets[idx], t)
	}
	return buckets
}

func oneBucket(s *model.Summary) []Bucket {
	b := Bucket{Start: s.FirstTime, End: s.LastTime}
	for _, t := range s.Transactions {
		add(&b, t)
	}
	return []Bucket{b}
}

func add(b *Bucket, t *model.Transaction) {
	b.Txns++
	b.Inserted += t.Inserted
	b.Updated += t.Updated
	b.Deleted += t.Deleted
	b.Rows += t.RowsChanged()
	if t.SizeKnown {
		b.Bytes += t.SizeBytes
	}
}
