# Roadmap

**Current assessment: pre-alpha / release no-go.** A runnable native Qt/C++ application and Go/SQLite backend have synthetic end-to-end tests. Limited spare-account evidence is not full WhatsApp parity or production acceptance.

1. **Messaging parity:** finish service-backed send/receive, receipts, reactions/edit/revoke, identity/history reconciliation, structured interaction and all supported media paths. Preserve unknown outcomes rather than resend blindly.
2. **Daily-use native UI:** complete notification delivery, conversation/group details, media browsing, keyboard access, profile/settings and accessibility. Cached/local-only features must stay visibly distinct from remote state.
3. **Broader service features:** groups, communities, channels, status and native calling require their own implementation and interoperability evidence. Disabled buttons or readable summaries do not count as support.
4. **Security and performance:** maintain bounded storage/models/RPC; verify recovery, vulnerability fixes, dependency/runtime licensing and an explicit local-storage threat model. Measure startup, memory, real-renderer interaction and scrolling without unsupported browser comparisons.
5. **Release:** complete project/dependency/runtime licensing and redistribution review, establish private vulnerability reporting, run the supported-toolchain CI matrix, produce packages, and verify clean install/upgrade/rollback, signing and update trust.

Local progress includes reviewed poll voting with encrypted synthetic updates, privacy-safe notification plumbing, cross-chat/per-chat cached media metadata and details, dependency/security checks and startup/resource benchmark tooling. These remain scoped implementation proofs, not closure of the broader release gates. No release date is promised.

The interface remains familiar to WhatsApp users. AI/MCP, bulk messaging and scraping are not core features; view-once/disappearing-content protections must not be bypassed.
