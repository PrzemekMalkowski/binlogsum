// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Przemysław Malkowski
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. See the LICENSE file for the full license text.

// Package parser turns the textual output of
//
//	mysqlbinlog --base64-output=DECODE-ROWS -v
//
// into a model.Summary. It is tolerant of both MySQL and MariaDB output and
// of ROW (FULL or MINIMAL image) and STATEMENT formats.
//
// The scanner works on the raw []byte of each line (never allocating a string
// per line) and recognises the common line shapes — row-image lines ("### ...")
// and event headers — by a cheap first-byte/prefix test, hand-parsing the
// fields. Strings are allocated only for values that are retained (table names,
// GTIDs, server version, current database). Regular expressions are reserved
// for rare lines (Format_description, MariaDB GTID, DDL statements).
package parser

import (
	"bufio"
	"bytes"
	"io"
	"sort"
	"time"

	"binlogsum/internal/model"
)

// Reasonable upper bound for a single decoded line. Row events with wide
// columns can be large, so we give the scanner plenty of headroom.
const maxLineBytes = 16 * 1024 * 1024

// Only rare lines still use regexp: the Start (Format_description) event, the
// MariaDB GTID event, and DDL table extraction.
var (
	reStart     = mustMatch(`Start:\s+binlog v \d+, server v ([^\s,]+)`)
	reMariaGTID = mustMatch(`GTID\s+(\d+-\d+-\d+)`)
	reDDLTable  = mustMatch("(?i)\\b(?:TABLE|VIEW|INDEX)\\b(?:\\s+IF\\s+(?:NOT\\s+)?EXISTS)?\\s+`?(?:([\\w$]+)`?\\.`?)?([\\w$]+)")
	// TRUNCATE allows omitting the TABLE keyword ("TRUNCATE tbl"); used as a
	// fallback when reDDLTable finds no TABLE/VIEW/INDEX anchor.
	reTruncBare = mustMatch("(?i)^TRUNCATE\\s+`?(?:([\\w$]+)`?\\.`?)?([\\w$]+)")
)

// Fixed needles searched within event lines; declared once to avoid per-call
// []byte allocation.
var (
	nServerID   = []byte("server id ")
	nEndLogPos  = []byte("end_log_pos ")
	nCRC32      = []byte("CRC32 ")
	nTableID    = []byte("table id ")
	nMappedTo   = []byte("mapped to number ")
)

// Parse reads decoded binlog text from r and returns the assembled summary.
// sourceName is recorded for display (e.g. a file name or "stdin").
func Parse(r io.Reader, sourceName, toolVersion string) (*model.Summary, error) {
	p := &parserState{
		sum: &model.Summary{
			ToolVersion: toolVersion,
			Source:      sourceName,
			ServerIDs:   map[int]int{},
			QueryCounts: map[string]int{},
			Flavor:      "MySQL",
		},
		tableMap:  map[int]string{},
		tableStat: map[string]*model.TableStat{},
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for sc.Scan() {
		p.line(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	p.finish()
	return p.sum, nil
}

// txnState carries both the public transaction and parser-only bookkeeping.
type txnState struct {
	txn        *model.Transaction
	tables     map[string]bool // tables actually modified (from "### " row images)
	mapped     map[string]bool // every Table_map'd table (incl. FK-referenced)
	awaitStart bool            // next "# at" sets StartPos
	committed  bool            // COMMIT/ROLLBACK seen; next "# at" closes it
	curTable   string
}

type parserState struct {
	sum       *model.Summary
	tableMap  map[int]string
	tableStat map[string]*model.TableStat

	cur         *txnState
	pendingGTID string
	currentDB   string
	curServerID int
	curTime     time.Time
	curTimeOK   bool
	lastAt      int64
	txnIndex    int

	gtidSeen []string
	xids     []int64

	tblKeyBuf []byte // reused scratch for building "db.tbl" keys

	inRowsQuery  bool   // collecting a Rows_query event's "# <sql>" lines
	rowsQueryBuf []byte // accumulates the current Rows_query text

	capTxn *model.Transaction // statement txn (DDL/DCL) capturing its SQL, or nil
	capBuf []byte             // accumulates a multi-line DDL/DCL statement
}

// line dispatches a single input line by its first byte. The ordering favours
// the most frequent line shapes in a row-formatted binlog. b is only valid for
// the duration of this call (it is the scanner's reused buffer).
func (p *parserState) line(b []byte) {
	if len(b) == 0 {
		return
	}
	// While collecting a Rows_query, its original SQL follows on one or more
	// "# <sql>" lines. Consume those here; any other line shape ends the query
	// and falls through to normal handling. ("# at " is an event boundary, not
	// query text, so it is explicitly excluded.)
	if p.inRowsQuery {
		if len(b) >= 2 && b[0] == '#' && b[1] == ' ' && !hasPrefix(b, "# at ") {
			p.appendRowsQuery(b)
			return
		}
		p.endRowsQuery()
	}
	if b[0] == '#' {
		// An event boundary ends any statement still being captured (normally
		// the "/*!*/;" terminator already did).
		if p.capTxn != nil {
			p.finishStmtCapture()
		}
		// "### ..." — row-image pseudo-statements (by far the most common).
		if len(b) >= 3 && b[1] == '#' && b[2] == '#' {
			p.rowLine(b)
			return
		}
		// "# at N" — position marker / transaction boundary.
		if hasPrefix(b, "# at ") {
			pos := int64(atoiLeading(b[5:]))
			p.lastAt = pos
			if p.cur != nil && p.cur.committed && p.cur.txn.EndPos == 0 {
				p.closeTxn(pos)
			} else if p.cur != nil && p.cur.awaitStart {
				p.cur.txn.StartPos = pos
				p.cur.awaitStart = false
			}
			return
		}
		// "#YYMMDD HH:MM:SS server id ..." — an event header.
		if len(b) >= 2 && b[1] >= '0' && b[1] <= '9' {
			p.eventHeader(b)
			return
		}
		// Any other "# ..." comment line is ignored.
		return
	}
	p.payload(b)
}

// rowLine handles the "### ..." family. Only the three statement-introducing
// forms carry row counts; the "### SET/WHERE/@n=..." decoration is ignored.
// The table a row change is attributed to is read from the statement line
// itself ("### UPDATE `db`.`tbl`"), which is authoritative: a single
// transaction routinely touches many tables, and the line names the exact one.
func (p *parserState) rowLine(b []byte) {
	switch {
	case hasPrefix(b, "### INSERT INTO"):
		p.sum.QueryCounts["INSERT"]++
		if p.cur != nil {
			p.cur.txn.Inserted++
			p.imageStat(b).Inserted++
		}
	case hasPrefix(b, "### UPDATE"):
		p.sum.QueryCounts["UPDATE"]++
		if p.cur != nil {
			p.cur.txn.Updated++
			p.imageStat(b).Updated++
		}
	case hasPrefix(b, "### DELETE FROM"):
		p.sum.QueryCounts["DELETE"]++
		if p.cur != nil {
			p.cur.txn.Deleted++
			p.imageStat(b).Deleted++
		}
	}
}

// imageStat returns the aggregate record for the table named on a row-image
// statement line, creating it on first use and recording it on the current
// transaction's table set. It falls back to the table-map-derived curTable when
// the line carries no parseable name (so unusual lines are still counted, under
// "(unknown)" at worst). The hot path performs no allocation for tables already
// seen: the "db.tbl" key is built in a reused buffer and looked up via the
// compiler's allocation-free map[string([]byte)] special case.
func (p *parserState) imageStat(b []byte) *model.TableStat {
	db, tbl, ok := imageTable(b)
	if !ok {
		// No parseable name on the line: fall back to the table-map-derived
		// curTable, and record it on the transaction's table set (the row-event
		// path no longer marks tables).
		if p.cur.curTable != "" {
			p.markTable(p.cur.curTable)
		}
		return p.stat(p.cur.curTable)
	}

	buf := p.tblKeyBuf[:0]
	if len(db) == 0 && p.currentDB != "" {
		buf = append(buf, p.currentDB...)
	} else {
		buf = append(buf, db...)
	}
	if len(buf) > 0 {
		buf = append(buf, '.')
	}
	buf = append(buf, tbl...)
	p.tblKeyBuf = buf

	s := p.tableStat[string(buf)] // allocation-free lookup
	if s == nil {
		key := string(buf) // allocate once, when the table is first seen
		s = &model.TableStat{Table: key}
		p.tableStat[key] = s
	}
	// Keep the transaction's table set consistent with what actually changed.
	if !p.cur.tables[string(buf)] {
		p.cur.tables[s.Table] = true
	}
	return s
}

// imageTable extracts the database and table from a row-image statement line
// such as "### UPDATE `db`.`tbl`" or "### INSERT INTO `tbl`". db is nil when the
// name is unqualified. It mirrors parseTableMap's backtick scanning and shares
// its limitation around identifiers containing escaped backticks (not produced
// for ordinary table names).
func imageTable(b []byte) (db, tbl []byte, ok bool) {
	a := bytes.IndexByte(b, '`')
	if a < 0 {
		return nil, nil, false
	}
	rel := bytes.IndexByte(b[a+1:], '`')
	if rel < 0 {
		return nil, nil, false
	}
	end1 := a + 1 + rel
	first := b[a+1 : end1]

	rest := b[end1+1:]
	if len(rest) > 0 && rest[0] == '.' {
		c := bytes.IndexByte(rest, '`')
		if c >= 0 {
			if r2 := bytes.IndexByte(rest[c+1:], '`'); r2 >= 0 {
				return first, rest[c+1 : c+1+r2], true // db, tbl
			}
		}
	}
	return nil, first, true // unqualified: table only
}

// appendRowsQuery adds a "# <sql>" continuation line to the in-progress
// Rows_query, stripping the leading "# " and joining multiple lines with "\n".
func (p *parserState) appendRowsQuery(b []byte) {
	q := stripHashSpace(b)
	if len(p.rowsQueryBuf) > 0 {
		p.rowsQueryBuf = append(p.rowsQueryBuf, '\n')
	}
	p.rowsQueryBuf = append(p.rowsQueryBuf, q...)
}

// endRowsQuery finalises a collected Rows_query, attaching it to the current
// transaction (one entry per statement). It is safe to call when no query is
// being collected.
func (p *parserState) endRowsQuery() {
	if !p.inRowsQuery {
		return
	}
	p.inRowsQuery = false
	if len(p.rowsQueryBuf) == 0 {
		return
	}
	q := string(p.rowsQueryBuf)
	p.rowsQueryBuf = p.rowsQueryBuf[:0]
	if p.cur != nil {
		p.cur.txn.Queries = append(p.cur.txn.Queries, q)
	}
}

// stripHashSpace drops a leading "# " (or just "#") from a comment line.
func stripHashSpace(b []byte) []byte {
	if len(b) > 0 && b[0] == '#' {
		b = b[1:]
	}
	if len(b) > 0 && b[0] == ' ' {
		b = b[1:]
	}
	return b
}

// eventHeader hand-scans a header line of the form:
//
//	#YYMMDD HH:MM:SS server id N  end_log_pos P [CRC32 0x..] <description>
//
// and then dispatches on the description prefix.
func (p *parserState) eventHeader(b []byte) {
	if len(b) < 8 {
		return
	}
	yymmdd := b[1:7]
	if !allDigits(yymmdd) {
		return // not actually an event header
	}
	p.sum.TotalEvents++

	rest := b[7:]
	i := 0
	for i < len(rest) && rest[i] == ' ' {
		i++
	}
	tStart := i
	for i < len(rest) && rest[i] != ' ' {
		i++
	}
	hms := rest[tStart:i]
	rem := rest[i:]

	sid := 0
	if k := bytes.Index(rem, nServerID); k >= 0 {
		sid = atoiLeading(rem[k+len(nServerID):])
	}
	p.sum.ServerIDs[sid]++
	p.curServerID = sid

	if t, ok := parseEventTime(yymmdd, hms); ok {
		p.curTime = t
		p.curTimeOK = true
		if !p.sum.HasTime {
			p.sum.FirstTime = t
			p.sum.HasTime = true
		}
		p.sum.LastTime = t
	}

	p.dispatchDesc(extractDesc(rem))
}

// dispatchDesc routes an event description to the right handler by its prefix,
// so a typical event runs at most one targeted parse instead of several
// regex probes.
func (p *parserState) dispatchDesc(desc []byte) {
	switch {
	case hasPrefix(desc, "Table_map:"):
		// Record the id -> table mapping. Do NOT add it to the modified-tables
		// set: since MySQL 9.7 (USE_SQL_FOREIGN_KEY_F) a transaction emits a
		// Table_map for every FK-referenced table even when no row in it changes.
		// The modified set is built from actual row images (see imageStat); the
		// full mapped set is kept separately so referenced-only tables can be
		// reported apart from updated ones.
		if full, num, ok := parseTableMap(desc); ok {
			p.tableMap[num] = full
			if p.cur != nil {
				p.cur.mapped[full] = true
			}
		}
	case hasPrefix(desc, "Write_rows"),
		hasPrefix(desc, "Update_rows"),
		hasPrefix(desc, "Delete_rows"):
		// curTable is only a fallback for row-image lines that carry no parseable
		// table name; the table set and per-table stats come from the "### " line.
		num := parseTableID(desc)
		if p.cur != nil {
			p.cur.txn.Format = "ROW"
			p.cur.curTable = p.tableMap[num]
		}
	case hasPrefix(desc, "Rows_query"):
		// binlog_rows_query_log_events: the original SQL follows on the next
		// "# <sql>" line(s); line() collects it into the current transaction.
		p.inRowsQuery = true
		p.rowsQueryBuf = p.rowsQueryBuf[:0]
	case hasPrefix(desc, "Xid ="):
		x := int64(atoiLeading(bytes.TrimSpace(desc[len("Xid ="):])))
		p.xids = append(p.xids, x)
		if p.cur != nil {
			p.cur.txn.Xid = x
		}
	case hasPrefix(desc, "Start:"):
		if m := reStart.FindSubmatch(desc); m != nil {
			p.sum.ServerVersion = string(m[1])
			if bytes.Contains(m[1], []byte("MariaDB")) {
				p.sum.Flavor = "MariaDB"
			}
		}
	case hasPrefix(desc, "GTID "):
		// MariaDB GTID event: "GTID 0-100-45 ...". MySQL's GTID event uses a tab
		// ("GTID\t...") and is handled via the SET @@SESSION.GTID_NEXT line.
		if m := reMariaGTID.FindSubmatch(desc); m != nil {
			g := string(m[1])
			p.gtidSeen = append(p.gtidSeen, g)
			p.pendingGTID = g
			p.sum.Flavor = "MariaDB"
		}
	}
}

// payload handles non-"#" lines: SQL statements and session settings.
func (p *parserState) payload(b []byte) {
	// If a DDL/DCL statement is being captured, accumulate its text until the
	// "/*!*/;" terminator. (SET/use directives precede the statement, so they
	// never reach here mid-capture.)
	if p.capTxn != nil {
		if hasPrefix(b, "/*!*/;") {
			p.finishStmtCapture()
			return
		}
		p.capBuf = append(p.capBuf, '\n')
		p.capBuf = append(p.capBuf, b...)
		return
	}

	switch b[0] {
	case 'S':
		if hasPrefix(b, "SET @@SESSION.GTID_NEXT") {
			if v, ok := singleQuoted(b); ok && !equalFold(v, "AUTOMATIC") {
				g := string(v)
				p.pendingGTID = g
				p.gtidSeen = append(p.gtidSeen, g)
			}
		}
		return
	case 'B':
		if hasPrefix(b, "BEGIN") {
			p.beginTxn()
		}
		return
	case 'C':
		if hasPrefix(b, "COMMIT") {
			p.commitTxn()
			return
		}
	case 'R':
		if hasPrefix(b, "ROLLBACK") {
			p.commitTxn()
			return
		}
	case 'u', 'U':
		if hasPrefix(b, "use ") || hasPrefix(b, "USE ") {
			p.currentDB = extractUseDB(b)
			return
		}
	}

	// Standalone statement (outside a BEGIN/COMMIT) — STATEMENT format. Reached
	// for a non-COMMIT 'C' (CREATE) or non-ROLLBACK 'R' (RENAME/REVOKE) too, via
	// fallthrough. DDL and DCL are each recorded as their own transaction and
	// have their original SQL captured.
	if p.cur == nil {
		if kw, ok := leadingDDL(b); ok {
			p.sum.QueryCounts[kw]++
			table := p.recordDDLTable(b, kw)
			t := p.addStmtTxn(kw, "DDL", table)
			p.startStmtCapture(t, b)
		} else if kw, ok := leadingDCL(b); ok {
			p.sum.QueryCounts[kw]++
			t := p.addStmtTxn(kw, "DCL", "")
			p.startStmtCapture(t, b)
		}
	}
}

// addStmtTxn records a standalone statement (DDL or DCL) as its own transaction
// entry so it is visible (and sortable) alongside DML in the per-transaction
// views. In the binlog each such statement is auto-committed under its own GTID.
// class is "DDL" or "DCL" (stored in Format); only DDL sets the DDL flag.
func (p *parserState) addStmtTxn(kind, class, table string) *model.Transaction {
	t := &model.Transaction{
		Index:    p.txnIndex,
		ServerID: p.curServerID,
		Format:   class,
		GTID:     p.pendingGTID,
		DDLKind:  kind,
	}
	if class == "DDL" {
		t.DDL = 1
	}
	if p.curTimeOK {
		t.StartTime = p.curTime
		t.EndTime = p.curTime
	}
	if table != "" {
		t.Tables = []string{table}
	}
	p.txnIndex++
	p.pendingGTID = ""
	p.sum.Transactions = append(p.sum.Transactions, t)
	return t
}

// startStmtCapture begins accumulating a statement's SQL, seeded with its first
// line. Continuation lines are appended in payload until the "/*!*/;" terminator.
func (p *parserState) startStmtCapture(t *model.Transaction, firstLine []byte) {
	p.capTxn = t
	p.capBuf = append(p.capBuf[:0], firstLine...)
}

// finishStmtCapture attaches the accumulated SQL to the statement transaction.
// Safe to call when no capture is active (e.g. at an event boundary or EOF).
func (p *parserState) finishStmtCapture() {
	if p.capTxn == nil {
		return
	}
	if len(p.capBuf) > 0 {
		p.capTxn.Queries = append(p.capTxn.Queries, string(p.capBuf))
	}
	p.capBuf = p.capBuf[:0]
	p.capTxn = nil
}

func (p *parserState) commitTxn() {
	if p.cur != nil {
		p.cur.committed = true
		if p.curTimeOK {
			p.cur.txn.EndTime = p.curTime
		}
	}
}

func (p *parserState) beginTxn() {
	ts := &txnState{
		txn: &model.Transaction{
			Index:    p.txnIndex,
			ServerID: p.curServerID,
			Format:   "STATEMENT",
			GTID:     p.pendingGTID,
		},
		tables:     map[string]bool{},
		mapped:     map[string]bool{},
		awaitStart: true,
	}
	if p.curTimeOK {
		ts.txn.StartTime = p.curTime
		ts.txn.EndTime = p.curTime
	}
	p.txnIndex++
	p.pendingGTID = ""
	p.cur = ts
}

func (p *parserState) closeTxn(pos int64) {
	t := p.cur.txn
	t.EndPos = pos
	if t.Format == "ROW" && t.StartPos > 0 && pos > t.StartPos {
		t.SizeBytes = pos - t.StartPos
		t.SizeKnown = true
	}
	t.Tables = sortedKeys(p.cur.tables)
	t.TablesReferenced = referencedKeys(p.cur.mapped, p.cur.tables)
	p.sum.Transactions = append(p.sum.Transactions, t)
	p.cur = nil
}

// referencedKeys returns the sorted tables that were Table_map'd but not among
// the modified set (i.e. present only because of FK references).
func referencedKeys(mapped, modified map[string]bool) []string {
	if len(mapped) == 0 {
		return nil
	}
	out := make([]string, 0, len(mapped))
	for k := range mapped {
		if !modified[k] {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func (p *parserState) markTable(full string) {
	if p.cur != nil {
		p.cur.tables[full] = true
	}
}

// stat returns the aggregate record for a table, creating it on first use.
// An empty name (unmapped row event) is bucketed under "(unknown)".
func (p *parserState) stat(table string) *model.TableStat {
	if table == "" {
		table = "(unknown)"
	}
	s := p.tableStat[table]
	if s == nil {
		s = &model.TableStat{Table: table}
		p.tableStat[table] = s
	}
	return s
}

func (p *parserState) recordDDLTable(b []byte, kind string) string {
	m := reDDLTable.FindSubmatch(b)
	if m == nil && kind == "TRUNCATE" {
		m = reTruncBare.FindSubmatch(b)
	}
	if m == nil {
		return ""
	}
	db := string(m[1])
	if db == "" {
		db = p.currentDB
	}
	name := string(m[2])
	if db != "" {
		name = db + "." + name
	}
	p.stat(name).DDL++
	return name
}

func (p *parserState) finish() {
	// Flush anything still being collected at EOF before closing out.
	p.endRowsQuery()
	p.finishStmtCapture()
	// An unterminated transaction at EOF is still worth reporting.
	if p.cur != nil {
		if p.cur.txn.EndPos == 0 {
			p.cur.txn.EndPos = p.lastAt
		}
		p.cur.txn.Tables = sortedKeys(p.cur.tables)
		p.cur.txn.TablesReferenced = referencedKeys(p.cur.mapped, p.cur.tables)
		p.sum.Transactions = append(p.sum.Transactions, p.cur.txn)
		p.cur = nil
	}

	if len(p.xids) > 0 {
		p.sum.HasXid = true
		p.sum.XidMin, p.sum.XidMax = p.xids[0], p.xids[0]
		for _, x := range p.xids {
			if x < p.sum.XidMin {
				p.sum.XidMin = x
			}
			if x > p.sum.XidMax {
				p.sum.XidMax = x
			}
		}
	}
	if len(p.gtidSeen) > 0 {
		p.sum.GTIDFirst = p.gtidSeen[0]
		p.sum.GTIDLast = p.gtidSeen[len(p.gtidSeen)-1]
	}

	p.sum.Tables = make([]*model.TableStat, 0, len(p.tableStat))
	for _, s := range p.tableStat {
		p.sum.Tables = append(p.sum.Tables, s)
	}
	sort.Slice(p.sum.Tables, func(i, j int) bool {
		a, b := p.sum.Tables[i], p.sum.Tables[j]
		if a.RowsChanged() != b.RowsChanged() {
			return a.RowsChanged() > b.RowsChanged()
		}
		return a.Table < b.Table
	})
}

// ---- hand-scanning helpers (all operate on []byte, no allocations) ----

// extractDesc returns the event description that follows
// "... end_log_pos N [CRC32 0x..]" within rem. If the anchor is missing it
// returns rem with leading whitespace trimmed.
func extractDesc(rem []byte) []byte {
	k := bytes.Index(rem, nEndLogPos)
	if k < 0 {
		return bytes.TrimLeft(rem, " \t")
	}
	j := k + len(nEndLogPos)
	for j < len(rem) && rem[j] >= '0' && rem[j] <= '9' {
		j++
	}
	j = skipWS(rem, j)
	if hasPrefixBytes(rem[j:], nCRC32) {
		j += len(nCRC32)
		for j < len(rem) && rem[j] != ' ' && rem[j] != '\t' {
			j++
		}
		j = skipWS(rem, j)
	}
	return rem[j:]
}

func skipWS(s []byte, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// parseTableMap extracts the "db.tbl" name and numeric id from a Table_map
// description: "Table_map: `db`.`tbl` mapped to number N". The returned name is
// a freshly allocated string (it is retained in the table map).
func parseTableMap(desc []byte) (name string, num int, ok bool) {
	a := bytes.IndexByte(desc, '`')
	if a < 0 {
		return "", 0, false
	}
	rel := bytes.IndexByte(desc[a+1:], '`')
	if rel < 0 {
		return "", 0, false
	}
	b := a + 1 + rel
	db := desc[a+1 : b]

	rel = bytes.IndexByte(desc[b+1:], '`')
	if rel < 0 {
		return "", 0, false
	}
	c := b + 1 + rel
	rel = bytes.IndexByte(desc[c+1:], '`')
	if rel < 0 {
		return "", 0, false
	}
	d := c + 1 + rel
	tbl := desc[c+1 : d]

	if k := bytes.Index(desc, nMappedTo); k >= 0 {
		num = atoiLeading(desc[k+len(nMappedTo):])
	}

	// Build "db.tbl" in one allocation.
	buf := make([]byte, 0, len(db)+1+len(tbl))
	buf = append(buf, db...)
	buf = append(buf, '.')
	buf = append(buf, tbl...)
	return string(buf), num, true
}

// parseTableID extracts N from "...: table id N ...".
func parseTableID(desc []byte) int {
	if k := bytes.Index(desc, nTableID); k >= 0 {
		return atoiLeading(desc[k+len(nTableID):])
	}
	return 0
}

// singleQuoted returns the contents of the first '...'-quoted span in b.
func singleQuoted(b []byte) ([]byte, bool) {
	a := bytes.IndexByte(b, '\'')
	if a < 0 {
		return nil, false
	}
	rel := bytes.IndexByte(b[a+1:], '\'')
	if rel < 0 {
		return nil, false
	}
	return b[a+1 : a+1+rel], true
}

// extractUseDB pulls the database name from a "use `db`/*!*/;" statement.
func extractUseDB(b []byte) string {
	s := b[4:] // drop "use "
	start := 0
	if len(s) > 0 && s[0] == '`' {
		start = 1
	}
	i := start
	for i < len(s) {
		switch s[i] {
		case '`', ';', '/', ' ', '\t', '\r':
			return string(s[start:i])
		}
		i++
	}
	return string(s[start:])
}

// ddlKeywords are upper-case for case-folded prefix matching.
var ddlKeywords = []string{"CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME"}

// leadingDDL reports whether b begins with a DDL keyword (case-insensitive),
// returning the canonical upper-case keyword.
func leadingDDL(b []byte) (string, bool) {
	return leadingKeyword(b, ddlKeywords)
}

// dclKeywords are the data-control statements surfaced as their own entries.
// (CREATE/ALTER/DROP USER begin with DDL keywords and are handled there.)
var dclKeywords = []string{"GRANT", "REVOKE"}

// leadingDCL reports whether b begins with a DCL keyword (case-insensitive),
// returning the canonical upper-case keyword.
func leadingDCL(b []byte) (string, bool) {
	return leadingKeyword(b, dclKeywords)
}

// leadingKeyword reports whether b begins with one of the given upper-case
// keywords (case-insensitive), bounded by whitespace or end of line.
func leadingKeyword(b []byte, keywords []string) (string, bool) {
	for _, kw := range keywords {
		if len(b) >= len(kw) && equalFoldPrefix(b, kw) {
			if len(b) == len(kw) || b[len(kw)] == ' ' || b[len(kw)] == '\t' {
				return kw, true
			}
		}
	}
	return "", false
}

// ---- low-level byte helpers ----

// hasPrefix reports whether b starts with the ASCII string s, without
// allocating.
func hasPrefix(b []byte, s string) bool {
	if len(b) < len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func hasPrefixBytes(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// equalFold compares b to the ASCII string s case-insensitively (full length).
func equalFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	return equalFoldPrefix(b, s)
}

// equalFoldPrefix compares the first len(s) bytes of b to s, case-insensitively.
// s must be upper-case ASCII for the folding to be correct.
func equalFoldPrefix(b []byte, s string) bool {
	for i := 0; i < len(s); i++ {
		c := b[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

// parseEventTime builds a time directly from the binlog header fields, avoiding
// a per-event time.Parse. Two-digit years map to 2000-2099, as mysqlbinlog does.
func parseEventTime(yymmdd, hms []byte) (time.Time, bool) {
	if len(yymmdd) != 6 {
		return time.Time{}, false
	}
	yy := atoi2(yymmdd[0:2])
	mo := atoi2(yymmdd[2:4])
	dd := atoi2(yymmdd[4:6])
	h, mi, s, ok := parseHMS(hms)
	if !ok || mo < 1 || mo > 12 || dd < 1 || dd > 31 {
		return time.Time{}, false
	}
	return time.Date(2000+yy, time.Month(mo), dd, h, mi, s, 0, time.UTC), true
}

// parseHMS parses "H:MM:SS" or "HH:MM:SS".
func parseHMS(s []byte) (h, m, sec int, ok bool) {
	c1 := bytes.IndexByte(s, ':')
	if c1 < 0 {
		return 0, 0, 0, false
	}
	rel := bytes.IndexByte(s[c1+1:], ':')
	if rel < 0 {
		return 0, 0, 0, false
	}
	c2 := c1 + 1 + rel
	h = atoiLeading(s[:c1])
	m = atoiLeading(s[c1+1 : c2])
	sec = atoiLeading(s[c2+1:])
	return h, m, sec, true
}

// atoiLeading parses the leading run of ASCII digits in b (0 if none).
func atoiLeading(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// atoi2 parses exactly the first two ASCII digits of b.
func atoi2(b []byte) int {
	if len(b) < 2 {
		return atoiLeading(b)
	}
	return int(b[0]-'0')*10 + int(b[1]-'0')
}

func allDigits(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] < '0' || b[i] > '9' {
			return false
		}
	}
	return len(b) > 0
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
