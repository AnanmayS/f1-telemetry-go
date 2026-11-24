# ghostlap

Pick any two Formula 1 laps from 2023 onward and see where on the track one beats the other. The circuit is drawn in sections coloured by the faster driver; pick a section to see time gained, entry, minimum and exit speeds, and the speed, throttle, brake, gear, RPM and DRS traces for that stretch. Every comparison, down to the section, is a link.

Underneath is a time-series storage engine written in Go for this one kind of data: column-oriented, compressed, CRC-protected segment files, an mmap reader, a concurrent HTTP API, and a write-ahead log for live sessions.

> Unofficial. Not associated with Formula 1 companies. Data from [OpenF1](https://openf1.org).

![Two qualifying laps compared](docs/img/compare-desktop.png)

## Status

| Milestone | State |
|---|---|
| M0 data spike | Done. Findings in [docs/DATA.md](docs/DATA.md). |
| M1 ingest + storage engine | Done. Full-corpus ingest takes about 12 hours at OpenF1's rate limit; see [docs/BENCHMARKS.md](docs/BENCHMARKS.md) for how much was ingested when the numbers were taken. |
| M2 query API | Done. [docs/API.md](docs/API.md). |
| M3 web UI | Done, then rebuilt around a sectioned track map. 24 Playwright tests; tested in Chromium (desktop and phone viewports); Safari and Firefox not yet checked. |
| M4 launch | Hosted on Vercel at https://ghostlap-six.vercel.app with a bundle of the newest sessions (a Vercel Function is capped at 250 MB, so not the full corpus). Image, CI and guide for the VPS path are in [docs/DEPLOY.md](docs/DEPLOY.md); that path is not deployed. |
| M5 live ingest | WAL, memtable, seal and crash test done, driven by replaying recorded sessions. No live network client: OpenF1's live feed is paid. |
| Race predictions (added scope) | Done. Monte Carlo race simulation, backtested walk-forward; page at `/predict.html`. Beats the grid-only baseline on finishing order and points, loses to it on picking the winner. [docs/PREDICTIONS.md](docs/PREDICTIONS.md). |

## Numbers

All from `bench/report.sh`, which regenerates [docs/BENCHMARKS.md](docs/BENCHMARKS.md). Taken on a development laptop, not the target VPS.

<!-- numbers:start -->
Full corpus, generated 2026-11-16: every session OpenF1 has telemetry for, 2023 through the 2026 São Paulo Grand Prix.

| | |
|---|---|
| Stored | 125,090,640 samples, 1.38 billion values, 266,383 laps in 464 sessions (1 more has no telemetry at the source) |
| Size | 1.31 GiB of segments from 63.38 GiB of API JSON |
| Compression | 48.4x vs all JSON fetched; 26.3x vs the JSON of just the stored samples; 7.8x vs raw 8-byte integers |
| Density | 11.24 bytes per sample across 11 channels, index included |
| Decode | 315 million values/s on one core (whole corpus in 4.4 s) |
| Lap lookup | p50 20.5 µs, p99 838 µs uncached (index search, CRC check, decode 11 blocks); p99 667 ns from the lap cache |
| `/api/compare` under load | p99 15 ms at 4,000 req/s, 100% success (20 s, 80% popular / 20% random laps, cold start). The laptop was running other work at load average 16 during this run, so results are uneven: p99 was 8 ms at 1,000 req/s but 583 ms at 2,000 req/s. At every rate up to 6,000 req/s at least 98.9% of requests finished within 50 ms server-side |
| Same, server held to 2 CPUs | p99 3.9 ms at 500 req/s, 100% success; p99 95 ms at 2,000 req/s, 99.3% success (same busy machine) |
| One comparison, nothing cached but the laps | 1.01 ms of CPU (resample, JSON, gzip) |
| Correctness | 8 golden sessions (qualifying + race per season, 2023–2026): lap counts, lap times and fastest laps match official timing; final delta equals the official difference |
| Live path | 697,845 events through WAL, memtable and seal in 1.2 s; 12 SIGKILLs during a replay, no acknowledged event lost, sealed file byte-identical to batch ingest |
<!-- numbers:end -->

## Race predictions

Each Grand Prix is simulated 10,000 times from its qualifying result, its grid and the races before it, giving every driver a chance to win, reach the podium, score points and retire, and a likely finishing range. The original spec ruled predictions out; they were added later on request, and they are an estimate for interest, not betting odds.

Measured on the 45 races from 2025 onwards, which the tuning never saw (`bench/predict-report.sh`, [docs/PREDICTIONS.md](docs/PREDICTIONS.md)):

| | Simulation | Grid order alone |
|---|---|---|
| Top pick won | 56% | 69% |
| Podium finishers among the top three picks | 2.04 of 3 | 2.04 of 3 |
| Mean finishing-position error | 2.60 places | 2.90 places |
| Brier score, points finish (lower is better) | 0.155 | 0.165 |
| Finishers inside their 10th-90th percentile range | 81% (80% is ideal) | n/a |

So it is useful for the shape of the whole result and not for calling the winner, where the pole sitter is the better bet.

## How it works

```
                 ┌───────────────────────────── cmd/ingest ─────────────────────────────┐
OpenF1 API ──►   │ rate limiter ─► fetch workers ─► raw cache ─► clean ─► lap slicer ─►  │
                 │                                                    segment writer    │
                 └──────────────────────────────────────────────────────────┬──────────┘
                                                                            ▼
                                             data/segments/{session}-{hash}.seg  (immutable)
                                             data/MANIFEST                       (atomic rename)
                                                                            │ mmap
                 ┌───────────────────────────── cmd/server ─────────────────▼──────────┐
browser ──────►  │ HTTP API ─► response cache ─► lap cache (LRU) ─► segment reader      │
 (Vite/uPlot)    │  /metrics                     align + resample by distance           │
                 └──────────────────────────────────────────────────────────────────────┘

live:  events ─► WAL (append, fsync) ─► memtable ─► in-memory segment for queries ─► sealed segment
```

- **Segment file** ([docs/FORMAT.md](docs/FORMAT.md)): one per session. One block per (driver, lap, channel), each with its own CRC32, then a sorted index and lap table, then a footer. Reading one lap touches only that lap's blocks. A damaged block fails that lap and nothing else.
- **Encodings**: delta or delta-of-delta with zigzag varints for smooth channels (the writer keeps whichever is smaller per block), run-length for gear and DRS, boolean run-length for brake, and a validity bitmap for missing readings.
- **Laps**: telemetry is cut at the official lap boundaries. Distance is integrated from speed and scaled so that each timed lap reaches the circuit length exactly at its official lap time. Two laps are compared on a common distance grid; the final delta therefore equals the official lap-time difference.
- **Store**: the set of open segments is an immutable snapshot behind an atomic pointer, with reference-counted mappings, so a rebuilt session is swapped in without a restart and without a lock on the read path. Decoded laps sit in a sharded, byte-bounded LRU; concurrent requests for the same lap decode it once.
- **Live path**: events go to a write-ahead log (length-prefixed records with CRC, group commit) and a memtable. Queries see an in-memory segment rebuilt about once a second. When the session ends it is sealed into a normal segment by the same code batch ingest uses.

## Run it

```bash
go run ./cmd/ingest -year 2024 -session 9492      # one session (2024 Japan qualifying), about 90 s
cd web && npm ci && npm run build && cd ..
go run ./cmd/server                                # http://localhost:8080/?a=9492:1:fastest&b=9492:4:fastest
```

`go run ./cmd/ingest -year all` fetches everything. It caches every raw response, stays under OpenF1's limit of 30 requests per minute, and resumes where it stopped.

Other tools:

```bash
go run ./cmd/segdump -driver 1 data/segments/9492-*.seg   # header, per-channel sizes, lap table
go run ./cmd/bench                                        # compression, decode speed, lookup latency
go run ./cmd/replay -year 2024 -session 9492 -speed 10    # a recorded session through the live path
bench/report.sh                                           # regenerate docs/BENCHMARKS.md
```

## Tests

```bash
go test -race ./...                                  # unit, property, integration, crash test
go test ./internal/encoding -fuzz FuzzDecodeBlock    # one of eight fuzz targets
go test ./internal/ingest -run Golden -v             # against official timing; needs ingested sessions
cd web && npm test                                   # Playwright smoke tests
```

The golden tests need real data and skip without it; the list of sessions and the command to ingest them is at the top of `internal/ingest/golden_test.go`.

## Layout

```
cmd/ingest      discover + fetch + build segments     internal/openf1    API client, rate limit, raw cache
cmd/server      HTTP API + static files               internal/laps      slicing, distance, alignment
cmd/bench       storage benchmarks                    internal/encoding  codecs (+ fuzz tests)
cmd/segdump     inspect a segment                     internal/segment   file format, mmap reader
cmd/replay      recorded session through live path    internal/store     manifest, registry, lap cache
cmd/spike       milestone-0 data probe                internal/lru       sharded byte-bounded LRU
web/            Vite + TypeScript + uPlot             internal/wal       write-ahead log
bench/          load test + report scripts            internal/live      memtable, replay, seal
docs/           PRD, DATA, FORMAT, API, DEPLOY        internal/api       HTTP handlers, metrics
cmd/predict     race records, backtest, build         internal/predict   race model, Monte Carlo simulator
```

## Known limits

- Telemetry is about 3.7 Hz with integer km/h, and the feed's lap start times are good to a few tens of milliseconds. The delta is exact at the end of the lap and approximate in between.
- Circuit length is measured from the data (median integrated distance of clean laps) and comes out about 0.5% short of the official figure.
- Only samples inside a lap are stored; time in the garage is dropped.
- Free, non-commercial, no ads, no accounts, no third-party scripts.
