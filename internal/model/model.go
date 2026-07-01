// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

// Package model holds the shared data structures produced by the parser and
// consumed by the text and web reporters.
package model

import "time"

// TableStat aggregates per-table write activity across the whole binlog.
type TableStat struct {
	Table    string `json:"table"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Deleted  int    `json:"deleted"`
	DDL      int    `json:"ddl"`
}

// RowsChanged is the total number of row-level modifications for the table.
func (t *TableStat) RowsChanged() int { return t.Inserted + t.Updated + t.Deleted }

// Transaction is the metadata collected for one logged transaction
// (BEGIN .. COMMIT/Xid) or, for non-transactional row events, one row group.
type Transaction struct {
	Index     int       `json:"index"`
	GTID      string    `json:"gtid,omitempty"`
	Xid       int64     `json:"xid,omitempty"`
	ServerID  int       `json:"server_id"`
	StartPos  int64     `json:"start_pos"`
	EndPos    int64     `json:"end_pos"`
	SizeBytes int64     `json:"size_bytes"` // valid only when SizeKnown
	SizeKnown bool      `json:"size_known"` // false for STATEMENT-format txns
	Format    string    `json:"format"`     // "ROW", "STATEMENT", "DDL" or "DCL"
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Inserted  int       `json:"inserted"`
	Updated   int       `json:"updated"`
	Deleted   int       `json:"deleted"`
	DDL       int       `json:"ddl,omitempty"`
	DDLKind   string    `json:"ddl_kind,omitempty"`
	Tables          []string `json:"tables"`                      // tables with actual row changes
	TablesReferenced []string `json:"tables_referenced,omitempty"` // mapped via FK refs but unmodified
	Queries         []string `json:"queries,omitempty"`           // original SQL (Rows_query / DDL / DCL)
}

// RowsChanged is the number of individual rows touched by the transaction.
func (t *Transaction) RowsChanged() int { return t.Inserted + t.Updated + t.Deleted }

// Summary is the complete result of analysing a decoded binary log.
type Summary struct {
	ToolVersion   string         `json:"tool_version"`
	Source        string         `json:"source"`
	FirstTime     time.Time      `json:"first_time"`
	LastTime      time.Time      `json:"last_time"`
	HasTime       bool           `json:"has_time"`
	ServerVersion string         `json:"server_version"`
	Flavor        string         `json:"flavor"`
	ServerIDs     map[int]int    `json:"server_ids"`
	GTIDFirst     string         `json:"gtid_first,omitempty"`
	GTIDLast      string         `json:"gtid_last,omitempty"`
	XidMin        int64          `json:"xid_min"`
	XidMax        int64          `json:"xid_max"`
	HasXid        bool           `json:"has_xid"`
	TotalEvents   int            `json:"total_events"`
	QueryCounts   map[string]int `json:"query_counts"`
	Tables        []*TableStat   `json:"tables"`
	Transactions  []*Transaction `json:"transactions"`
}

// TotalRows sums all row modifications recorded across transactions.
func (s *Summary) TotalRows() int {
	n := 0
	for _, t := range s.Transactions {
		n += t.RowsChanged()
	}
	return n
}

// TotalBytes sums transaction sizes for which a size is known.
func (s *Summary) TotalBytes() int64 {
	var n int64
	for _, t := range s.Transactions {
		if t.SizeKnown {
			n += t.SizeBytes
		}
	}
	return n
}
