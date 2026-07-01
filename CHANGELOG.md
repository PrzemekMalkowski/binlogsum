# Changelog

## 0.2.0

- Fix: attribute each row change to the table named on its own
  `### INSERT/UPDATE/DELETE` line. The previous indirect lookup (row event
  `table id` → table map → current table) could misattribute or collapse a
  multi-table transaction onto a single table when MySQL reused table-map ids.
- Per-transaction tables are now split into **updated** (tables with actual row
  changes) and **referenced** (tables present only via foreign-key `Table_map`
  events, as emitted since MySQL 9.7 with `USE_SQL_FOREIGN_KEY_F`). Only updated
  tables count toward the most-updated summary; the table set no longer includes
  merely-referenced tables.
- Capture the original SQL of statements: row-based DML logged with
  `binlog_rows_query_log_events` (`Rows_query` events), plus DDL and DCL
  statements (multi-line statements included). Shown in the web UI transaction
  detail.
- Recognize DCL statements (`GRANT`, `REVOKE`): counted in the per-query-type
  tally and surfaced as their own transaction entries, alongside DDL.
- Web UI: transaction detail shows updated vs. referenced tables and the original
  query; the per-query-type list and stat boxes include GRANT / REVOKE.

## 0.1.0

Initial release.

- Parse decoded `mysqlbinlog --base64-output=DECODE-ROWS -v` output (ROW with
  FULL or MINIMAL row image, and STATEMENT format) from stdin or `--file`.
- Header summary: time frame, server flavor/version, per-`server_id` event
  counts, GTID range, Xid range, total events, per-query-type tally, and
  most-updated tables by rows changed.
- Per-transaction metadata: byte size, tables, rows ins/upd/del, DDL kind/table,
  timestamps. DDL statements appear as their own entries.
- Writes-over-time histogram (by rows changed and by transaction size).
- Three modes: styled `text` terminal report, interactive `web` UI, and
  self-contained `snapshot` HTML.
- Allocation-free `[]byte` scanner on the hot path for fast parsing of
  multi-GB logs.
