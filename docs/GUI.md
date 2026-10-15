# GUI Direction — Native Messaging, Not a Dashboard

Status: proposed UX specification and wireframe. No rendered application or verified visual implementation exists yet.

## Reference-led direction

Use WhatsApp's familiar task ordering (chat list → conversation → composer), and native KDE/Qt messaging applications such as NeoChat as references for desktop density, keyboard interaction and adaptive panels. Do not copy logos, screenshots, proprietary assets or source. Borrow interaction principles, not a web wrapper or dashboard layout.

References: [Kirigami overview and NeoChat example](https://develop.kde.org/frameworks/kirigami/), [KDE HIG](https://hig.kde.org/), [Qt ListView](https://doc.qt.io/qt-6/qml-qtquick-listview.html).

## Desktop wireframe

```text
┌ WhatsFeather ─────────────────────────────────────────────────────┐
│ Chats                 │ Contact / Group                         │
│ [Search chats] [New]  │ Connection or typing          [Search][…] │
│ All  Unread  Archived ├──────────────────────────────────────────┤
│                       │              Today                       │
│ Avatar  Contact  Time │ Incoming message                         │
│         Preview   3   │                         Outgoing message │
│ Avatar  Group    Time │                         Delivery + time  │
│         Preview       │                                          │
│                       │ [New messages — jump to latest]          │
│                       ├──────────────────────────────────────────┤
│ Connection [Settings] │ [Attach] [Write a message…]       [Send] │
└───────────────────────┴──────────────────────────────────────────┘
```

This is structural, not a pixel-perfect mockup. No permanent navigation rail or third panel in MVP. Contact/group details open on demand in a drawer; settings replace content or open a dedicated page, not a giant dashboard.

## Geometry and scroll ownership

- Initial target 1180×780 logical pixels; verify 900×600, 1280×800, 1440×900 and 200% scale.
- Sidebar starts at 304 logical px, resizable between 280–380; timeline takes remaining width.
- Below 800 px, show chat list OR conversation with Back, not two squeezed columns. Desktop minimum target 640×480; mobile is not an MVP platform.
- Sidebar and timeline independently own scrolling. Header and composer remain fixed; long draft expands up to a bounded height and then scrolls internally.
- Content max width roughly 880 in wide timelines; message bubbles max 72% of available timeline width. Long URLs wrap; copy must preserve original text.
- Compact rows around 68 px; use 4/8/12/16/24/32 spacing tokens rather than per-component arbitrary values.

## Visual language

Calm neutral surfaces, visible dividers, restrained shadows and small radii. Use platform font and native window decorations. No wallpaper by default, gradients, KPI cards, giant welcome banners, decorative green or fake presence badges.

Light/dark/system appearance from semantic tokens: canvas, surface, raisedSurface, text, textMuted, border, selection, outgoingBubble, incomingBubble, focus, warning, danger. Platform accent used sparingly for selection and focus; feather identity stays in brand mark/onboarding. Colors are not finalized hex values; calculate contrast in each theme before implementation approval.

Target WCAG AA: body text 4.5:1, non-text controls/focus 3:1. Native theme cannot exempt measured contrast. Do not convey read state, selection, or errors by color alone.

## Screens and reusable components

| Screen | Components | Important states |
| --- | --- | --- |
| Onboarding | RiskNotice, PairingQr, PairingStatus | disconnected, QR loading/expired, scan pending, linked, revoked/error |
| Chat shell | ChatSidebar, ChatRow, ChatSearch, ConnectionBanner | empty, local loading, syncing, offline, stale, error |
| Conversation | ChatHeader, MessageTimeline, MessageBubble, DateSeparator | cached, loading older, history timeout, no more available, unavailable message |
| Composer | DraftEditor, SendAction, ReplyPreview | empty, editing draft, queued, sending, unknown outcome, failed |
| Media (later) | AttachmentCard, DownloadAction, MediaViewer | not downloaded, progress, canceled, expired, quota full, failed |
| Details/settings | ContactDrawer, SettingsPage, StoragePolicy | read-only detail, confirmation required, cleanup preview, destructive failure |

The UI must distinguish “no more local messages” from “all WhatsApp history is present.” Unavailable calls/channels/status do not get fake enabled buttons.

## Interaction contract

- Enter sends; Shift+Enter inserts newline. User can opt for Ctrl+Enter-to-send.
- Ctrl+K focuses chat search; Ctrl+F searches conversation; Escape closes drawer/search or returns to list in compact mode.
- Tab order: navigation → header actions → message actions → composer. Every icon-only control has tooltip and accessible label.
- Scrolling up suspends auto-follow; incoming messages show jump-to-latest. Loading older messages and media dimensions must not move the reader's anchor.
- Switching chat saves draft, cancels view-specific requests, and rejects stale responses. Selection and copy work across wrapped message text.
- Clicking a link is explicit; automatic third-party link-preview requests default off. No network work solely to decorate history.
- Failed send keeps content and explains retry safety; unknown delivery shows “Check delivery” rather than a blind Retry.
- Close-to-tray only where supported; Quit terminates the app/backend. Explain whether queued messages require the app to remain running.
- Destructive local cleanup shows what will be lost. Unlink device and erase local history are separate actions.

## Design verification before expanding UI scope

Implement one synthetic vertical slice: sidebar → chat open → scroll → draft → mock send/ack. Review actual native screenshots before polishing other screens. Test long names/text, emoji/RTL, 200% scale, keyboard-only use, reduced motion, light/dark and offline/errors. Measure scroll ownership and clipping; offscreen tests are not frame-rate benchmarks.
