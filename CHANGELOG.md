# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Per-target `weight` in upstream pools (a target is a URL or `{url, weight}`, weights 1-1000, default 1). `round_robin` uses smooth weighted round-robin and `least_conn` compares in-flight requests per unit of weight (#1).
- `slow_start` on upstream pools: a target that turns healthy again ramps linearly from 10% to 100% of its weight over the window (#1).
- Admin `/upstreams` endpoint (`admin.status_path`) with per-target health, circuit state, in-flight requests, configured and effective weights (#1).

- CodeQL analysis for Go and GitHub Actions on pushes and pull requests to `main` and on a weekly schedule.
- Dependabot version updates for Go modules, Docker and GitHub Actions, with minor and patch updates grouped per ecosystem.
- README screenshots: a curl session against gateway-demo and the test/benchmark run.
- Roadmap items tracked as GitHub issues under the v0.2.0 milestone.

### Changed

- The demo `catalog` pool uses a 20s slow-start.
- Demo configuration and tests use fictional courier partner names and reserved `.test` domains (the `partners` key set now lists `swift-courier` and `river-logistics`).

## [0.1.0] - 2026-10-25

First public release of go-api-gateway.

### Added

- **Routing**: host match (exact or `*.example.com`) and longest path-prefix match on segment boundaries, method filters with proper `405` + `Allow`, optional prefix stripping
- **Upstream pools**: round-robin or least-connections, active health checks with healthy/unhealthy thresholds, per-target circuit breaker (closed → open → half-open probes)
- **Retries**: idempotent, bodiless requests are retried on a *different* target after connection errors or 502/503/504, with backoff, inside the route's overall timeout
- **Auth**: JWT (HS256/384/512, RS256/384/512 from a PEM file or a JWKS URL with caching, rotation and fetch throttling), issuer/audience/expiry checks, claims forwarded as headers; API keys stored as SHA-256 digests and compared in constant time
- **Rate limiting**: token bucket per client IP, authenticated consumer or any header; sharded in-memory buckets or an atomic Lua script in Redis shared by all replicas; `RateLimit-*` and `Retry-After` headers; fail-open or fail-closed when Redis is down
- **Edge hygiene**: request IDs (kept from a trusted edge or generated), `X-Forwarded-For` honoured only from trusted proxies, body size limits, per-route timeouts mapped to `504`, CORS with credential-safe origin echo, gzip that skips small, binary, pre-encoded and SSE responses, JSON error envelope
- **Operations**: structured `slog` access logs, Prometheus metrics, `/healthz` and `/readyz` (fails while draining or when a pool has no healthy target), `SIGHUP` hot reload that keeps the old config if the new one is invalid, graceful shutdown with a drain delay
- **Packaging**: static binaries on distroless (37 MB image, non-root), compose stack with Redis and demo upstreams, `-check` to validate configs in CI, `-healthcheck` for container probes

[Unreleased]: https://github.com/AkibUllah/go-api-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/AkibUllah/go-api-gateway/releases/tag/v0.1.0
