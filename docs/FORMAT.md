# Segment file format (GLAP v1)

One file per session, immutable once written, all integers little-endian. Source of truth: `internal/segment/segment.go` and `internal/encoding/encoding.go`. Inspect a file with `go run ./cmd/segdump file.seg`.

```
+---------+----------------------+---------+----------+
| Header  | Blocks ...           | Index   | Footer   |
+---------+----------------------+---------+----------+
```

## Header

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `GLAP` |
| 4 | 2 | `format_version` u16 (= 1) |
| 6 | 4 | `session_key` u32 |
| 10 | 8 | `epoch_unix_ms` i64: the session's `date_start`; every timestamp in the file is ms relative to it |
| 18 | 4 | `meta_len` u32 |
| 22 | `meta_len` | metadata JSON: session, drivers, `circuit_length_m`, `samples`, `raw_samples`, `raw_json_bytes` |
| 22 + `meta_len` | 4 | CRC32 (IEEE) of every header byte before it |

## Block

One block per (driver, lap, channel). Each lap has all 11 channels, written in channel order.

| Size | Field |
|---|---|
| 1 | `encoding` u8: low 7 bits = codec, bit 7 (`0x80`) = payload starts with a validity bitmap |
| 4 | `count` u32: number of samples |
| n | payload |
| 4 | CRC32 of `encoding`, `count` and payload |

A reader checks the CRC on every read. A failed block fails that lap only.

### Channels

| Id | Name | Unit | Codecs the writer may pick |
|---|---|---|---|
| 0 | `t` | ms since epoch | delta, delta-of-delta |
| 1 | `speed` | km/h | delta, delta-of-delta |
| 2 | `rpm` | | delta, delta-of-delta |
| 3 | `gear` | 0 = neutral | RLE |
| 4 | `throttle` | % | delta, delta-of-delta |
| 5 | `brake` | 0 or 1 | boolean RLE |
| 6 | `drs` | raw feed code | RLE |
| 7–9 | `x`, `y`, `z` | feed units | delta, delta-of-delta |
| 10 | `distance` | cm from the lap start | delta, delta-of-delta |

For smooth channels the writer encodes the block both ways and keeps the smaller; the choice is recorded in the block's `encoding` byte.

### Codecs

All varints are unsigned LEB128 (`encoding/binary` uvarint). Signed values are zigzag-mapped first: `(v << 1) ^ (v >> 63)`.

| Id | Name | Payload |
|---|---|---|
| 1 | delta | per value: zigzag varint of `v[i] - v[i-1]` (`v[-1]` = 0) |
| 2 | delta-of-delta | per value: zigzag varint of `(v[i] - v[i-1]) - (v[i-1] - v[i-2])` (previous value and previous delta start at 0) |
| 3 | RLE | repeated pairs: zigzag varint value, uvarint run length (>= 1); runs sum to `count` |
| 4 | boolean RLE | one byte first value (0 or 1), then uvarint run lengths of alternating values; empty when `count` = 0 |

### Validity bitmap

Written only when a block has a missing value (the feed's 104 sentinel for throttle and brake, or no location fix for x/y/z). It is `ceil(count / 8)` bytes at the start of the payload; bit `i % 8` of byte `i / 8` is 1 when sample `i` is present. A missing sample repeats the previous present value in the codec stream, so it costs no extra bytes there.

## Index

| Size | Field |
|---|---|
| 4 | `block_count` u32 |
| 16 x `block_count` | entries sorted by (driver, lap, channel): `driver` u8, `lap` u16, `channel` u8, `offset` u64 (from file start), `length` u32 (whole block incl. CRC) |
| 4 | `lap_count` u32 |
| 40 x `lap_count` | lap table sorted by (driver, lap), see below |

Lap record (40 bytes):

| Size | Field |
|---|---|
| 1 | `driver` u8 |
| 1 | `flags` u8: 1 = pit-out, 2 = pit-in, 4 = valid official time, 8 = distance normalised |
| 2 | `lap` u16 |
| 8 | `start_ms` i64 |
| 8 | `end_ms` i64 |
| 4 | `official_ms` i32 (-1 = none) |
| 12 | sector 1–3 times, i32 ms each (-1 = none) |
| 4 | `samples` u32 (0 = lap has no telemetry) |

## Footer (20 bytes)

| Size | Field |
|---|---|
| 8 | `index_offset` u64 |
| 4 | `index_length` u32 |
| 4 | CRC32 of the index |
| 4 | magic `GLAP` |

## Reading

1. `mmap` the file. Check both magics and the version; reject unknown versions.
2. Verify the header CRC and the index CRC. Parse the index and lap table once.
3. To read (driver, lap, channel): binary-search the index, verify that block's CRC, decode. No other block is touched.

## Manifest

`data/MANIFEST` is JSON listing each live segment: `session_key`, `year`, `file`, `size`, `sha256`, `samples`, plus a `version` that increases on every write and a `skipped` map of sessions with no telemetry. It is replaced by writing `MANIFEST.tmp`, `fsync`, atomic rename, then `fsync` of the directory. A segment file is durable before the manifest names it. Writers (batch ingest and live seal) change it only inside `store.Update`, which holds an exclusive `flock` on `MANIFEST.lock` and re-reads the file under the lock, so two writers cannot overwrite each other's entries.

Segment files are named `{session_key}-{first 12 hex of sha256}.seg`. Rebuilding a session produces a new file name; the server swaps it in on its next manifest reload and unmaps the old file when the last in-flight read releases it.

The file hash is SHA-256 rather than CRC32 on purpose. Each block is followed by its own CRC32, and CRC32 over "data followed by its CRC32" is a constant, so a whole-file CRC32 does not change when a block changes without changing length. A test caught this.

## Deviations from the PRD sketch

- The index stores `block_count` and `lap_count` prefixes, and the header has its own CRC.
- Lap records add `samples` and separate pit-in / distance flags.
- The lap cache key is (session, file hash, driver, lap). A cached entry holds all channels of the lap, because a comparison always needs time, distance and the plotted channels together.
