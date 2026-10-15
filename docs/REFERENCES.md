# References and Evidence Boundaries

- [whatsmeow](https://github.com/tulir/whatsmeow): intended upstream Go protocol dependency; unofficial multi-device implementation, not a GUI. Verify current capability and license at the pinned version before implementation.
- [WhatsAppGo audited source](https://github.com/shukiv/whatsappgo/tree/f0426d1758bf66685b287dfc948dc6368ff41c43): architecture and failure-path reference; no source copied. See AUDIT-LESSONS.md.
- [KDE Kirigami framework](https://develop.kde.org/frameworks/kirigami/): public native desktop/adaptive UI references, including NeoChat. Reference, not a dependency commitment.
- [KDE Human Interface Guidelines](https://hig.kde.org/): proposed platform interaction/accessibility reference for implementation.
- [Qt Quick ListView](https://doc.qt.io/qt-6/qml-qtquick-listview.html): delegate reuse/cache/model behavior reference; virtualization must not be confused with total model bounds.

GUI geometry and performance budgets in this repository are our proposals, not claims endorsed or measured by these sources. No benchmark for WhatsFeather exists yet.
