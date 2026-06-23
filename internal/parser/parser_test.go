// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

package parser

import (
	"os"
	"strings"
	"testing"
)

func TestParseSample(t *testing.T) {
	f, err := os.Open("../../testdata/sample.binlog")
	if err != nil {
		t.Fatalf("open sample: %v", err)
	}
	defer f.Close()

	s, err := Parse(f, "sample.binlog", "0.1.0")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if s.ServerVersion != "8.0.34" {
		t.Errorf("server version = %q, want 8.0.34", s.ServerVersion)
	}
	if s.Flavor != "MySQL" {
		t.Errorf("flavor = %q, want MySQL", s.Flavor)
	}
	if s.ServerIDs[100] == 0 {
		t.Errorf("expected events from server id 100")
	}
	if s.ServerIDs[200] == 0 {
		t.Errorf("expected events from server id 200")
	}
	if !s.HasXid || s.XidMin != 33 || s.XidMax != 35 {
		t.Errorf("xid range = [%d,%d] hasXid=%v, want [33,35] true", s.XidMin, s.XidMax, s.HasXid)
	}
	if !strings.HasSuffix(s.GTIDFirst, ":10") || !strings.HasSuffix(s.GTIDLast, ":14") {
		t.Errorf("gtid range = %q..%q, want ...:10 .. ...:14", s.GTIDFirst, s.GTIDLast)
	}

	wantQ := map[string]int{"INSERT": 7, "UPDATE": 2, "DELETE": 1, "ALTER": 1, "CREATE": 1, "DROP": 1}
	for k, v := range wantQ {
		if s.QueryCounts[k] != v {
			t.Errorf("query count %s = %d, want %d", k, s.QueryCounts[k], v)
		}
	}

	if len(s.Transactions) != 6 {
		t.Fatalf("transactions = %d, want 6 (3 DML + 3 DDL)", len(s.Transactions))
	}
	tx0 := s.Transactions[0]
	if tx0.Format != "ROW" || !tx0.SizeKnown || tx0.SizeBytes != 146 {
		t.Errorf("txn0 = {fmt:%s sizeKnown:%v size:%d}, want {ROW true 146}",
			tx0.Format, tx0.SizeKnown, tx0.SizeBytes)
	}
	if tx0.Inserted != 3 || tx0.Xid != 33 {
		t.Errorf("txn0 inserted=%d xid=%d, want 3 33", tx0.Inserted, tx0.Xid)
	}
	if tx0.GTID == "" || !strings.HasSuffix(tx0.GTID, ":10") {
		t.Errorf("txn0 gtid = %q, want ...:10", tx0.GTID)
	}

	// DDL statements must appear as their own transaction entries.
	ddl := map[string]string{} // kind -> table
	ddlCount := 0
	for _, tx := range s.Transactions {
		if tx.DDL == 1 {
			ddlCount++
			ddl[tx.DDLKind] = ""
			if len(tx.Tables) > 0 {
				ddl[tx.DDLKind] = tx.Tables[0]
			}
			if tx.Format != "DDL" {
				t.Errorf("DDL txn %q has format %q, want DDL", tx.DDLKind, tx.Format)
			}
		}
	}
	if ddlCount != 3 {
		t.Errorf("DDL transactions = %d, want 3", ddlCount)
	}
	if ddl["CREATE"] != "test.audit" {
		t.Errorf("CREATE table = %q, want test.audit", ddl["CREATE"])
	}
	if ddl["ALTER"] != "test.t1" {
		t.Errorf("ALTER table = %q, want test.t1", ddl["ALTER"])
	}

	if len(s.Tables) == 0 || s.Tables[0].Table != "test.orders" {
		got := "<none>"
		if len(s.Tables) > 0 {
			got = s.Tables[0].Table
		}
		t.Errorf("top table = %s, want test.orders", got)
	}

	if got := s.TotalRows(); got != 10 {
		t.Errorf("total rows = %d, want 10", got)
	}
}

func TestTruncateTableExtraction(t *testing.T) {
	input := "#260601 10:00:00 server id 1  end_log_pos 100 \tQuery\tthread_id=1\texec_time=0\terror_code=0\n" +
		"SET TIMESTAMP=1/*!*/;\n" +
		"TRUNCATE sbtest5\n" +
		"/*!*/;\n" +
		"# at 200\n" +
		"#260601 10:00:01 server id 1  end_log_pos 200 \tQuery\tthread_id=1\texec_time=0\terror_code=0\n" +
		"TRUNCATE TABLE sbtest4\n" +
		"/*!*/;\n" +
		"# at 300\n"
	s, err := Parse(strings.NewReader(input), "t", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tx := range s.Transactions {
		if tx.DDLKind == "TRUNCATE" && len(tx.Tables) > 0 {
			got[tx.Tables[0]] = true
		}
	}
	if !got["sbtest5"] {
		t.Errorf("bare 'TRUNCATE sbtest5' did not capture its table; got %v", got)
	}
	if !got["sbtest4"] {
		t.Errorf("'TRUNCATE TABLE sbtest4' did not capture its table; got %v", got)
	}
	if s.QueryCounts["TRUNCATE"] != 2 {
		t.Errorf("TRUNCATE count = %d, want 2", s.QueryCounts["TRUNCATE"])
	}
}
