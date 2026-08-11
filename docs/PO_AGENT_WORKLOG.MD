# PO Agent Worklog

## 2026-08-11

- Hardened the Go CI matrix: PRs now run for every base branch, setup-go reads `go.mod`, all platforms build/test, and Linux additionally verifies modules, vets, and runs race-enabled tests.

## 2026-06-23

- Bumped SDK fallback protocol version to `0.3.0` and hardened runtime bug fixes across root SDK files: login/session send guards, UTF-8 text chunking, QR/API decode errors, CDN/remote HTTP client reuse, post-login account state binding, and matching tests/docs.

## 2026-08-08

- Hardened inbound delivery and outbound validation: added 24-hour persistent replay dedupe, decoupled long polling from ordered handler execution, committed cursors only after batch processing, and rejected missing `context_token` before downloads, uploads, or sends; added focused regression coverage.

## 2026-08-09

- Replaced fire-and-forget inbound callbacks with one context-aware Handler pipeline and explicit Ack/Retry/Drop outcomes; retry and hook failures now leave replay/cursor state uncommitted, handler panics request redelivery, and concurrent Run calls are rejected with lifecycle state reset on exit.

## 2026-08-11

- Closed reliable-delivery failure gaps: malformed wire messages now fail closed, replay identities are conversation-scoped and committed as one alias set, and credential/replay/cursor/context JSON state uses atomic replacement with disk-before-memory publication.
