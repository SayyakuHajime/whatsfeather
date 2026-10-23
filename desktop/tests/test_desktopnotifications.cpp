#include "desktopnotifications.h"

#include <QApplication>
#include <QSettings>
#include <QSystemTrayIcon>
#include <QTemporaryDir>
#include <QtTest>

#include <memory>

namespace {
class FakeNotificationDelivery final : public DesktopNotificationDelivery {
public:
  bool trayAvailable() const override { return trayAvailableValue; }
  bool notificationsAvailable() const override {
    return notificationsAvailableValue;
  }
  void setTrayVisible(bool visible) override { trayVisible = visible; }
  void setToolTip(const QString &toolTip) override { lastToolTip = toolTip; }
  void showNotification(const QString &title, const QString &body,
                        int timeoutMs) override {
    notifications.append({title, body, timeoutMs});
  }
  void setOpenHandler(std::function<void()> handler) override {
    openHandler = std::move(handler);
  }
  void setQuitHandler(std::function<void()> handler) override {
    quitHandler = std::move(handler);
  }
  void setNotificationClickHandler(std::function<void()> handler) override {
    notificationClickHandler = std::move(handler);
  }

  struct ShownNotification {
    QString title;
    QString body;
    int timeoutMs;
  };
  bool trayAvailableValue = true;
  bool notificationsAvailableValue = true;
  bool trayVisible = false;
  QString lastToolTip;
  QList<ShownNotification> notifications;
  std::function<void()> openHandler;
  std::function<void()> quitHandler;
  std::function<void()> notificationClickHandler;
};

DesktopNotifications::IncomingMessage
incoming(const QString &id = "message-1",
         const QString &body = "private text") {
  return {QStringLiteral("chat-1"),
          id,
          QStringLiteral("Synthetic contact"),
          body,
          false,
          false,
          false,
          false};
}
} // namespace

class DesktopNotificationsTest final : public QObject {
  Q_OBJECT
private slots:
  void defaultsAreOptInAndDoNotPersistMessagePreviews();
  void optedInNotificationOmitsBodyUntilPreviewsAreEnabled();
  void previewIsPlainTextAndBoundedByUnicodeCharacters();
  void suppressesOutgoingDemoHistoricalAndActiveVisibleMessages();
  void unclassifiedMessageIsSuppressedByDefault();
  void deduplicatesByChatAndMessageAndEvictsOldEntries();
  void unavailableTrayAndNotificationServiceAreReportedTruthfully();
  void preferencesPersistWithoutPersistingMessageContent();
  void trayActionsAreForwardedToTheOwner();
  void productionDeliveryReflectsQtTrayAvailability();
};

void DesktopNotificationsTest::
    defaultsAreOptInAndDoNotPersistMessagePreviews() {
  QTemporaryDir dir;
  QVERIFY(dir.isValid());
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));

  QVERIFY(!notifications.notificationsEnabled());
  QVERIFY(!notifications.messagePreviewsEnabled());
  QVERIFY(notifications.trayAvailable());
  QVERIFY(notifications.notificationsAvailable());
  QVERIFY(delivery->trayVisible);
  QCOMPARE(delivery->lastToolTip, QStringLiteral("WhatsFeather"));
  QVERIFY(!notifications.notifyIncomingMessage(incoming()));
  QVERIFY(delivery->notifications.isEmpty());
  QVERIFY(!settings.contains(QStringLiteral("notifications/enabled")));
  QVERIFY(!settings.contains(QStringLiteral("notifications/messagePreviews")));
}

void DesktopNotificationsTest::
    optedInNotificationOmitsBodyUntilPreviewsAreEnabled() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);

  QVERIFY(notifications.notifyIncomingMessage(incoming()));
  QCOMPARE(delivery->notifications.size(), 1);
  QCOMPARE(delivery->notifications.last().title,
           QStringLiteral("Synthetic contact"));
  QCOMPARE(delivery->notifications.last().body, QStringLiteral("New message"));

  auto preview = incoming(QStringLiteral("message-2"),
                          QStringLiteral("Text only after opt-in"));
  notifications.setMessagePreviewsEnabled(true);
  QVERIFY(notifications.notifyIncomingMessage(preview));
  QCOMPARE(delivery->notifications.size(), 2);
  QCOMPARE(delivery->notifications.last().body,
           QStringLiteral("Text only after opt-in"));
}

void DesktopNotificationsTest::
    previewIsPlainTextAndBoundedByUnicodeCharacters() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);
  notifications.setMessagePreviewsEnabled(true);
  auto event = incoming(QStringLiteral("safe-preview"),
                        QStringLiteral("line one\nline two\u0001 ") +
                            QString(300, QLatin1Char('x')));

  QVERIFY(notifications.notifyIncomingMessage(event));
  const auto shown = delivery->notifications.last().body;
  QVERIFY(!shown.contains(QChar::LineFeed));
  QVERIFY(!shown.contains(QChar(1)));
  QVERIFY(shown.startsWith(QStringLiteral("line one line two ")));
  QVERIFY(shown.toUcs4().size() <=
          DesktopNotifications::MaximumPreviewCharacters);
  QVERIFY(shown.count(QLatin1Char('x')) < 300);
}

void DesktopNotificationsTest::
    suppressesOutgoingDemoHistoricalAndActiveVisibleMessages() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);

  auto event = incoming(QStringLiteral("outgoing"));
  event.outgoing = true;
  QVERIFY(!notifications.notifyIncomingMessage(event));
  event = incoming(QStringLiteral("demo"));
  event.demo = true;
  QVERIFY(!notifications.notifyIncomingMessage(event));
  event = incoming(QStringLiteral("historical"));
  event.historical = true;
  QVERIFY(!notifications.notifyIncomingMessage(event));
  event = incoming(QStringLiteral("visible"));
  event.activeVisibleChat = true;
  QVERIFY(!notifications.notifyIncomingMessage(event));
  QVERIFY(delivery->notifications.isEmpty());

  QVERIFY(notifications.notifyIncomingMessage(
      incoming(QStringLiteral("eligible"))));
  QCOMPARE(delivery->notifications.size(), 1);
}

void DesktopNotificationsTest::unclassifiedMessageIsSuppressedByDefault() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);

  DesktopNotifications::IncomingMessage unclassified;
  unclassified.chatId = QStringLiteral("chat-1");
  unclassified.messageId = QStringLiteral("unknown-classification");
  unclassified.chatTitle = QStringLiteral("Synthetic contact");
  unclassified.body = QStringLiteral("must not leak");
  QVERIFY(!notifications.notifyIncomingMessage(unclassified));
  QVERIFY(delivery->notifications.isEmpty());
}

void DesktopNotificationsTest::
    deduplicatesByChatAndMessageAndEvictsOldEntries() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);

  QVERIFY(
      notifications.notifyIncomingMessage(incoming(QStringLiteral("same-id"))));
  QVERIFY(!notifications.notifyIncomingMessage(
      incoming(QStringLiteral("same-id"))));
  auto otherChat = incoming(QStringLiteral("same-id"));
  otherChat.chatId = QStringLiteral("chat-2");
  QVERIFY(notifications.notifyIncomingMessage(otherChat));
  QCOMPARE(delivery->notifications.size(), 2);

  for (int i = 0; i < DesktopNotifications::MaximumRememberedMessages; ++i)
    QVERIFY(notifications.notifyIncomingMessage(
        incoming(QStringLiteral("bounded-%1").arg(i))));
  QCOMPARE(delivery->notifications.size(),
           2 + DesktopNotifications::MaximumRememberedMessages);
  QVERIFY(
      notifications.notifyIncomingMessage(incoming(QStringLiteral("same-id"))));
  QCOMPARE(delivery->notifications.size(),
           3 + DesktopNotifications::MaximumRememberedMessages);
}

void DesktopNotificationsTest::
    unavailableTrayAndNotificationServiceAreReportedTruthfully() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  delivery->trayAvailableValue = false;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  notifications.setNotificationsEnabled(true);

  QVERIFY(!notifications.trayAvailable());
  QVERIFY(!notifications.notificationsAvailable());
  QVERIFY(!delivery->trayVisible);
  QVERIFY(!notifications.notifyIncomingMessage(incoming()));
  QVERIFY(delivery->notifications.isEmpty());

  delivery->trayAvailableValue = true;
  delivery->notificationsAvailableValue = false;
  notifications.refreshAvailability();
  QVERIFY(notifications.trayAvailable());
  QVERIFY(!notifications.notificationsAvailable());
  QVERIFY(delivery->trayVisible);
  QVERIFY(!notifications.notifyIncomingMessage(
      incoming(QStringLiteral("unsupported"))));
  QVERIFY(delivery->notifications.isEmpty());

  delivery->notificationsAvailableValue = true;
  notifications.refreshAvailability();
  QVERIFY(notifications.notificationsAvailable());
  QVERIFY(notifications.notifyIncomingMessage(
      incoming(QStringLiteral("available"))));
}

void DesktopNotificationsTest::
    preferencesPersistWithoutPersistingMessageContent() {
  QTemporaryDir dir;
  const auto settingsPath = dir.filePath("prefs.ini");
  QString persistedBody;
  {
    QSettings settings(settingsPath, QSettings::IniFormat);
    auto *delivery = new FakeNotificationDelivery;
    DesktopNotifications notifications(
        &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
    notifications.setNotificationsEnabled(true);
    notifications.setMessagePreviewsEnabled(true);
    persistedBody = QStringLiteral("must never be saved");
    QVERIFY(notifications.notifyIncomingMessage(
        incoming(QStringLiteral("private-id"), persistedBody)));
  }
  QSettings settings(settingsPath, QSettings::IniFormat);
  QCOMPARE(settings.value(QStringLiteral("notifications/enabled")).toBool(),
           true);
  QCOMPARE(
      settings.value(QStringLiteral("notifications/messagePreviews")).toBool(),
      true);
  const auto allKeys = settings.allKeys();
  for (const auto &key : allKeys)
    QVERIFY(!settings.value(key).toString().contains(persistedBody));
}

void DesktopNotificationsTest::trayActionsAreForwardedToTheOwner() {
  QTemporaryDir dir;
  QSettings settings(dir.filePath("prefs.ini"), QSettings::IniFormat);
  auto *delivery = new FakeNotificationDelivery;
  DesktopNotifications notifications(
      &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
  QSignalSpy openSpy(&notifications,
                     &DesktopNotifications::showWindowRequested);
  QSignalSpy quitSpy(&notifications, &DesktopNotifications::quitRequested);
  QVERIFY(openSpy.isValid());
  QVERIFY(quitSpy.isValid());

  QVERIFY(static_cast<bool>(delivery->openHandler));
  QVERIFY(static_cast<bool>(delivery->quitHandler));
  QVERIFY(static_cast<bool>(delivery->notificationClickHandler));
  delivery->openHandler();
  delivery->quitHandler();
  delivery->notificationClickHandler();
  QCOMPARE(openSpy.size(), 2);
  QCOMPARE(quitSpy.size(), 1);
}

void DesktopNotificationsTest::productionDeliveryReflectsQtTrayAvailability() {
  QTemporaryDir dir;
  QVERIFY(dir.isValid());
  QCoreApplication::setOrganizationName(
      QStringLiteral("WhatsFeatherSyntheticTests"));
  QCoreApplication::setApplicationName(
      QStringLiteral("DesktopNotificationsTest"));
  QSettings::setDefaultFormat(QSettings::IniFormat);
  QSettings::setPath(QSettings::IniFormat, QSettings::UserScope, dir.path());

  DesktopNotifications notifications;
  const bool expectedTray = QSystemTrayIcon::isSystemTrayAvailable();
  QCOMPARE(notifications.trayAvailable(), expectedTray);
  QCOMPARE(notifications.notificationsAvailable(),
           expectedTray && QSystemTrayIcon::supportsMessages());
  if (!expectedTray)
    QCOMPARE(notifications.trayStatus(),
             QStringLiteral("System tray unavailable"));
  QVERIFY(!notifications.notifyIncomingMessage(
      incoming(QStringLiteral("production"))));
}

QTEST_MAIN(DesktopNotificationsTest)
#include "test_desktopnotifications.moc"
