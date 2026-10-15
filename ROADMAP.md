# WhatsFeather Roadmap

## Product contract

Build a native, local-first WhatsApp client for Linux that remains responsive during message sync, media work, and reconnection. Linux desktop is the target, not a mobile clone. Performance and reliable delivery take priority over feature parity.

Current status: repository and documentation only. All implementation milestones below are pending. Sequence is gated by evidence, not calendar promises.

## M0 — Foundation and risk spikes

Deliver:
- Select project license; record dependency licenses and any borrowed code attribution. No WhatsAppGo code copying without GPL review.
- Choose supported Qt/Go baseline, use a pinned upstream whatsmeow version, reproducible dependency files and minimum Linux matrix.
- Establish desktop build, backend build, CI, synthetic fixtures, isolated XDG directories, and redacted diagnostics.
- Spike Qt Quick RHI versus software rendering on the target machine; choose measured default and documented fallback.
- Spike local RPC framing/version negotiation and controlled startup/shutdown.
- Validate linking, reconnect, and obtainable history using an explicitly authorized spare account; no account automation in CI.

Exit: clean-checkout builds; mock chat renders; profile permissions and lifecycle tests pass; linking/reconnect compatibility is either demonstrated or reported as a blocker. License chosen before any release.

## M1 — Native read-only chat shell

Deliver:
- QR onboarding, refresh/expiry/cancel states, persistent session and explicit unlink.
- Two-panel desktop shell, searchable chat list, selected-chat header, virtualized timeline, settings shell.
- SQLite migrations, timestamp+ID cursor pagination, canonical PN/LID identity handling and deduplication.
- Local-first opening with 50-row pages; bounded loaded model; preserve scroll anchor while prepending/evicting.
- Connection/sync state separate from UI loading; own cached history remains readable offline.

Exit: fixtures with 100,000 messages/1,000 chats do not cause full-history load; rapid chat switching cannot mix results; drafts are not yet sendable; no false online/complete-history states.

## M2 — Reliable text MVP

Deliver:
- Send/receive text in private and existing group chats; native selection/copy, safe URL activation.
- Persistent draft per chat, explicit queued/sending/accepted/delivered/read/failed/unknown states.
- Durable outbox; preassigned stable IDs where supported; reconcile acknowledgements after reconnect. Do not blindly resend unknown outcomes.
- Local RPC request deadlines, sequence/revision gap detection and snapshot resync, reconnect backoff with jitter.
- Remote-history timeout and retry; duplicate history/event merges; restart recovery.
- Explicit send action; typing indicators and receipt controls do not block UI.

Exit: failure-injection tests cover network loss, crash before/after remote acceptance, DB write failure, delayed acknowledgements, duplicate events, buffer overflow and reordered responses. Text loop works on an approved spare account. No exactly-once claim without evidence.

## M3 — Daily-use alpha

Deliver:
- Local full-text search with indexed queries, bounded results, and jump-to-message.
- Notifications with preview privacy; tray integration where supported; close/quit behavior explicit.
- Images/documents first, then audio/video playback and voice recording (permission and cancellation handling).
- Replies, reactions, editing and deletion with truthful server results; scope if protocol coverage differs.
- Bounded downloads/decode workers; on-demand media default; hard storage quota and transparent retention settings.
- Light/dark/system themes, complete keyboard navigation, accessible labels and scale testing.

Exit: one-hour soak and restart/reconnect suite; storage-full and expired-media recovery; no unexplained background downloads. User reviews real native GUI. Call this alpha, not production-ready.

## M4 — Performance and privacy release gate

Deliver:
- Execute the published benchmark protocol; compare with WhatsApp Web under matching conditions.
- Profile cold/warm launch, chat opening, scrolling, idle CPU/RSS, disk growth and sync workloads.
- Enforce loaded-row/media/cache/queue budgets and cancellation; prove UI survives slow disk and sync bursts.
- Document threat model, at-rest protection decision, export policy and disappearing/view-once semantics.
- Independent code review and dependency scan, redacted bug reports, backup/restore tests.

Exit: meet or explicitly renegotiate docs/PERFORMANCE.md targets with recorded evidence; no high-risk unresolved delivery/data-loss findings. No main-account recommendation based only on mocks.

## M5 — Preview packaging and maintenance

Deliver:
- Source build and AppImage first; Flatpak after portal and sandbox compatibility tests; AUR recipe after packaging is stable.
- Signed release manifest/artifacts, pinned workflow actions, checksums and release provenance where feasible.
- Manual update first. Automatic updater is deferred until trust/rollback design is reviewed.
- Release migration/rollback policy, crash recovery, supported distro/Qt matrix and protocol-update smoke checklist.

Exit: clean-machine install/launch, own-profile upgrade and uninstall tested; preserve user data by default; releases blocked by failed required checks. No private credentials in build artifacts.

## Later, only after demand

Multi-account, group administration, stickers, status/channels, scheduling, cross-platform ports, and optional automation. AI/MCP is not part of the core client; any future integration requires enforced method scopes and approval boundaries, not prompt-only warnings. Voice/video calls depend on upstream support or a separately validated implementation and are not promised.

## Suggested implementation slices

Proposed paths, not existing application files:
1. `backend/cmd/whatsfeatherd`, `backend/internal/config` — private profile roots and process lifecycle tests.
2. `backend/internal/rpc` — protocol handshake, bounded framing, deadlines and unauthorized access tests.
3. `desktop/src/main.cpp`, `desktop/qml/App.qml` — mock shell and renderer probe.
4. `backend/internal/store` — migration, fixture import and cursor tests.
5. `desktop/src/models`, `desktop/qml/components/MessageTimeline.qml` — anchor-stable bounded model.
6. `backend/internal/transport/whatsapp` — pinned whatsmeow adapter and event mapping tests.
7. `backend/internal/outbox` — failure-injected sending/reconciliation.
8. `desktop/tests`, `backend/internal/*/*_test.go` — gates attached to every vertical slice.

For each slice: failing regression → minimal implementation → focused tests → full gates. Commit only with explicit approval. Do not add placeholder APIs that imply unsupported capabilities.
