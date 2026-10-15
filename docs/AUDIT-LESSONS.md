# Lessons from the WhatsAppGo Audit

Reference source: https://github.com/shukiv/whatsappgo/tree/f0426d1758bf66685b287dfc948dc6368ff41c43

This is a summary of observed paths at that commit, not an allegation about every version and not benchmark evidence for WhatsFeather. The audit cloned source, built Linux desktop, passed Go tests/race/vet and 51 local offscreen desktop tests. Upstream desktop CI for that commit failed; cause was not established. No real WhatsApp account was linked.

| Observation | Source path at audited commit | WhatsFeather design response |
| --- | --- | --- |
| Native Qt + Go/whatsmeow separates UI from protocol | desktop/CMakeLists.txt; go.mod | Keep separation, no WebView |
| Default software renderer | desktop/src/main.cpp:179–184 | Benchmark software/RHI before selecting default |
| Older pages rebuild full loaded message index | desktop/src/messagemodel.cpp:236–255 | Bound backing model and preserve anchor during eviction |
| Auto-download defaults enabled; media collection retains data | internal/store/local_settings.go:9–12; internal/whatsapp/media.go:17–108 | On-demand default, explicit quota and retention |
| Nonblocking event send can drop on full buffer | internal/events/broker.go:18–26 | Revisions/gap detection + snapshot resync |
| Remote-history UI waits for event | desktop/src/rpcclient.cpp:3001–3025 | Timeout, cancel, safe retry |
| Remote send precedes local save | internal/service/service.go:1008–1020 | Durable outbox + unknown-outcome reconciliation |
| Local plaintext storage and broad same-UID RPC | docs/SECURITY.md:18–23,76–82 | Explicit threat model, permissions and encryption decision |
| MCP labels do not enforce approvals | cmd/whatsappmcp/main.go:295–323,350–360 | No MCP in MVP |

No WhatsAppGo code has been copied. Its GPL-3.0-or-later license must be respected if implementation reuse is authorized later. Reuse design lessons without claiming that an unimplemented system already fixes the reference's risks.
