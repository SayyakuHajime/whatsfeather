# Security and Privacy Plan

WhatsFeather is an unofficial project at planning stage, not an audited or approved WhatsApp application. There is currently no implementation to report as secure.

## Account risk

Reverse-engineered protocol access can break and may expose accounts to restrictions or bans. Initial tests use an explicitly approved spare number. Never connect a real account, send a message, unlink a device or export a profile merely to verify a build. No bulk sending or scraping features are planned.

## Required controls before alpha

- Private profile directory (0700), owner-only files/socket (0600), peer UID checks, no public TCP listener.
- Session credentials and QR/linking secrets never committed, logged, attached to issues or included in screenshots/fixtures.
- Evaluate native keyring and at-rest encryption; until implemented, explicitly disclose plaintext local DB/media. E2EE transport does not protect an unlocked computer.
- Bounded network/decode work, restricted URL resolution and redirects, safe filenames, no auto-opening executable attachments, no automatic external preview fetching.
- No core MCP/AI integration. Future automation needs enforceable least-privilege scopes, confirmation and audit boundaries.
- Signing/provenance plan before binary distribution; checksums alone are not independent publisher authentication.
- User-visible retention policy. View-once payloads must not be archived/exported; disappearing-message semantics must be implemented and tested or explicitly marked unsupported, not silently claimed compatible.
- Separate unlink, local deletion, media cache eviction and profile export. Export remains out of MVP until a safe credential-transfer design exists.

The same Unix user is a trust boundary limitation: permissions cannot protect credentials against arbitrary malicious programs running as that user. Full-disk encryption and a locked session complement, but do not replace, application controls.

## Reporting

Do not post credentials or private conversation data in a public issue. Use GitHub private vulnerability reporting if enabled; otherwise request a private contact without including exploit secrets. A dedicated contact and supported-version policy must be established before a release. Do not assume this planning repository provides a security response SLA.
