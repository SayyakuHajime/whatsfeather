# Performance Acceptance Proposal

All numbers below are provisional targets, not achieved results. Choose a reference Linux machine and renderer during M0, then keep hardware, power profile, Qt version, build flags, display scale and dataset fixed for comparisons.

## Fixture sets

- Small: 20 chats, 2,000 text messages.
- Large: 1,000 chats, 100,000 messages with skew toward one busy group.
- Mixed: text, long links, emoji, replies, delayed image metadata and unavailable media.
- Burst/failure: replayed/out-of-order events, slow subscriber, remote-history timeout, disk-full and disconnect after remote acceptance.

All fixtures synthetic. Real-account comparisons require explicit approval and redacted captures. Do not infer live WhatsApp latency from mocks.

## Initial budgets to validate

| Metric | Proposed target | Method |
| --- | --- | --- |
| Warm local chat open | p95 ≤150 ms | selection to usable first local page; at least 100 switches |
| Warm startup | ≤1.5 s | process start to usable cached shell, no network gate |
| Cold startup | ≤3 s | record cache definition; at least 10 runs |
| Timeline frame time | p95 ≤16.7 ms on reference 60 Hz setup | native profiler during controlled scroll; measure 120 Hz separately |
| Idle CPU | ≤1% of one logical core average | 60 s after sync/media settles |
| Idle memory | combined desktop+backend RSS ≤250 MiB, PSS also reported | standardized loaded fixture; report mapped/shared libraries |
| Loaded messages | ≤500 provisional | assert backing model cap, not only delegate count |
| Media concurrency | ≤2 downloads, bounded queue | cancellation/priority tests |
| Media cache | configurable hard quota, default 1 GiB | quota-full and pinned-file cases |

Failing a target triggers profiling and budget review, not fabricated results or hidden fixture changes. Cold start font/shader/OS caching must be recorded. Renderer A/B includes software and RHI; GPU-accelerated is not automatically faster or more power-efficient.

## Baseline comparison

WhatsApp Web and WhatsFeather must use the same machine, network, comparable chat workload, power state and observation period. Record browser version, extensions, tab load and renderer. Compare local UI latency separately from server sync/download latency. Report distributions and peak/settled memory, not only one flattering screenshot.

## Release evidence

Store raw timings/logs, commit hash, fixture generator seed, machine/Qt/Go/kernel details and reproduction commands with each benchmark. Release claims require those artifacts. UI smoke tests and Go race tests establish different properties and never substitute for this protocol.
