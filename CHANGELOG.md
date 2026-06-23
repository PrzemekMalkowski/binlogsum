# Changelog

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
