# Architecture Proposal

Status: proposed design, not implemented. Revisit decisions based on M0 spikes, not preference alone.

## Components and ownership

| Layer | Proposed technology | Owns |
| --- | --- | --- |
| Desktop | Qt 6 Quick/QML, C++ QAbstractListModel | Rendering, input, bounded view models, accessibility |
| Application service | Go | Lifecycle, use cases, connection status, RPC, outbox/reconciliation |
| WhatsApp adapter | Pinned upstream whatsmeow | Protocol, encryption/session transport, wire event mapping |
| Local store | SQLite | Messages, cursors, identity aliases, outbox, sync revisions, settings |
| Media service | Go worker queue + quota-controlled files | Download validation, integrity, eviction, decoder handoff |

Qt Quick is the rendering layer: no QtWebEngine, WebKit, embedded WhatsApp Web page, or browser automation. Optional KDE primitives may be evaluated, but core UX must not require a KDE desktop. Default renderer is chosen by benchmark with software fallback available.

## Proposed repository layout

```text
backend/
  cmd/whatsfeatherd/
  internal/{config,rpc,service,store,outbox,media,transport/whatsapp}/
desktop/
  src/{models,rpc}/
  qml/{components,pages}/
  tests/
tests/fixtures/
docs/
packaging/
```

Only documentation exists today; these directories are implementation targets.

## Native/backend boundary

Desktop owns one backend for its active profile. Start lazily; exit/quit cleans up owned processes. Closing to tray must be explicit and configurable. No always-on system service required for MVP.

Use a private Unix-domain socket (0700 parent, 0600 socket), peer-UID verification and no TCP listener. These controls do not isolate malicious same-user software; document this limit. No AI/MCP endpoint is shipped in MVP.

Versioned request/response protocol with IDs, structured error codes, maximum frame sizes, deadlines, cancellation and bounded concurrency. Subscription events include revision/sequence; gaps and overflows trigger invalidation+snapshot, not silent event loss. Slow clients cannot hold DB locks or block transport callbacks indefinitely.

## Persistence and sending truth

Persist normalized incoming message and sync revision transactionally before publishing an event. Merge duplicate/replayed events by protocol ID and canonical chat identity. Keep original identifiers for reconciliation; do not silently normalize malformed IDs.

Persist outgoing intent before transport submission. Use a stable message ID when the protocol supports it. States distinguish queued, submitting, accepted remotely, delivered, read, failed and unknown outcome. A local save failure after remote acceptance must not appear as a safe-to-retry unsent draft. Reconcile before retrying unknown outcomes; do not promise exactly-once delivery.

At-rest encryption is an M0/M4 decision: compare native keyring-wrapped keys and SQLite encryption integration with dependency/backup complexity. Until implemented and tested, describe data as plaintext protected by filesystem permissions; do not call E2EE transport local-disk encryption.

## Bounded resources

Start with 50-message pages, cap loaded rows at a provisional 500, and implement anchor-aware eviction/reload. Virtualization alone is insufficient. Index timeline by (chat, timestamp, message ID); FTS search must not scan all messages on every keypress.

Bound subscription queues, RPC bodies, outbox, image decode dimensions, network response sizes, media workers and disk storage. Baseline policy: media manual/on-demand, configurable 1 GiB media cache, no automatic video/document backfill. Evicted media may no longer be downloadable from WhatsApp: warn, distinguish pinned downloads from cache, and never silently delete text/session/outbox data to satisfy media quota.

## Failure model

- Offline: cached history available; distinguish draft from explicitly queued send.
- Backend restart: reconnect with backoff; snapshots repair missed events.
- Remote-history timeout: visible retry without duplicating concurrent requests.
- Storage full: stop accepting durable sends, surface actionable error, preserve state.
- Logout/revocation: stop reconnect loops; require relink, do not auto-delete local history.
- Profile migration: transactional schema versioning and recovery/backup policy before release.

## Dependency and reuse policy

Use whatsmeow through an adapter and pin a tested revision. Protocol changes can still break compatibility. Only fork whatsmeow for a reproducible internal fix, kept separately from this app.

WhatsAppGo is a reference/audited predecessor, not a vendored dependency. Architectural ideas are not evidence of matching implementation quality. Copying its code later requires preserving GPL-compatible licensing and attribution; current project license remains undecided.
