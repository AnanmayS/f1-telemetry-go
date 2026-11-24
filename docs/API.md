# HTTP API

Base path `/api`. All responses are JSON, gzip-compressed when the client sends `Accept-Encoding: gzip`. Data responses carry an `ETag` and `Cache-Control: public, max-age=604800`; send `If-None-Match` to get `304`. Listings are cached for 5 minutes.

Errors: `{"error": "human message", "code": "machine_code"}` with status 400, 404, 422, 429 or 500.

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | malformed parameter |
| `not_found` | 404 | no such session, driver, lap or endpoint |
| `no_telemetry` | 404 | the lap exists in timing but has no car data |
| `no_fastest_lap` | 404 | driver has no timed lap with telemetry |
| `no_circuit_length` | 422 | a session has no clean lap to measure the circuit, so laps cannot be aligned |
| `rate_limited` | 429 | per-IP limit exceeded; see `Retry-After` |
| `corrupt_data` | 500 | the stored block for this lap failed its checksum |

## `GET /api/seasons`

```json
{"seasons": [2026, 2025, 2024, 2023]}
```

## `GET /api/sessions?year=2024`

`year` is optional. Newest first.

```json
{"sessions": [{"session_key": 9492, "meeting_key": 1232, "year": 2024,
  "meeting_name": "Japanese Grand Prix", "session_name": "Qualifying", "session_type": "Qualifying",
  "circuit": "Suzuka", "country": "Japan",
  "date_start": "2024-04-06T06:00:00Z", "date_end": "2024-04-06T07:00:00Z"}]}
```

## `GET /api/sessions/{key}/drivers`

```json
{"session": {"session_key": 9492, "...": "...", "circuit_length_m": 5781.45},
 "drivers": [{"number": 1, "acronym": "VER", "name": "Max VERSTAPPEN", "team": "Red Bull Racing",
   "colour": "3671C6", "laps": 12, "fastest_lap": 11, "fastest_ms": 88197}]}
```

`colour` is `RRGGBB` without `#` and may be empty. `fastest_lap` and `fastest_ms` are `null` when the driver set no timed lap with telemetry.

## `GET /api/sessions/{key}/laps?driver=1`

```json
{"session_key": 9492, "driver": 1, "laps": [
  {"lap": 1, "duration_ms": null, "sectors_ms": [null, 52689, 20505],
   "pit_out": true, "pit_in": false, "valid_time": false, "has_telemetry": true, "is_fastest": false},
  {"lap": 11, "duration_ms": 88197, "sectors_ms": [30777, 39850, 17570],
   "pit_out": false, "pit_in": false, "valid_time": true, "has_telemetry": true, "is_fastest": true}]}
```

`valid_time: false` means the feed has no line-to-line time for the lap ("unofficial time"); `duration_ms` is then `null`.

## `GET /api/sessions/{key}/laps/{driver}/{lap}?channels=speed,throttle`

Raw samples of one lap, including one sample either side of the lap window. `lap` is a number or `fastest`. `channels` is any of `speed`, `rpm`, `gear`, `throttle`, `brake`, `drs`, `x`, `y`, `z` (default: `speed,throttle,brake,gear,rpm,drs`).

```json
{"session_key": 9492, "driver": 1, "lap": {"lap": 11, "duration_ms": 88197, "...": "..."},
 "samples": 346,
 "t": [-101, 179, 459],
 "distance": [-8.0, 14.18, 36.68],
 "channels": {"speed": [282, 287, 290], "throttle": [100, 100, null]}}
```

`t` is milliseconds from the lap start, `distance` metres from the lap start. Missing readings are `null`. `brake` is 0/1. `drs` is 1 when the flap is open, else 0.

## `GET /api/compare?a={key}:{driver}:{lap}&b={key}:{driver}:{lap}&channels=...&step=5`

Both laps resampled every `step` metres (1–500, default 5) along lap A's circuit length, plus the running delta. The two laps may come from different sessions. `lap` is a number or `fastest`.

```json
{"a": {"session_key": 9492, "session": {"...": "..."}, "driver": {"number": 1, "acronym": "VER", "...": "..."},
       "lap": {"lap": 11, "duration_ms": 88197, "...": "..."}},
 "b": {"...": "..."},
 "step_m": 5, "length_m": 5781.45, "same_circuit": true, "aligned": true,
 "official_delta_s": 0.292, "final_delta_s": 0.292,
 "distance": [0, 5, 10, "...", 5781.45],
 "delta": [0, 0.001, 0.002, "...", 0.292],
 "time": {"a": [0, 0.063, "..."], "b": [0, 0.065, "..."]},
 "channels": {"speed": {"a": [283.8, "..."], "b": [278.5, "..."]}, "gear": {"a": [7, "..."], "b": [7, "..."]}}}
```

- Every array has the same length as `distance`. The last point is the timing line.
- `delta[i]` = `time.b[i] - time.a[i]` in seconds. Positive means B is behind A at that point.
- `aligned` is true when both laps have official times and full telemetry. Then `final_delta_s` equals `official_delta_s` (the difference of the official lap times).
- Values are `null` where a lap has no data.
- Smooth channels are linearly interpolated; `gear`, `drs` and `brake` hold the previous sample.

## `GET /api/predictions`

Races that have a prediction, newest first, and the measured accuracy of the model on races it was not tuned on (see [PREDICTIONS.md](PREDICTIONS.md)). `run` is false for a race whose qualifying is over but which has not been run. Without a predictions file the list is empty.

```json
{"races":[{"session_key":11412,"year":2026,"name":"São Paulo Grand Prix","circuit":"Interlagos","date":"2026-11-08","run":true,"wet":false}],
 "backtest":{"from_year":2025,"model":{"races":40,"winner_hit":0.5,"podium_overlap":1.9,"mae":3.1},"grid_only":{"races":40,"winner_hit":0.45}},
 "generated":"2026-11-24T18:00:00Z"}
```

## `GET /api/predictions/{key}`

One race simulated 10,000 times from inputs that only use earlier races, this race's qualifying and its grid. Deterministic: the seed is the session key. Drivers are ordered by chance of winning. `p10`/`p50`/`p90` are finishing-position percentiles over the runs in which the car finished, `dnf` the chance it does not, `dist[i]` the chance of finishing in position `i+1`, `pace` the expected race pace against the field median in percent of a lap (negative is faster). `actual` and `retired` appear once the race has been run. 404 `not_found` for a session without a prediction.

```json
{"race":{"session_key":11412,"year":2026,"name":"São Paulo Grand Prix","circuit":"Interlagos","date":"2026-11-08","run":true,"wet":false},
 "runs":10000,
 "drivers":[{"number":1,"acronym":"NOR","name":"Lando NORRIS","team":"McLaren","colour":"F47600","grid":1,
   "win":0.41,"podium":0.78,"points":0.9,"dnf":0.1,"mean":3.4,"p10":1,"p50":2,"p90":8,"dist":[0.41,0.2],"pace":-0.52,"actual":1}],
 "inputs":{"laps":71,"pit_loss_s":21.4,"lap_s":73.9,"trained_on":88,"retirement_rate":0.1,"strategies":40}}
```

## `GET /api/health`

```json
{"status": "ok", "manifest_version": 12, "segments": 480}
```

## `GET /metrics`

Prometheus text format. Metrics are prefixed `ghostlap_`.
