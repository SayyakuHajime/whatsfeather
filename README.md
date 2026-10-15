<div align="center">

# WhatsFeather

**WhatsApp conversations. A native Linux desktop.**

An unofficial WhatsApp client planned around Qt Quick, Go, and whatsmeow.
No browser, no WebView, no Electron.

[Roadmap](ROADMAP.md) · [GUI Design](docs/GUI.md) · [Architecture](docs/ARCHITECTURE.md) · [Security](SECURITY.md)

</div>

## Status

**Planning stage — no application implementation or runnable release yet.**
This repository currently contains the product roadmap and engineering/design specifications.
Native rendering and lower resource use are design goals, not measured results for WhatsFeather.

## Contents

- [Why build this?](#why-build-this)
- [First usable version](#first-usable-version)
- [Technical direction](#technical-direction)
- [Development](#development)
- [Safety and attribution](#safety-and-attribution)

## Why build this?

A keyboard-friendly, local-first Linux chat client that avoids running the WhatsApp Web page.
Prioritize predictable chat opening, stable scrolling, bounded background work, and honest delivery states over feature count.

## First usable version

- Single-account QR linking and explicit unlinking.
- Local chat list and paginated text history, private and existing group chats.
- Send/receive text with receipts, drafts, and clear offline/error states.
- Bounded history model, reconnect reconciliation, and timeout/retry controls.
- Native light/dark interface, search, keyboard navigation, and notification privacy controls.

Images, documents, and playback follow the text reliability milestone. Calls, AI/MCP, status/channels, scheduling, and multi-account are not MVP features.

## Technical direction

```text
Qt 6 Quick/QML + C++ models
            │ private, versioned local RPC
Go application service ── SQLite history/outbox
            │
       whatsmeow dependency
            │
       WhatsApp servers
```

This is a new application repository, not a fork of WhatsAppGo or whatsmeow.
Use upstream whatsmeow as a pinned dependency; maintain a separate protocol fork only if a demonstrated compatibility fix needs one.
See [architecture](docs/ARCHITECTURE.md) and [performance acceptance](docs/PERFORMANCE.md).

## Development

Application build/test commands will be introduced with the first implementation milestone; none exist yet.
Start from [ROADMAP.md](ROADMAP.md). Repository rules are in [AGENTS.md](AGENTS.md).

## Safety and attribution

Not affiliated with, authorized by, or endorsed by WhatsApp or Meta. Unofficial clients can break or cause account restrictions. Never use the main account for initial testing.

The WhatsAppGo source audit informed this plan; no application code from that project is included. Its GPL-3.0-or-later obligations apply if its code is reused later.
See [audit lessons](docs/AUDIT-LESSONS.md), [references](docs/REFERENCES.md), and [security policy](SECURITY.md).

License: **not selected yet; no license grant is made by this repository.** Select a compatible license before accepting external code contributions or distributing an application. Public visibility alone is not an open-source license.

[Back to top](#whatsfeather)
