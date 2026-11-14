# PRD: ghostlap — F1 telemetry store and lap comparison site

> Working name. Rename freely.
> This document is written to be handed to Claude Code (or any engineer) as the build spec.
> Put it in the new repo as `docs/PRD.md` and reference it from `CLAUDE.md`.

---

## 0. Instructions for the builder (read first)

- Build **milestone by milestone** (section 11). Stop at the end of each milestone, summarize what was built, show the measured numbers, and wait for review before starting the next.
- **Measure, never guess.** Every number in the README (compression ratio, latency, throughput, session counts) must come from a benchmark or script committed to the repo that anyone can rerun. If a target is missed, report the real number; do not tune the benchmark to hit it.
- **Verify the data source before building on it.** Milestone 0 is a spike that confirms every endpoint and field in section 4 still behaves as described. If anything differs, stop and report.
- Prefer the Go standard library. Allowed dependencies are listed in section 5. Ask before adding others.
- Write tests alongside code, not after. Each milestone's acceptance criteria include tests.
- Keep the storage engine **purpose-built for this data**. Do not generalize it into a database.

---

## 1. Summary

A website where F1 fans pick any two laps from any session (2023 onward) and instantly see them overlaid: speed, throttle, brake, gear, RPM, DRS, and a running time delta, aligned by distance around the track. Every comparison is a shareable link.

Underneath is a **time-series storage engine written in Go**, designed for F1 car telemetry: column-oriented, compressed, indexed by session / driver / lap, and served by a concurrent HTTP API. Later it accepts live race data through a write-ahead log so laps can be compared moments after they are driven.

**Why it exists:** today, comparing two laps means running FastF1 in a Python notebook, which downloads and processes a whole session first. Fans on r/F1Technical, F1 Discord servers, and social media argue about lap comparisons every weekend. A link that loads in under a second is something people will actually share.

**Engineering goals (in priority order):**
1. Correct data: lap slices and lap times match official timing.
2. A real storage engine: own file format, compression, index, crash safety.
3. Fast concurrent reads: any comparison p99 < 50 ms server-side under load.
4. A simple, fast site real people use.

---

## 2. Users and user stories

**Primary user:** an F1 fan who follows technical discussion (Reddit r/F1Technical, r/formula1, Discord, X). Not a programmer.

**Secondary user:** content creators and analysts who post comparison charts; developers who want a fast telemetry API.

| # | As a… | I want to… | So that… |
|---|---|---|---|
| U1 | fan | pick a season → event → session → two drivers → two laps | I can compare them without code |
| U2 | fan | default to each driver's fastest lap | the common case is one click |
| U3 | fan | see speed, throttle, brake, gear, RPM, DRS overlaid by distance | I can see where time is won or lost |
| U4 | fan | see a running delta (gap in seconds across the lap) | I know exactly which corners matter |
| U5 | fan | hover anywhere and see both cars' values at that point | I can read exact numbers |
| U6 | fan | copy a link that reopens the exact comparison | I can share it in a thread |
| U7 | fan | compare laps from different sessions (e.g. 2023 vs 2024 at the same track) | I can see year-on-year changes |
| U8 | developer | call a documented JSON API | I can build on it |
| U9 (M5) | fan | compare laps from a race that is happening now | I can follow it live |

---

## 3. Scope

**In scope (MVP, milestones 0–4):** historical sessions from the data source's first available season (2023) to the latest completed session; car telemetry + lap timing; storage engine; HTTP API; web UI; deployment; benchmarks.

**Later (milestone 5):** live ingest during sessions via write-ahead log.

**Non-goals:**
- No accounts, logins, comments, or social features.
- No predictions, ML models, or betting content.
- No video, audio, team radio, or F1 imagery/logos.
- No general-purpose query language or SQL.
- No pre-2023 data unless a free, permitted source is found (see section 4.4).
- No mobile app (the site must work on phones, though).

---

## 4. Data source

### 4.1 OpenF1 (primary)

Free, unauthenticated REST API at `https://api.openf1.org/v1/` serving historical data from 2023 onward. Verified working on 2026-11-14:

| Endpoint | Use | Key fields |
|---|---|---|
| `GET /v1/sessions?year=YYYY` | list sessions | `session_key`, `meeting_key`, `session_name`, `session_type`, `date_start`, `date_end`, `circuit_short_name`, `country_name`, `year`, `is_cancelled` |
| `GET /v1/meetings?year=YYYY` | event names | `meeting_key`, `meeting_name`, … |
| `GET /v1/drivers?session_key=K` | driver list per session | `driver_number`, `name_acronym`, `full_name`, `team_name`, `team_colour` |
| `GET /v1/laps?session_key=K&driver_number=N` | lap boundaries + official times | `lap_number`, `date_start`, `lap_duration` (null on some laps), `duration_sector_1/2/3`, `is_pit_out_lap`, `i1_speed`, `i2_speed`, `st_speed` |
| `GET /v1/car_data?session_key=K&driver_number=N` | telemetry, ~3.7 Hz | `date`, `speed` (km/h), `rpm`, `n_gear`, `throttle` (%), `brake`, `drs` |
| `GET /v1/location?session_key=K&driver_number=N` | car X/Y/Z on track, ~3.7 Hz | `date`, `x`, `y`, `z` |

Observed size: one driver, one qualifying session (2024 Japan, `session_key=9492`) → ~17,250 `car_data` rows, ~2.8 MB JSON, ~3 s response.

**Rough corpus size:** ~4 seasons × ~24 events × ~5 sessions × 20 drivers × 15k–40k samples × 7 channels (+3 location) ≈ **1–3 billion values**. Uncompressed as 8-byte values that is tens of GB; the engine should bring it to a few GB.

### 4.2 Known data quirks (handle explicitly, with tests)

- Sentinel values: `throttle`/`brake` = 104 appear when the car is stationary or data is invalid. Map to "missing", never plot as 104%.
- `brake` is effectively boolean (0 or 100) in this feed; store it as such.
- `drs` is an enum code, not a boolean (8 seen in the spike sample; community docs list 0, 1, 8, 10, 12, 14, with 10/12/14 meaning open). Confirm the mapping in M0, store the raw code, and derive "open" at query time.
- Rows may be duplicated or slightly out of order. Sort by timestamp; drop exact duplicates.
- `lap_duration` is null on out-laps and some in-laps. Use the next lap's `date_start` as the end time when available; mark such laps "unofficial time".
- Timestamps are ISO 8601 UTC with microseconds. Store as int64 milliseconds since session start (ms precision is sufficient; record the session epoch in the segment header).
- Cancelled sessions (`is_cancelled: true`) and sessions with no data must be skipped cleanly.

### 4.3 Politeness and caching

- Every raw API response is cached to disk (`data/raw/{session_key}/{endpoint}_{driver}.json.gz`) and fetched **once**. Re-runs read the cache.
- Global rate limiter (token bucket) shared by all ingest workers; default ≤ 2 requests/second and ≤ 2 in flight, configurable. Check OpenF1's current published rate limits in milestone 0 and set defaults at or below them.
- Retry with exponential backoff + jitter on 429/5xx; respect `Retry-After` if present.
- Send a descriptive `User-Agent` with the project name and repo URL (no personal email).

### 4.4 Sources that are NOT available (do not build on these)

- `livetiming.formula1.com/static/...` (the archive FastF1 historically used) returned **HTTP 401** for every year on 2026-11-14. Treat as unavailable unless milestone 0 shows otherwise.
- Live data: confirm in milestone 0 whether OpenF1's real-time feed is free or paid, and what F1's own live feed requires. Milestone 5 depends on this.

### 4.5 Terms

- Unofficial data. The site must say: "Unofficial. Not associated with Formula 1 companies. Data from OpenF1." with links.
- Free and non-commercial. No ads, no paywall.
- Read and follow OpenF1's terms/license in milestone 0; record them in `docs/DATA.md`.

---

## 5. Tech stack

| Layer | Choice | Notes |
|---|---|---|
| Storage engine, ingest, API | **Go** (latest stable) | stdlib `net/http`, `encoding/binary`, `hash/crc32`, `sync`, `context`, `log/slog` |
| Go deps allowed | `golang.org/x/time/rate`, `golang.org/x/sync/errgroup`, `github.com/prometheus/client_golang` | ask before adding others |
| Load testing | `vegeta` (Go) | committed scripts under `bench/load/` |
| Frontend | TypeScript + Vite + **uPlot** | uPlot handles large series fast; no React needed, a small vanilla TS app is fine |
| Deploy | Docker, single small VPS (e.g. Hetzner/Fly.io) behind Cloudflare | immutable data files make CDN caching easy |
| CI | GitHub Actions | `go vet`, `staticcheck`, tests with `-race`, fuzz smoke run, frontend build |

---

## 6. Architecture

```
                 ┌─────────────────────────── ingest (cmd/ingest) ───────────────────────────┐
OpenF1 API ──►   │ rate limiter ─► fetch workers ─► raw cache ─► normalize ─► lap slicer ─►   │
                 │                    (errgroup, N)                    segment writer       │
                 └───────────────────────────────────────────────────────────┬───────────────┘
                                                                             ▼
                                                    data/segments/{session_key}.seg   (immutable)
                                                    data/MANIFEST   (atomic rename)
                                                                             │ mmap
                 ┌──────────────────────────── server (cmd/server) ─────────▼───────────────┐
browser ──────►  │ HTTP API ─► query layer ─► lap cache (LRU) ─► segment reader ─► decode     │
 (Vite/uPlot)    │   /metrics (Prometheus)        align + resample by distance               │
                 └───────────────────────────────────────────────────────────────────────────┘

M5 live:  live feed ─► WAL (append, fsync) ─► memtable ─► seal into segment when session ends
```

### 6.1 Repo layout

```
cmd/ingest/        CLI: discover + fetch + build segments
cmd/server/        HTTP API + static files
cmd/bench/         storage benchmarks (compression, decode speed, lookup latency)
cmd/segdump/       debug tool: print a segment's header/index/blocks
internal/openf1/   API client, rate limiting, raw cache
internal/model/    Session, Driver, Lap, Sample types
internal/laps/     lap slicing, distance integration, alignment, resampling
internal/encoding/ delta-of-delta, zigzag, varint/bitpack codecs (+ fuzz tests)
internal/segment/  file format writer/reader, index, CRC, mmap
internal/store/    manifest, segment registry, LRU cache, query API
internal/wal/      (M5) write-ahead log + memtable
internal/api/      HTTP handlers, JSON shapes, caching headers
web/               Vite + TypeScript + uPlot frontend
bench/load/        vegeta targets + scripts
docs/              PRD.md, FORMAT.md, DATA.md, BENCHMARKS.md
```

---

## 7. Storage engine specification

### 7.1 Data model

- **Session**: `session_key`, year, meeting name, session name/type, circuit, start time (epoch for relative timestamps).
- **Driver**: number, acronym, full name, team, team colour (per session).
- **Lap**: driver, lap number, start ms, end ms, official duration (nullable), sector times, pit-out/in flags, `is_valid_time` flag.
- **Channels per sample**: `t` (ms since session start), `speed`, `rpm`, `gear`, `throttle`, `brake`, `drs`, `x`, `y`, `z`. All integers.

### 7.2 Segment file (one per session)

Immutable once written. Little-endian. Documented byte-for-byte in `docs/FORMAT.md`.

```
[Header]
  magic "GLAP" | format_version u16 | session_key u32 | epoch_unix_ms i64
  session metadata (length-prefixed JSON, small)
[Blocks...]
  one block per (driver, lap, channel):
    encoding u8 | count u32 | payload bytes | crc32 u32
[Index]
  sorted entries: (driver u8, lap u16, channel u8) -> (offset u64, length u32)
  lap table: per (driver, lap): start_ms, end_ms, official_ms, flags, sector times
[Footer]
  index_offset u64 | index_length u32 | crc32(index) u32 | magic "GLAP"
```

- Reader opens the file with `mmap`, reads the footer, validates CRCs, binary-searches the index. A single-lap, single-channel read touches only that block.
- Corrupt block → error for that lap only; never crash the server.
- Version field allows format changes; reader rejects unknown versions.

### 7.3 Encodings

| Channel | Encoding | Why |
|---|---|---|
| `t` | delta-of-delta + zigzag + varint | sampling is near-regular, so most values are 0 or tiny |
| `speed`, `rpm`, `x`, `y`, `z` | delta + zigzag + varint | smooth signals; small deltas |
| `throttle` | delta + zigzag + varint | often flat at 0 or 100 |
| `gear`, `drs` | run-length encoding | change rarely |
| `brake` | run-length of booleans | on/off |
| missing values | per-block validity bitmap (only written if any missing) | handles the 104 sentinel |

Requirements:
- Every codec has round-trip property tests and a Go native fuzz test (`FuzzDecode…`) that must never panic on arbitrary input.
- `cmd/bench` reports bytes per sample per channel, total compression ratio vs raw JSON and vs raw 8-byte ints, and decode throughput (samples/sec).

### 7.4 Manifest

- `data/MANIFEST` lists live segment files and their checksums. Updated by writing `MANIFEST.tmp`, fsync, then atomic rename. A half-written segment is never visible.
- Rebuilding a session writes a new segment file and swaps it in via the manifest; the server picks it up without restart (watch or periodic reload) and closes old mmaps after in-flight reads finish (reference counting).

### 7.5 Lap cache

- In-memory LRU of decoded laps (bounded by bytes, default 256 MB). After a race, everyone looks at the same few laps; cache hits should dominate.
- Cache key: (session, driver, lap, channels).
- Concurrent requests for the same uncached lap decode it **once** (singleflight pattern, implemented with stdlib or a small internal helper).

---

## 8. Lap processing

### 8.1 Slicing

- A lap's samples are those with `t` in `[lap.start, next_lap.start)`, or `[start, start + lap_duration]` when the next lap is missing.
- Include one sample before and after the window for interpolation at the edges.

### 8.2 Distance

- Compute cumulative distance by integrating speed over time (trapezoidal; km/h → m/s). Normalize so a lap's total distance matches the circuit length median across clean laps in that session (store the computed circuit length in session metadata).
- Expose `distance_m` as a derived channel (computed at ingest, stored, encoded like `speed`).

### 8.3 Alignment and delta

- To compare two laps, resample both onto a common distance grid (default every 5 m) with linear interpolation (step interpolation for `gear`, `drs`, `brake`).
- Running delta at distance d = time_B(d) − time_A(d).
- The final delta must equal the difference of official lap times within ±0.05 s for laps with official times; mismatches are logged and counted as a metric.

### 8.4 Correctness tests (golden)

- For a fixed set of sessions (at least 1 qualifying + 1 race per season in the corpus), assert: number of laps per driver matches `laps` endpoint; sum of sliced lap durations within tolerance of official; fastest lap per driver matches official fastest lap.

---

## 9. HTTP API

All responses JSON, gzip-compressed, with `ETag` and long `Cache-Control` for historical data (segments are immutable).

| Method + path | Returns |
|---|---|
| `GET /api/seasons` | list of years available |
| `GET /api/sessions?year=YYYY` | sessions with names, circuit, keys |
| `GET /api/sessions/{key}/drivers` | drivers with team + colour |
| `GET /api/sessions/{key}/laps?driver=N` | lap table: number, duration, sectors, flags, `is_fastest` |
| `GET /api/sessions/{key}/laps/{driver}/{lap}?channels=speed,throttle,...` | raw samples for one lap |
| `GET /api/compare?a={key}:{driver}:{lap}&b={key}:{driver}:{lap}&channels=...&step=5` | both laps resampled on a common distance grid + delta series |
| `GET /api/health` | ok + manifest version |
| `GET /metrics` | Prometheus metrics |

- `lap` accepts `fastest` as a value.
- Errors: JSON `{ "error": "...", "code": "..." }` with 400/404/500. Never leak stack traces.
- Per-IP rate limit on the API (generous; protects the VPS).

---

## 10. Web UI

Minimal, fast, works on a phone.

- **Picker:** season → event → session → driver A + lap, driver B + lap. Lap dropdown shows lap time and marks the fastest lap. Default: each driver's fastest lap.
- **Charts (uPlot), stacked and sharing one x-axis (distance):** delta (top), speed, throttle, brake, gear, RPM, DRS. Team colours for each driver; use dashed lines when both drivers share a team.
- **Hover:** a crosshair across all charts showing both drivers' values at that distance.
- **Track map:** small X/Y outline of the lap from location data, with a dot following the hover position.
- **Share:** URL encodes the full selection (`?a=9492:1:fastest&b=9492:4:fastest`); a "Copy link" button.
- **Footer:** the unofficial-data disclaimer from section 4.5 and a link to the repo.
- Performance budget: initial JS ≤ 150 KB gzipped; a cached comparison renders in < 1 s on a mid-range phone.

---

## 11. Milestones and acceptance criteria

### M0 — Data spike (1–2 days)
- Script hits every endpoint in 4.1 for one session and saves samples.
- Confirms field names, sampling rate, quirks in 4.2, rate limits, terms, and the status of the sources in 4.4.
- Output: `docs/DATA.md` with findings. **Stop and report if anything contradicts this PRD.**

### M1 — Ingest + storage engine (week 1–2)
- `cmd/ingest --year 2024` discovers sessions, fetches with rate limiting + raw cache, slices laps, writes segments + manifest. Resumable after Ctrl-C; idempotent on rerun.
- Codecs with property + fuzz tests; segment writer/reader with CRC validation; `cmd/segdump`.
- Golden lap tests (8.4) pass.
- `cmd/bench` prints: total samples, raw JSON bytes, segment bytes, compression ratio, decode samples/sec, single-lap lookup latency (cold and warm).
- Acceptance: full ingest of all available seasons completes; all tests pass with `-race`.

### M2 — Query API (week 3)
- Endpoints in section 9, lap cache with singleflight, distance alignment + delta (8.2–8.3).
- Vegeta load test against a realistic mix (80% popular laps, 20% random laps).
- Acceptance: `/api/compare` p99 < 50 ms server-side at a sustained rate recorded in `docs/BENCHMARKS.md` (report the highest rate that keeps p99 under 50 ms on the target VPS size). Delta matches official lap-time difference within ±0.05 s on golden laps.

### M3 — Web UI (week 4)
- Everything in section 10.
- Playwright smoke test: open a share link → charts render → hover shows values → copy link round-trips.
- Acceptance: works on desktop Chrome/Safari/Firefox and mobile Safari/Chrome.

### M4 — Launch (week 4–5)
- Dockerfile, deploy to a VPS behind Cloudflare, HTTPS, Prometheus metrics, basic uptime check.
- Privacy-friendly visit counting (server logs or a self-hosted counter; no third-party trackers) so usage numbers are real.
- README with screenshots/GIF, architecture diagram, benchmark table (generated from committed scripts), and the disclaimer.
- Post to relevant communities after a race weekend with an interesting comparison.

### M5 — Live ingest (2027 season; depends on M0 findings)
- Live source client (only if a free, permitted source exists).
- WAL: append-only, length-prefixed records with CRC, fsync policy configurable (per batch, default every 100 ms); replay on startup; truncate torn tail records.
- Memtable per live session; queries merge sealed segments + memtable; seal to a segment when the session ends.
- Crash test: kill -9 the process at random points during a replayed live session (feed recorded data through the live path at 10× speed); after restart, no acknowledged sample is lost and no duplicates appear.
- Acceptance: a lap is comparable on the site within 10 s of crossing the line during a real or replayed session.

---

## 12. Non-functional requirements

| Area | Requirement |
|---|---|
| Latency | `/api/compare` p99 < 50 ms server-side (warm cache), < 200 ms cold |
| Storage | full corpus fits in a few GB on disk; record actual size and ratio |
| Concurrency | all handlers safe under `-race`; no global locks on the read path |
| Reliability | corrupt segment or block affects only that data; server stays up |
| Observability | Prometheus: request count/latency by route, cache hit ratio, segment count, decode errors, lap-time mismatch count |
| Security | no user data stored; per-IP rate limit; input validated; no secrets in repo |
| Cost | runs on one small VPS (≤ $10/month) plus free CDN tier |

---

## 13. Success metrics

**Engineering (from committed benchmarks):** compression ratio vs raw JSON; p99 compare latency at a stated request rate; ingest time for the full corpus; samples stored; 0 lost samples in WAL crash tests (M5).

**Usage (from real traffic):** unique visitors per race weekend; comparisons served; share links opened; communities where it was shared.

Only report numbers that the repo or server logs can reproduce.

---

## 14. Risks

| Risk | Mitigation |
|---|---|
| OpenF1 changes, rate-limits harder, or goes paid | raw cache means already-fetched data stays usable; ingest is source-agnostic behind an interface |
| Live data not freely available | M5 is optional; M0–M4 stand alone |
| Lap slicing inaccuracies | golden tests against official times; mismatch metric in production |
| Scope creep into a general database | section 0 rule; format is per-session and immutable |
| Low adoption | launch right after a race with a striking comparison; make share links look good (Open Graph preview image of the chart) |

---

## 15. Open questions

1. OpenF1's current rate limits and terms (answer in M0).
2. Is any free, permitted source available for pre-2023 telemetry?
3. Is a free live feed available for 2027 (OpenF1 real-time or otherwise)?
4. Final project name.
