# ProxyOps implementation status

Last reviewed: 2026-11-04
Branch: `product/proxyops-mvp`

This document records repository evidence, not estimated completion percentages.

## Existing implementation verified

### Proxy management API
- `GET /api/v1/proxies`: tenant-scoped list with status, health and keyword filters.
- `GET /api/v1/proxies/{id}`: detail view; returns a password mask only.
- `POST /api/v1/proxies`: create proxy.
- `PUT /api/v1/proxies/{id}`: update proxy; password has keep/clear/replace semantics.
- `DELETE /api/v1/proxies/{id}`: delete only when no environment references it.
- `POST /api/v1/proxies/{id}/test`: perform a live probe and return a result.

### Persistence and credential handling
- Migration `0012_launcher_core.up.sql` creates the tenant-scoped `proxies` table with protocol, port and status constraints plus RLS.
- Migration `0013_proxy_upstream.up.sql` adds proxy type, city and latest probe-result fields.
- PostgreSQL repository is implemented in `platform/store/pg/launcher.go`.
- Memory repository is implemented in `platform/store/mem/launcher.go`.
- The repository read-column list explicitly excludes `password_enc`; ciphertext has a dedicated repository accessor.
- `shop.ProxyService` encrypts passwords with the envelope cipher before persistence and never returns plaintext.
- App wiring constructs `shop.ProxyService` and injects it into the API gateway.

### Existing UI and tests
- The web console has a proxy-management renderer in `gateway/web/app.js`.
- Existing service tests: `shop/proxy_probe_test.go`.
- Existing gateway contract tests: `gateway/p3_proxy_test.go`.

## Gaps to close before calling the MVP complete

1. **Immutable probe history:** current schema stores only the latest result on `proxies`. Add a tenant-scoped probe-history table and repository methods; preserve recent 100 records per proxy in the API.
2. **Trend and reliability metrics:** calculate success rate and latency from persisted history, not from the latest status alone.
3. **Anomaly state and in-app alerts:** support configurable latency threshold and consecutive-failure threshold; avoid duplicate alert spam and clear/recover alerts when the proxy recovers.
4. **Windows desktop acceptance:** verify the Tauri Windows build launches the intended UI, persists API configuration safely, and can complete create/edit/disable/test/history flows against the local service.
5. **Release acceptance:** run Go tests/vet and Windows build on CI or a Windows runner, then publish a clearly versioned first trial package with setup and rollback instructions.

## Execution order

1. Finish probe-history persistence, metrics and anomaly-alert API contracts while retaining the existing proxy CRUD and encryption implementation.
2. Complete and verify the Windows client experience against those APIs.
3. Exercise real proxies and failure cases; verify history, trends and alert recovery.
4. Run on Windows and fix platform-specific issues.
5. Produce the first trial package and a concise seller test guide.

## Security and product boundaries

- Never return proxy passwords or ciphertext from API responses.
- Keep all proxy queries tenant-scoped in both memory and PostgreSQL repositories.
- Probe targets must remain HTTP(S), time-bounded and response-size limited.
- Do not add browser fingerprint spoofing, account automation, proxy resale or a payment system to this MVP.
