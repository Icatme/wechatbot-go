# PO Agent Worklog

## 2026-08-11

- Hardened the Go CI matrix: PRs now run for every base branch, setup-go reads `go.mod`, all platforms build/test, and Linux additionally verifies modules, vets, and runs race-enabled tests.

## 2026-06-23

- Bumped SDK fallback protocol version to `0.3.0` and hardened runtime bug fixes across root SDK files: login/session send guards, UTF-8 text chunking, QR/API decode errors, CDN/remote HTTP client reuse, post-login account state binding, and matching tests/docs.

## 2026-08-08

- Hardened inbound delivery and outbound validation: added 24-hour persistent replay dedupe, decoupled long polling from ordered handler execution, committed cursors only after batch processing, and rejected missing `context_token` before downloads, uploads, or sends; added focused regression coverage.

## 2026-08-09

- Replaced fire-and-forget inbound callbacks with one context-aware Handler pipeline and explicit Ack/Retry/Drop outcomes; retry and hook failures now leave replay/cursor state uncommitted, handler panics request redelivery, and concurrent Run calls are rejected with lifecycle state reset on exit.

- Added a bounded keyed dispatcher with same-user ordering, cross-user concurrency, bounded uncommitted batches, and an ordered cursor-completion frontier; cursor state now fails closed on load/write errors, concurrent context-token persistence and hook registration are synchronized, and the echo example is concurrency-safe.

- Aligned the public message envelope with official Agent fields: message/item timestamps, session/group/run correlation IDs, and tool-call start/result item types 11/12, with parsed correlation metadata and protocol tests.

- Added a one-item outbound contract with caller-stable/generated `client_id`, failure-safe `SendResult`, per-message `run_id`, tool-call progress builders, and wire-level BeforeSend/AfterSend hooks across text and media sends.

- Preserved endpoint, HTTP status, `ret`, and `errcode` independently in the public API error type; wrapped errors and HTTP error envelopes now retain reliable `-14` classification.

- Replaced the timed `-14` pause with a fail-closed reauthentication state: all authenticated API paths invalidate credentials/config/context once, cancel Run without auto-QR or token reuse, preserve cursor/replay state, recover only through explicit fresh QR reauthentication, and isolate stale auth/context work from the new session with generation checks plus a cancellable request gate.

## 2026-08-11

- Closed reliable-delivery failure gaps: malformed wire messages now fail closed, replay identities are conversation-scoped and committed as one alias set, and credential/replay/cursor/context JSON state uses atomic replacement with disk-before-memory publication.

- Made explicit reauthentication recoverable across process crashes after the durable commit point: a per-credential marker blocks normal login while a transition is active, records every context path, survives fresh credential acquisition, and is removed only immediately before publishing the validated fresh session. Sudden-power-loss directory durability remains outside the atomic-write contract.

- Bumped the SDK fallback protocol version to `0.4.0` and prepared the source-only Go module release after the reliable-delivery, Agent-envelope, outbound-identity, typed-error, and explicit-reauthentication stack merged to `main`.

- Updated README for the `v0.4.0` delivery/session changes and migration notes; replaced promotional repository comparisons with neutral scope, provenance, and synchronization statements.

## 2026-08-12

- Split the 1,804-line root `bot.go` into same-package responsibility files for login, session gating, polling, content, media, message parsing, handlers, and logging; preserved declarations, public API, control flow, and lock ordering, and refreshed the documented Go file structure.

## 2026-08-20

- Normalized unsafe bare `<` characters independently in every outbound wire message and referenced text after hooks on a request-owned copy, so caller templates remain unchanged and concurrency-safe; an explicit small HTML-tag set and line-aware fenced code remain preserved, with Markdown stripping and chunking before the final wire-boundary guard.

- Adapted upstream sensitive-data hardening to the Go SDK: centralized recursive and malformed-input redaction (including quoted/delimited credentials and authority-form URLs), capped upstream API error messages at 4 KiB of valid UTF-8, rebuilt unsafe error wrappers while preserving safe causes, and added API/CDN/QR/remote-media/log leakage regressions.

- Bumped the SDK fallback protocol version to `0.4.1` and prepared the source-only patch release covering sensitive-error redaction, safe outbound bare-`<` normalization, concurrency-safe request copies, and the same-package root implementation split.
