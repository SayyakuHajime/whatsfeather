# Security

WhatsFeather has an early native implementation, not a security-reviewed or production-ready release. There is no supported-version policy or security response SLA yet. Default demo mode never connects to WhatsApp; experimental live mode requires explicit connection approval. Local messages and sessions are not encrypted at rest by the application.

## Account and privacy risks

This unofficial project is not affiliated with WhatsApp or Meta. Protocol changes can interrupt service, and unofficial access may lead to account restrictions. Do not use a primary account for initial experiments.

Never include session databases, QR payloads, pairing codes, credentials or private message contents in public issues, logs or screenshots. Transport encryption must not be mistaken for protection of local files.

## Local controls and verification limits

Profiles, SQLite files and the same-UID Unix RPC use restrictive filesystem permissions. These controls do not protect copied disks/backups or a compromised user account. Application-level local encryption and a supported retention/deletion policy remain release gates.

Notifications and message-text previews are separate opt-ins and default off. The notification stream contains only recent newly committed live inbound identities; initial subscription, overflow and restart do not replay history. Preview text is requested only after explicit opt-in. This does not establish tested delivery through every desktop notification service.

The offline source gate detects selected high-confidence secret patterns and rejects forbidden tracked publication paths without reading their contents. Dependency checksum verification is not a vulnerability or license audit. The CI configuration additionally includes a pinned Go advisory scanner; Qt, codecs, system libraries and packaged contents need separate assessment. A green test suite is not a security certification.

## Reporting

Do not post sensitive vulnerability details in a public issue. Use GitHub private vulnerability reporting if available; otherwise request a private contact without including secrets. A dedicated reporting channel must be established before a release.
