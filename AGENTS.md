# WhatsFeather Project Rules

## Current scope

Planning-stage native Linux messaging app. Read README.md, ROADMAP.md, docs/ARCHITECTURE.md, docs/GUI.md, SECURITY.md and docs/PERFORMANCE.md before implementing. Do not claim proposed features are implemented.

## Boundaries

- Native Qt Quick UI and Go backend; no embedded browser, WebView, WhatsApp Web wrapper or Electron.
- Use upstream whatsmeow as a pinned dependency, not this application repo as a library fork.
- Preserve unrelated work and private local artifacts. No commits/pushes without explicit user approval.
- Never connect an account, send messages, scan QR secrets, unlink, export or delete user data without explicit task authorization. Fixtures are synthetic; CI has no real credentials.
- No AI/MCP, scraping, bulk messaging or unsolicited automation in MVP.
- Do not copy WhatsAppGo code without licensing/attribution review. License remains undecided until user approval.

## Engineering gates

Add regressions before fixes. Use bounded models, queues, media storage and decoding; DB/network/image work does not block the GUI thread. Preserve IDs and distinguish local queued state from remote acceptance. Unknown outcomes require reconciliation, not blind resend.

Build and test commands must be introduced and executed with implementation; there are no working application commands yet. Native UI must be reviewed through rendered screenshots and interaction tests at both themes/scales. Offscreen passing is not a performance benchmark. Record commit, environment and fixtures for performance claims.

## Delivery

Report what changed, real verification, blockers and remaining risks. Never label a mock as live WhatsApp compatibility. Update roadmap status only when exit criteria are demonstrated.
