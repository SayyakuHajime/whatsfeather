<a id="readme-top"></a>
<div align="center">
<h3>WhatsFeather</h3>
<p>A native WhatsApp client for Linux.<br/>Qt Quick / C++ · Go / whatsmeow · SQLite</p>
<p>No browser, WebView or Electron.</p>
<img src="https://img.shields.io/badge/Qt_Quick-41CD52?style=for-the-badge&logo=qt&logoColor=white" alt="Qt Quick" />
<img src="https://img.shields.io/badge/Go-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go" />
</div>

---

<details>
<summary>Table of Contents</summary>
<ol>
<li><a href="#status">Status</a></li>
<li><a href="#getting-started">Getting Started</a></li>
<li><a href="#implemented-paths">Implemented Paths</a></li>
<li><a href="#live-mode">Experimental Live Mode</a></li>
<li><a href="#verification">Verification</a></li>
<li><a href="#license">Disclaimer and License</a></li>
</ol>
</details>

## Status
<a id="status"></a>

**Pre-alpha, not a production-ready WhatsApp replacement.** The native application and synthetic demo are runnable. Limited spare-account tests do not establish complete service compatibility, phone/peer delivery, or feature parity. No speed or memory advantage over WhatsApp Web has been established.

See [Roadmap](ROADMAP.md) and [Security](SECURITY.md). Calls, full group/channel/status management, complete identity/history reconciliation, encrypted local storage, and supported release packages remain open work.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>

---

## Getting Started
<a id="getting-started"></a>

Requires Linux, Go 1.27.3+, CMake 3.22+, a C++20 compiler and Qt 6.5+ development modules: Core, Gui, Widgets, Quick, QuickControls2, QuickDialogs2, Network, Multimedia and Test. Host verification uses Qt 6.12; committed-source CI has also passed on Qt 6.5.3 and 6.8.3. Media preparation and synthetic media tests also require `ffmpeg` and `ffprobe`.

```bash
./scripts/build.sh
./scripts/run.sh --demo
```

The scripts do not install system packages. Demo history and drafts are stored separately, normally under `~/.local/share/whatsfeather/demo`. Demo sends save locally: no account connection, simulated delivery, or automatic replies.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>

---

## Implemented Paths
<a id="implemented-paths"></a>

- Native chat layout, light/dark themes, compact navigation, conversation/message search, per-chat drafts and emoji insertion.
- Bounded timeline (500 loaded messages), private same-UID Unix RPC, single-owner profiles and explicit unknown send outcomes without automatic replay.
- Typed local media staging/review, explicit incoming downloads, native viewing/playback, static WebP stickers, and image/document/video/audio transport paths. Format limits and experimental live acceptance still apply.
- Explicit Wikimedia Commons GIF search and review; no bundled GIPHY key. GIF animation and GIF-to-video paths have synthetic tests.
- Protocol reply/reaction/edit/revoke implementations, local-only organization/star/pin state, and inert readable cards for unsupported interactive actions.
- Poll creation/result rendering and reviewed voting with SDK encryption/decryption and durable unresolved-outcome guards. Live voting compatibility remains unverified; list/button/native-flow actions remain unavailable.
- Cross-chat and per-conversation cached-media metadata views plus conversation details, limited to retained local data. Link-only entries and media not retained locally are omitted; nothing downloads or opens automatically, and this is not a complete remote gallery.
- Opt-in native tray notifications. Message-text previews default off; history, outgoing messages, duplicates, muted chats and active-visible chats are suppressed. Actual live-service notification delivery remains unverified.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>

---

## Experimental Live Mode
<a id="live-mode"></a>

```bash
./scripts/run.sh --live
```

Live mode uses a separate profile and stays disconnected until explicit Connect confirmation in Settings. The pinned [whatsmeow](https://github.com/tulir/whatsmeow) adapter provides protocol connectivity without a browser. Only use an authorized spare account after reviewing the risks.

Pairing and selected self-send/community paths have limited test evidence; this is not broad live acceptance. Older-history requests are explicit and bounded to the selected chat; a submitted request is not proof that history was recovered. View-once and disappearing content remains unavailable rather than archived. Voice/video calls remain unavailable.

Strict `--live-test-scope` sessions currently reject Connect before device-store or network access: SDK-internal metadata handling precedes application filters and cannot yet enforce the requested privacy boundary. Ordinary `--live` is not scope-restricted and is not a workaround for a restricted pilot.

### Offline profile migration

After `./scripts/build.sh`, stop WhatsFeather and run the one-time migration into a new encrypted destination:

```bash
./build/bin/whatsfeatherd --migrate-live-source /absolute/path/to/legacy-live \
  --data-dir /absolute/path/to/new-encrypted-vault \
  --runtime-dir /absolute/path/to/private-runtime
```

Migration requires and holds the existing `profile.lock` through completion, which excludes normal daemon writers that honor the lock. It cannot stop same-user tools that ignore that lock; a final source rescan rejects observed changes, but keep the source until you verify the result.

If it is interrupted, the destination stays inactive and migration does not resume in place. Copy and hash cancellation is checked between 64 KiB chunks and migration operations; an in-flight kernel read/sync or SQLite call must return before cancellation is observed. Keep the plaintext source and incomplete vault untouched; retry from the same source to a different, never-before-used `--data-dir`. Never delete the source or reset/reuse an existing vault as a retry step. Retain the source until you have independently verified the encrypted profile and your backup.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>

---

## Verification
<a id="verification"></a>

```bash
./scripts/verify.sh
python3 scripts/check-release.py --mode verify
python3 scripts/benchmark_backend.py --case small --runs 3
python3 scripts/benchmark_desktop.py --runs 3
```

The canonical verifier uses synthetic isolated profiles, Go race tests/vet, native/controller tests, private RPC, rendered UI checks, relocated install/reinstall and offline source/dependency checks. It prints a unique private evidence directory and does not connect an account. Desktop benchmarking additionally requires X11/XWayland, `xdotool` and `wmctrl`; its readiness/resource observations are not scrolling or browser-comparison benchmarks.

Headless rendered-video verification needs a working renderer, not only decoded-frame signals. With Xvfb installed, the CI-compatible command is `WHATSFEATHER_VIDEO_QPA_PLATFORM=xcb xvfb-run -a -s '-screen 0 1280x900x24' ./scripts/verify.sh`. Pure offscreen/software rendering does not establish video-pixel acceptance.

[Hosted verification](https://github.com/SayyakuHajime/whatsfeather/actions) includes offline/Go checks and the two Qt baselines; a green run covers its exact committed revision, not later local changes. Release-mode checks require the root LICENSE and asset-license manifest alongside the offline source-secret and Go-checksum gates; they do not interpret license terms, independently verify authorship, audit third-party compatibility, run vulnerability scans, sign artifacts, or attest provenance. Passing these narrow checks does not replace legal, packaging, device or live-service acceptance.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>

---

## Disclaimer and License
<a id="license"></a>

Unofficial; not affiliated with, authorized by or endorsed by WhatsApp or Meta. Protocol changes and account restrictions are possible. Local history, media and sessions are not encrypted at rest by this application.

**Project license: GPL-3.0-only** (see [LICENSE](LICENSE)). This applies to original WhatsFeather code and the 32 listed image/vector assets; [ASSET-LICENSES.md](LICENSES/ASSET-LICENSES.md) records the explicit asset grant and file hashes. Asset authorship is owner-self-attested, not independently verified. [THIRD-PARTY-NOTICES.md](LICENSES/THIRD-PARTY-NOTICES.md) reproduces recursively attributed vendored notices and selected modernc generated-source headers, with canonical LGPL-2.1 and GCC Runtime Library Exception 3.1 texts. These observed headers do not settle their applicability to generated declarations; legal/maintainer review of applicability and any static-link/relink route remains open. Corresponding-source availability and reproducible build instructions are also not yet prepared. The runtime inventory marks gocryptfs, libsecret/secret-tool, and fuse3/fusermount3 as required by encrypted mode with packaging pending; Qt metadata license options are alternatives, not a selected license. This inventory is not a complete compatibility, provenance, or distribution-compliance review.

<div align="right"><a href="#readme-top">↖ Back to top</a></div>
