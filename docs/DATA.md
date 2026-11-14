# Data source findings (milestone 0)

Spike run on 2026-11-14 with `go run ./cmd/spike` (session 9492, 2024 Japan qualifying, driver 1). Rerun it to refresh every number below.

## Endpoints

All six endpoints in PRD 4.1 return HTTP 200 with the documented fields.

| Endpoint | Rows | Bytes | Fields |
|---|---|---|---|
| `sessions?year=2024` | 123 | 44,993 | `circuit_key`, `circuit_short_name`, `country_code`, `country_key`, `country_name`, `date_end`, `date_start`, `gmt_offset`, `is_cancelled`, `location`, `meeting_key`, `session_key`, `session_name`, `session_type`, `year` |
| `meetings?year=2024` | 25 | 18,993 | `meeting_key`, `meeting_name`, `meeting_official_name`, `circuit_short_name`, `country_name`, `date_start`, `date_end`, `is_cancelled`, … |
| `drivers?session_key=9492` | 20 | 8,317 | `driver_number`, `name_acronym`, `full_name`, `team_name`, `team_colour`, `broadcast_name`, `first_name`, `last_name`, `country_code`, `headshot_url` |
| `laps?session_key=9492&driver_number=1` | 12 | 5,770 | `lap_number`, `date_start`, `lap_duration`, `duration_sector_1/2/3`, `is_pit_out_lap`, `i1_speed`, `i2_speed`, `st_speed`, `segments_sector_1/2/3` |
| `car_data?session_key=9492&driver_number=1` | 17,250 | 2,821,599 | `date`, `speed`, `rpm`, `n_gear`, `throttle`, `brake`, `drs` |
| `location?session_key=9492&driver_number=1` | 17,620 | 2,203,199 | `date`, `x`, `y`, `z` |

Seasons: 2023 (118 sessions, 5 cancelled), 2024 (123, 0), 2025 (123, 0), 2026 (131, 10 cancelled, includes sessions not yet run). `sessions?year=2022` returns 404 `{"detail":"No results found."}`.

## Sampling

- `car_data`: 3.79 Hz; interval median 240 ms, p10 160 ms, p90 400 ms, max 1.24 s.
- `location`: 3.87 Hz, same shape, but **its timestamps are not the car_data timestamps** (offset by roughly 100–200 ms). Ingest linearly interpolates x/y/z onto the car_data time base so a lap has one clock.
- No out-of-order rows, duplicate timestamps, or exact duplicates in this sample. Ingest still sorts and de-duplicates.

## Quirks confirmed

| Quirk | Observed | Handling |
|---|---|---|
| `throttle` sentinel 104 | 6,198 of 17,250 rows | stored as missing (validity bitmap) |
| `brake` values | only 0, 100, 104 | stored as boolean; 104 is missing |
| `drs` codes | 8, 10, 12, 14 seen | raw code stored; "open" = 10, 12, 14 derived at query time |
| `n_gear` | 0–8 (0 = neutral) | stored raw |
| `lap_duration` null | lap 1 (out-lap) | lap end = next lap's `date_start`; flagged "unofficial time" |
| `duration_sector_1` null | every pit-out lap | stored as null |

OpenF1 documents the DRS mapping as a "supposed interpretation" taken from FastF1: 0, 1 off; 2, 3, 9 unknown; 8 eligible; 10, 12, 14 on.

**Found in the spike, not in the PRD:**

- **Pit-out lap durations are measured from the previous timing-line crossing**, not from `date_start` (which is the pit exit). Lap 4 reports 1054.112 s but lap 5 starts 117 s after lap 4's `date_start`. A lap time is therefore only treated as official when the lap is not a pit-out lap and `lap_duration` agrees with the gap to the next lap's `date_start`.
- **`date_start` is approximate.** Consecutive `date_start` values differ from `lap_duration` by 20–30 ms on clean laps. Distance is normalised over `[date_start, date_start + lap_duration]` so that a lap's elapsed time at full distance equals its official time exactly.

## Rate limits

Published on openf1.org: free tier **3 requests/second and 30 requests/minute**. Sponsor tier (EUR 9.90/month): 6/s and 60/min.

The PRD default of 2 requests/second exceeds the per-minute cap. Ingest defaults are **1 request per 2.1 s (about 28.5/minute), burst 1, 2 in flight**. At about 43 requests per session and about 470 sessions, a cold full ingest is roughly 20,000 requests, or about 11–12 hours.

**Response size limit (found during ingest, not documented by OpenF1):** `car_data` and `location` for one driver over a very long session (the 9-hour pre-season testing days) return HTTP 422 "You're likely asking for too much data at once". Ingest then splits the query into time windows with `date>=...&date<...`, halving until each piece is accepted, joins the rows and caches the joined result.

Historical queries are refused for unauthenticated clients while a session is live (from 30 minutes before to 30 minutes after). Ingest treats that as retryable-later and exits cleanly; rerun after the session.

## Terms

- Code licence: CC BY-NC-SA 4.0. No separate terms-of-service page.
- Stated intent: "educational purposes, personal learning projects, research, and non-commercial fan engagement". Other uses require contacting OpenF1.
- Attribution is not required but appreciated. This project credits and links OpenF1.
- OpenF1 disclaimer: unofficial, not affiliated with Formula 1 companies; F1 trademarks belong to Formula One Licensing B.V.
- This project is free, non-commercial, carries no ads, and shows: "Unofficial. Not associated with Formula 1 companies. Data from OpenF1."

## Sources that are not available

- `livetiming.formula1.com/static/...` returns **HTTP 401** (confirmed). Since October 2026 it requires an F1 TV subscription token, and F1's terms do not permit rehosting. Not used.
- **Pre-2023 telemetry:** no free, permitted source found. Out of scope.
- **Live data:** OpenF1 real-time (MQTT/WebSocket, about 3 s delay) is **paid only** (sponsor tier). No free live source exists, so milestone 5 builds the WAL, memtable and seal path and proves them against a recorded session replayed through the live path. No live network client is built.

## Open questions answered

1. Rate limits and terms: above.
2. Free pre-2023 telemetry: none.
3. Free live feed for 2027: none; OpenF1 live is paid.
