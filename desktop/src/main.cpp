#include "controller.h"
#include "desktopnotifications.h"
#include <QApplication>
#include "communityfoldermodel.h"
#include "legaldocuments.h"
#include <QCommandLineParser>
#include <QDebug>
#include <QDir>
#include <QFileInfo>
#include <QFont>
#include <QGuiApplication>
#include <QImage>
#include <QIcon>
#include <QQmlApplicationEngine>
#include <QQmlContext>
#include <QQuickWindow>
#include <QSettings>
#include <QStandardPaths>
#include <QTimer>
void runCommunityCacheTest(QGuiApplication &, QQuickWindow *, Controller &, CommunityFolderModel &);
void runCommunityTest(QGuiApplication &, QQuickWindow *, Controller &, CommunityFolderModel &, const QString &);
void runPolishTest(QGuiApplication &, QQuickWindow *, Controller &,
                   const QString &, const QString &, const QStringList &);
void runProductTest(QGuiApplication &, QQuickWindow *, Controller &,
                    const QString &, const QString &);
void runSettingsTest(QGuiApplication &, QQuickWindow *, Controller &,
                     const QString &);
void runAboutLegalTest(QGuiApplication &, QQuickWindow *,
                       const LegalDocuments &);
QObject *startMediaDownloadFixture(QGuiApplication &, Controller &);
void runMediaDownloadTest(QGuiApplication &, QQuickWindow *, Controller &,
                          QObject *, const QString &, const QString &);
QObject *startChatRosterFixture(QGuiApplication &, Controller &,
                                const QString &);
void runChatRosterTest(QGuiApplication &, QQuickWindow *, Controller &,
                       QObject *);
QObject *startContactCardFixture(QGuiApplication &, Controller &,
                                 const QString &);
void runContactCardTest(QGuiApplication &, QQuickWindow *, Controller &,
                        QObject *);
QObject *startLocationFixture(QGuiApplication &, Controller &, const QString &);
void runLocationTest(QGuiApplication &, QQuickWindow *, Controller &, QObject *);
QObject *startPollFixture(QGuiApplication &, Controller &, const QString &);
void runPollTest(QGuiApplication &, QQuickWindow *, Controller &, QObject *);
void runPollDaemonTest(QGuiApplication &, QQuickWindow *, Controller &);
QObject *startListMessageFixture(QGuiApplication &, Controller &, const QString &);
void runListMessageTest(QGuiApplication &, QQuickWindow *, Controller &, QObject *);
QObject *startTextCardFixture(QGuiApplication &, Controller &, const QString &);
void runTextCardTest(QGuiApplication &, QQuickWindow *, Controller &, QObject *);
QObject *startButtonsFixture(QGuiApplication &, Controller &, const QString &);
void runButtonsTest(QGuiApplication &, QQuickWindow *, Controller &, QObject *);
int main(int argc, char **argv) {
  QApplication app(argc, argv);
  app.setOrganizationName("WhatsFeather");
  app.setApplicationName("WhatsFeather");
  app.setDesktopFileName(QStringLiteral("whatsfeather"));
  QIcon applicationIcon;
  for (const int size : {16, 24, 32, 48, 64, 128, 256, 512})
    applicationIcon.addFile(QStringLiteral(":/app-icons/whatsfeather-%1.png").arg(size), QSize(size, size));
  app.setWindowIcon(applicationIcon);
  QCommandLineParser parser;
  parser.setApplicationDescription(
      "Native WhatsFeather first slice — demo by default");
  parser.addHelpOption();
  parser.addOptions(
      {{"demo", "Synthetic local demo (default)"},
       {"live", "Experimental live mode, never auto-connect"},
       {"live-test-scope", "Private bounded scope file for live-test mode", "path"},
       {"socket", "Use an external backend socket", "path"},
       {"data-dir", "Profile root (demo/live use separate defaults)", "path"},
       {"theme", "light or dark", "theme"},
       {"screenshot", "Save synthetic demo screenshot", "path"},
       {"quit-after", "Exit after milliseconds", "ms"},
       {"ui-test", "Run native demo interaction smoke test"},
       {"draft-close-test", "Exercise immediate draft-save on native close"},
       {"polish-test", "Exercise native reference-polish controls in demo"},
       {"polish-fixture", "Synthetic GIF fixture for polish test", "path"},
       {"polish-sticker-fixture", "Synthetic static WebP sticker fixture for polish test", "path"},
       {"polish-album-fixture", "Synthetic still-image fixture for album polish test", "path"},
       {"product-test", "Exercise native product completion in demo"},
       {"product-fixture", "Synthetic local media fixture for product test", "path"},
       {"settings-test", "Verify synthetic Settings theme persistence and connection controls"},
       {"community-test", "Exercise synthetic native community folders"},
       {"community-cache-test", "Verify demo daemon community cache reaches native UI"},
       {"media-download-test", "Exercise synthetic incoming-media actions with keyboard input"},
       {"chat-roster-test", "Exercise synthetic New chat roster selection in the native UI"},
       {"contact-card-test", "Exercise private synthetic native contact-card review and send"},
       {"location-test", "Exercise private synthetic native location snapshot rendering"},
       {"poll-test", "Exercise private synthetic native poll creation rendering"},
       {"poll-daemon-test", "Exercise read-only synthetic poll results through the real demo daemon"},
       {"list-message-test", "Exercise private synthetic inert list rendering"},
       {"text-card-test", "Exercise private synthetic inert text cards"},
       {"buttons-message-test", "Exercise private synthetic native inert ButtonsMessage rendering"},
       {"about-legal-test", "Exercise offline legal documents in the synthetic native About UI"},
       {"icon-test", "Verify packaged application icon and desktop identity"}});
  parser.process(app);
  if (parser.isSet("demo") && parser.isSet("live")) {
    qCritical() << "Conflicting modes";
    return 2;
  }
  if (parser.isSet("live-test-scope") &&
      (!parser.isSet("live") || parser.value("live-test-scope").isEmpty() ||
       parser.isSet("socket") || !parser.isSet("data-dir") ||
       parser.value("data-dir").isEmpty())) {
    qCritical() << "Live-test scope requires explicit --live, --data-dir, and the managed local backend";
    return 2;
  }
  if (parser.isSet("about-legal-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "about-legal-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("live") &&
      (parser.isSet("screenshot") || parser.isSet("ui-test") ||
       parser.isSet("draft-close-test") || parser.isSet("polish-test") ||
       parser.isSet("polish-fixture") || parser.isSet("polish-sticker-fixture") ||
       parser.isSet("polish-album-fixture") || parser.isSet("product-test") || parser.isSet("product-fixture") || parser.isSet("icon-test") ||
       parser.isSet("settings-test") ||
       parser.isSet("community-test") || parser.isSet("community-cache-test") ||
       parser.isSet("media-download-test") || parser.isSet("chat-roster-test") ||
       parser.isSet("contact-card-test") || parser.isSet("location-test") ||
       parser.isSet("poll-test") || parser.isSet("poll-daemon-test") ||
       parser.isSet("list-message-test") ||
       parser.isSet("buttons-message-test") || parser.isSet("text-card-test"))) {
    qCritical() << "Capture/test flags are demo-only";
    return 2;
  }
  if (parser.isSet("polish-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "polish-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("polish-album-fixture") &&
      (!parser.isSet("polish-test") || !parser.isSet("demo") ||
       parser.isSet("socket") || parser.values("polish-album-fixture").size() < 2 ||
       parser.values("polish-album-fixture").size() > 10)) {
    qCritical() << "polish-album-fixture requires --polish-test, --demo, no --socket, and 2 to 10 fixtures";
    return 2;
  }
  if (parser.isSet("product-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "product-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("product-fixture") &&
      (!parser.isSet("product-test") || !parser.isSet("demo") ||
       parser.isSet("socket"))) {
    qCritical() << "product-fixture requires --product-test, --demo, and no --socket";
    return 2;
  }
  if (parser.isSet("media-download-test") &&
      (!parser.isSet("demo") || parser.isSet("socket") ||
       qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_STATE").isEmpty() ||
       qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_COMMAND").isEmpty())) {
    qCritical() << "media-download-test requires --demo, no --socket, and test control files";
    return 2;
  }
  if (parser.isSet("chat-roster-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "chat-roster-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("contact-card-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "contact-card-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("location-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "location-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("poll-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "poll-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("poll-daemon-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "poll-daemon-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("list-message-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "list-message-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("text-card-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "text-card-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("settings-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "settings-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("buttons-message-test") &&
      (!parser.isSet("demo") || parser.isSet("socket"))) {
    qCritical() << "buttons-message-test requires --demo and rejects --socket";
    return 2;
  }
  if (parser.isSet("theme") && parser.value("theme") != "dark" &&
      parser.value("theme") != "light") {
    qCritical() << "Theme must be light or dark";
    return 2;
  }
  if (parser.isSet("icon-test")) {
    const auto sizes = app.windowIcon().availableSizes();
    for (const int size : {16, 24, 32, 48, 64, 128, 256, 512}) {
      if (!sizes.contains(QSize(size, size))) {
        qCritical() << "Missing packaged application icon size" << size;
        return 1;
      }
    }
    const QImage image(QStringLiteral(":/app-icons/whatsfeather-512.png"));
    if (image.isNull() || !image.hasAlphaChannel() || image.pixelColor(0, 0).alpha() != 0 ||
        app.desktopFileName() != QStringLiteral("whatsfeather")) {
      qCritical() << "Invalid application icon transparency or desktop identity";
      return 1;
    }
    qInfo() << "Native app icon PASS: eight packaged sizes, transparent corners, desktop identity";
    return 0;
  }
  bool live = parser.isSet("live");
  QSettings prefs;
  bool dark = parser.isSet("theme") ? parser.value("theme") == "dark"
                                    : prefs.value("dark", true).toBool();
  Controller controller;
  DesktopNotifications desktopNotifications;
  auto updateNotificationSubscription = [&] {
    controller.setNotificationSubscription(desktopNotifications.notificationsEnabled(),
                                           desktopNotifications.messagePreviewsEnabled());
  };
  QObject::connect(&desktopNotifications, &DesktopNotifications::preferencesChanged,
                   &controller, updateNotificationSubscription);
  updateNotificationSubscription();
  CommunityFolderModel communityFolders;
  communityFolders.setSourceModel(controller.chats());
  LegalDocuments legalDocuments;
  QQmlApplicationEngine engine;
  engine.rootContext()->setContextProperty("communityFolders", &communityFolders);
  engine.rootContext()->setContextProperty("bridge", &controller);
  engine.rootContext()->setContextProperty("desktopNotifications", &desktopNotifications);
  engine.rootContext()->setContextProperty("legalDocuments", &legalDocuments);
  engine.rootContext()->setContextProperty("initialDark", dark);
  engine.rootContext()->setContextProperty("systemFont",
                                           QGuiApplication::font().family());
  QObject::connect(
      &engine, &QQmlApplicationEngine::objectCreationFailed, &app,
      [] { QCoreApplication::exit(1); }, Qt::QueuedConnection);
  engine.loadFromModule("WhatsFeather", "Main");
  if (engine.rootObjects().isEmpty())
    return 1;
  auto *window = qobject_cast<QQuickWindow *>(engine.rootObjects().first());
  QObject::connect(&desktopNotifications,
                   &DesktopNotifications::showWindowRequested, window,
                   [window] {
                     window->show();
                     window->raise();
                     window->requestActivate();
                   });
  QObject::connect(&desktopNotifications, &DesktopNotifications::quitRequested,
                   &controller, &Controller::requestQuit);
  QObject::connect(
      &controller, &Controller::incomingNotification, &desktopNotifications,
      [window, &controller, &desktopNotifications](const QVariantMap &event) {
        const auto chat = event.value("chat_id").toString();
        const bool activeVisibleChat = window->isVisible() &&
                                       window->isActive() &&
                                       controller.selectedId() == chat;
        desktopNotifications.notifyLiveInboundEvent(event, controller.demo(),
                                                    activeVisibleChat);
      });
  QString dir = parser.value("data-dir");
  if (dir.isEmpty())
    dir =
        QStandardPaths::writableLocation(QStandardPaths::GenericDataLocation) +
        "/whatsfeather/" + (live ? "live" : "demo");
  dir = QDir(dir).absolutePath();
  communityFolders.setProfileKey(dir);
  QObject *mediaFixture = nullptr;
  QObject *chatRosterFixture = nullptr;
  QObject *contactCardFixture = nullptr;
  QObject *locationFixture = nullptr;
  QObject *pollFixture = nullptr;
  QObject *listMessageFixture = nullptr;
  QObject *buttonsFixture = nullptr;
  QObject *textCardFixture = nullptr;
  if (parser.isSet("chat-roster-test")) {
    chatRosterFixture = startChatRosterFixture(app, controller, dir);
    if (!chatRosterFixture)
      return 1;
  } else if (parser.isSet("contact-card-test")) {
    contactCardFixture = startContactCardFixture(app, controller, dir);
    if (!contactCardFixture)
      return 1;
  } else if (parser.isSet("location-test")) {
    locationFixture = startLocationFixture(app, controller, dir);
    if (!locationFixture)
      return 1;
  } else if (parser.isSet("poll-test")) {
    pollFixture = startPollFixture(app, controller, dir);
    if (!pollFixture)
      return 1;
  } else if (parser.isSet("list-message-test")) {
    listMessageFixture = startListMessageFixture(app, controller, dir);
    if (!listMessageFixture)
      return 1;
  } else if (parser.isSet("text-card-test")) {
    textCardFixture = startTextCardFixture(app, controller, dir);
    if (!textCardFixture) return 1;
  } else if (parser.isSet("buttons-message-test")) {
    buttonsFixture = startButtonsFixture(app, controller, dir);
    if (!buttonsFixture)
      return 1;
  } else if (parser.isSet("media-download-test")) {
    mediaFixture = startMediaDownloadFixture(app, controller);
    if (!mediaFixture)
      return 1;
  } else {
    controller.start(parser.value("socket"), dir, live,
                     parser.value("live-test-scope"));
  }
  if (parser.isSet("chat-roster-test"))
    runChatRosterTest(app, window, controller, chatRosterFixture);
  if (parser.isSet("contact-card-test"))
    runContactCardTest(app, window, controller, contactCardFixture);
  if (parser.isSet("location-test"))
    runLocationTest(app, window, controller, locationFixture);
  if (parser.isSet("poll-test"))
    runPollTest(app, window, controller, pollFixture);
  if (parser.isSet("poll-daemon-test"))
    runPollDaemonTest(app, window, controller);
  if (parser.isSet("list-message-test"))
    runListMessageTest(app, window, controller, listMessageFixture);
  if (parser.isSet("text-card-test"))
    runTextCardTest(app, window, controller, textCardFixture);
  if (parser.isSet("buttons-message-test"))
    runButtonsTest(app, window, controller, buttonsFixture);
  if (parser.isSet("community-test"))
    runCommunityTest(app, window, controller, communityFolders, qEnvironmentVariable("WHATSFEATHER_QA_CAPTURE_DIR"));
  if (parser.isSet("community-cache-test"))
    runCommunityCacheTest(app, window, controller, communityFolders);
  QTimer captureSelection;
  if (parser.isSet("screenshot") || parser.isSet("ui-test") ||
      parser.isSet("draft-close-test") || parser.isSet("polish-test") ||
      parser.isSet("product-test") || parser.isSet("media-download-test") ||
      parser.isSet("contact-card-test") || parser.isSet("location-test") ||
      parser.isSet("poll-test") || parser.isSet("poll-daemon-test") ||
      parser.isSet("list-message-test") ||
       parser.isSet("buttons-message-test") || parser.isSet("text-card-test")) {
    captureSelection.setInterval(50);
    QObject::connect(&captureSelection, &QTimer::timeout, &app, [&] {
      if (controller.chats()->rowCount() > 0 &&
          controller.selectedId().isEmpty()) {
        if (!controller.demo()) {
          qCritical() << "Refusing automated interactions with a live profile";
          app.exit(2);
          return;
        }
        controller.selectChat(
            controller.chats()->row(0).value("id").toString());
        captureSelection.stop();
      }
    });
    captureSelection.start();
  }
  if (parser.isSet("media-download-test"))
    runMediaDownloadTest(app, window, controller, mediaFixture,
                        qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_STATE"),
                        qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_COMMAND"));
  if (parser.isSet("polish-test"))
    runPolishTest(app, window, controller, parser.value("polish-fixture"),
                  parser.value("polish-sticker-fixture"),
                  parser.values("polish-album-fixture"));
  if (parser.isSet("product-test")) {
    auto captureDir = qEnvironmentVariable("WHATSFEATHER_QA_CAPTURE_DIR");
    if (captureDir.isEmpty())
      captureDir = QDir::currentPath() + "/.hermes/product-finish/ui-qa/" +
                   (dark ? "dark" : "light");
    runProductTest(app, window, controller, parser.value("product-fixture"),
                   captureDir);
  }
  if (parser.isSet("settings-test")) {
    auto captureDir = qEnvironmentVariable("WHATSFEATHER_QA_CAPTURE_DIR");
    if (captureDir.isEmpty())
      captureDir = QDir::currentPath() +
                   "/.hermes/parity-implementation/settings-theme-connection/qa-captures";
    runSettingsTest(app, window, controller, captureDir);
  }
  if (parser.isSet("about-legal-test"))
    runAboutLegalTest(app, window, legalDocuments);
  QTimer closeTest;
  if (parser.isSet("draft-close-test")) {
    closeTest.setInterval(50);
    QObject::connect(&closeTest, &QTimer::timeout, &app, [&] {
      if (controller.messageCount() > 0) {
        closeTest.stop();
        controller.setDraft("last keystroke before closing");
        window->close();
      }
    });
    closeTest.start();
    QTimer::singleShot(10000, &app, [] { QCoreApplication::exit(1); });
  }
  QTimer test;
  int stage = 0;
  int ticks = 0;
  QString testText = "Native UI integration message";
  if (parser.isSet("ui-test")) {
    test.setInterval(50);
    QObject::connect(&test, &QTimer::timeout, &app, [&] {
      if (++ticks > 240) {
        qCritical() << "UI smoke timed out at stage" << stage;
        app.exit(1);
        return;
      }
      if (stage == 0 && controller.messageCount() > 0) {
        auto *composer = window->findChild<QObject *>("composer");
        auto *button = window->findChild<QObject *>("sendButton");
        if (!composer || !button) {
          qCritical() << "Missing native controls";
          app.exit(1);
          return;
        }
        composer->setProperty("text", testText);
        QMetaObject::invokeMethod(button, "clicked");
        stage = 1;
      } else if (stage == 1 && !controller.sending()) {
        bool found = false;
        for (int i = 0; i < controller.messageCount(); ++i) {
          auto m = controller.messages()->row(i);
          if (m.value("body") == testText && m.value("status") == "demo_sent")
            found = true;
        }
        if (found && controller.draft().isEmpty()) {
          controller.loadOlder();
          stage = 2;
        }
      } else if (stage == 2 && !controller.loading() &&
                 controller.historyWindow()) {
        if (controller.messageCount() > 500) {
          app.exit(1);
          return;
        }
        window->setProperty("dark", !dark);
        window->setProperty("dark", dark);
        qInfo() << "Native UI smoke PASS: real backend, selection, composer "
                   "send, draft clear, earlier page, theme";
        test.stop();
        if (!parser.isSet("screenshot"))
          app.quit();
      }
    });
    test.start();
  }
  if (parser.isSet("screenshot"))
    QTimer::singleShot(parser.isSet("ui-test") ? 3500 : 2200, &app, [&] {
      if (!controller.demo()) {
        qCritical() << "Refusing non-demo capture";
        app.exit(1);
        return;
      }
      QDir().mkpath(QFileInfo(parser.value("screenshot")).absolutePath());
      if (!window->grabWindow().save(parser.value("screenshot"))) {
        qCritical() << "Screenshot failed";
        app.exit(1);
      } else
        qInfo() << "Saved native demo screenshot";
    });
  int quit = parser.value("quit-after").toInt();
  if (quit > 0)
    QTimer::singleShot(quit, &app, &QCoreApplication::quit);
  return app.exec();
}
