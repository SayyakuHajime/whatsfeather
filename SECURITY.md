# Security

WhatsFeather has an early native implementation, not a security-reviewed or production-ready release. There is no supported-version policy or security response SLA yet. Default demo mode never connects to WhatsApp; experimental live mode requires explicit connection approval. Local messages and sessions are not encrypted at rest by the application.

## Account and privacy risks

This unofficial project is not affiliated with WhatsApp or Meta. Protocol changes can interrupt service, and unofficial access may lead to account restrictions. Do not use a primary account for initial experiments.

Never include session databases, QR payloads, pairing codes, credentials or private message contents in public issues, logs or screenshots. Transport encryption must not be mistaken for protection of local files.

## Local controls and verification limits

Profiles, SQLite files and the same-UID Unix RPC use restrictive filesystem permissions. These controls do not protect copied disks/backups or a compromised user account. Production release requires application-managed encryption backed by the OS keyring, with tested migration, key-loss behavior and crash recovery; there must be no plaintext fallback when the keyring is unavailable. This requirement is not yet implemented in the application. A supported retention/deletion policy remains a release gate.

Strict live-test mode currently refuses Connect before opening the device store or contacting WhatsApp. The pinned SDK can download or persist account metadata before application event filters run; those filters alone do not enforce an account-wide privacy scope. Ordinary experimental live mode is not a scoped mode and must not be used to bypass a restricted test authorization.

Notifications and message-text previews are separate opt-ins and default off. The notification stream contains only recent newly committed live inbound identities; initial subscription, overflow and restart do not replay history. Preview text is requested only after explicit opt-in. This does not establish tested delivery through every desktop notification service.

The offline source gate detects selected high-confidence secret patterns and rejects forbidden tracked publication paths without reading their contents. Dependency checksum verification is not a vulnerability or license audit. The CI configuration additionally includes a pinned Go advisory scanner; Qt, codecs, system libraries and packaged contents need separate assessment. A green test suite is not a security certification.

## Reporting

GitHub private vulnerability reporting is enabled. Use [Report a vulnerability](https://github.com/SayyakuHajime/whatsfeather/security/advisories/new), not a public issue, for sensitive reports. Describe the affected revision, impact and a minimal synthetic reproduction; omit credentials, session databases and private conversation data. If the private form is unavailable, request a private contact without posting exploit details.

There are no supported production versions yet. The current pre-alpha receives best-effort investigation only, with no guaranteed response or remediation SLA. A production release must define its supported-version and update policy before publication.
