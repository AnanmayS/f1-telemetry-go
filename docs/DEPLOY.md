# Deploying

Two ways to host: **Vercel** (running now, limited data) or a **VPS** (the PRD's target, full corpus). The Vercel section is first; the VPS guide follows.

## Vercel

Project `ghostlap`, linked to this repository. The frontend is built by Vite and served from Vercel's CDN; `/api/*` is rewritten to one Go function (`api/index.go`) that runs the same HTTP layer as `cmd/server` over segments bundled into the deployment. Configuration is in `vercel.json`.

**Limit: a Vercel Function may be at most 250 MB uncompressed, so only a bundle of the newest sessions is hosted** (default budget 200 MiB, roughly 70–80 sessions of about 470). The full corpus does not fit; it needs the VPS, or segments moved to object storage and fetched on demand.

Publish data and code:

```bash
deploy/vercel/publish.sh
```

It builds a bundle with `cmd/bundle`, commits it on top of the current commit and force-pushes the single-commit branch `vercel-deploy`. Segments never enter `main`'s history.

Pushing that branch creates a **preview** deployment, because the project's production branch is `main` (which is excluded from deploys: it has no data). To make every publish go live, set Production Branch to `vercel-deploy` once in the Vercel dashboard (Project → Settings → Environments → Production). Until then, promote the preview from the dashboard.

Differences from the server:

- No `/metrics` and no visitor counters: the page is served by the CDN, and function instances are short-lived. Use Vercel's own analytics if wanted.
- The rate limit and both caches are per function instance. Shared caching comes from the CDN: data responses carry `s-maxage`.
- No live path (no long-running process, no writable disk).

---

The rest of this document is the VPS deployment.

Target: one small VPS (2 vCPU, 2–4 GB RAM, about 20 GB disk) behind Cloudflare. Nothing here has been run against a real host yet; the image was built and smoke-tested locally (`docker build`, then health, index page and a comparison served from a read-only container).

## 1. Build the data

Ingest runs anywhere with Go and network access. A cold run of every season is about 20,000 requests at OpenF1's free limit of 30 per minute, so roughly 11–12 hours. It is resumable: stop it and rerun.

```bash
go run ./cmd/ingest -year all
```

Copy `data/MANIFEST` and `data/segments/` to the server (`rsync -a`). The raw cache (`data/raw/`) is only needed to rebuild segments without refetching; keep it wherever ingest runs.

After each race weekend, rerun the same command (it fetches only new sessions) and sync again. The server notices the new manifest within 30 seconds; no restart.

## 2. Run the server

```bash
cd deploy
echo "TUNNEL_TOKEN=<token from the Cloudflare dashboard>" > .env
docker compose up -d --build
```

`deploy/docker-compose.yml` runs the server bound to `127.0.0.1:8080` (read-only filesystem and data mount, no capabilities, non-root, `-trust-proxy` on) and a Cloudflare Tunnel that provides HTTPS without opening any inbound port. Point the tunnel's public hostname at `http://ghostlap:8080`.

Without Docker: `go build -o server ./cmd/server && ./server -data data -static web/dist -trust-proxy` after `cd web && npm ci && npm run build`.

Only pass `-trust-proxy` when every request arrives through Cloudflare. It makes the rate limiter and visitor counter read the client address from `CF-Connecting-IP`; exposed directly, that header can be forged.

## 3. Cloudflare

- Cache rule: "Eligible for cache" for `/api/*` and `/assets/*`, respecting origin `Cache-Control`. Lap and compare responses are `max-age=604800` with an `ETag`; listings are 5 minutes; live sessions 2 seconds.
- Do not cache `/metrics`; better, block it at the edge (WAF rule: path equals `/metrics` then block) and scrape it over the private network or an SSH tunnel.
- Compression and HTTP/3 on. No Rocket Loader, no injected analytics: the site ships no third-party scripts.

## 4. Monitoring

- Uptime: any external checker on `GET /api/health` (expects `{"status":"ok",...}`).
- Prometheus: scrape `/metrics`. Useful series:
  - `ghostlap_http_request_duration_seconds` by route (alert on compare p99 > 50 ms)
  - `ghostlap_lap_cache_hits_total` / `ghostlap_lap_cache_misses_total`, `ghostlap_response_cache_*`
  - `ghostlap_decode_errors_total` (should stay 0; non-zero means a damaged segment file)
  - `ghostlap_lap_time_mismatch_total`
  - `ghostlap_segments`, `ghostlap_manifest_version`

## 5. Usage numbers

Counted by the server itself, with no cookies and no third parties:

- `ghostlap_page_views_total`: page loads.
- `ghostlap_share_link_opens_total`: page loads that arrived with a comparison in the URL.
- `ghostlap_http_requests_total{route="compare"}`: comparisons served by the origin (Cloudflare cache hits never reach it; use Cloudflare analytics for totals).
- `ghostlap_unique_visitors_today`: distinct visitors since 00:00 UTC. Addresses are hashed with a random salt that lives only in memory and is replaced daily; nothing is written to disk. The finished day's count is logged once at rollover (`"daily visitors"`).

## 6. Replaying a session through the live path

No free live feed exists (see `docs/DATA.md`), so the live path is driven from a recorded session:

```bash
./server -data data -replay-year 2024 -replay-session 9492 -replay-speed 10
```

The session must be in the raw cache and not yet in the manifest. Laps become comparable as they complete; when the replay ends the session is sealed into a normal segment.
