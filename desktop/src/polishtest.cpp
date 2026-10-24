// Native demo-only acceptance probe. Never runs against a real account.
#include "controller.h"
#include <QClipboard>
#include <QAccessible>
#include <QDebug>
#include <QDir>
#include <QFileInfo>
#include <QGuiApplication>
#include <QFontMetricsF>
#include <QImage>
#include <QJSValue>
#include <QQuickItem>
#include <QImageReader>
#include <QJsonDocument>
#include <QMouseEvent>
#include <QKeyEvent>
#include <QQuickWindow>
#include <QSettings>
#include <QStringList>
#include <QTimer>
#include <QUrl>
#include <QVideoFrame>
#include <QVideoSink>
#include <memory>

namespace {
bool renderedSmpteBars(const QImage &screenshot, QQuickWindow *window,
                       QQuickItem *video, const QSize &frameSize,
                       QImage *videoCrop, QString *diagnostic) {
  if (screenshot.isNull() || !window || !video || frameSize.isEmpty() ||
      window->width() <= 0 || window->height() <= 0 || video->width() <= 0 ||
      video->height() <= 0) {
    if (diagnostic)
      *diagnostic = QStringLiteral("rendered screenshot/video geometry is unavailable");
    return false;
  }

  const qreal pixelScaleX = qreal(screenshot.width()) / window->width();
  const qreal pixelScaleY = qreal(screenshot.height()) / window->height();
  const qreal contentScale =
      qMin(video->width() / frameSize.width(),
           video->height() / frameSize.height());
  const qreal contentWidth = frameSize.width() * contentScale;
  const qreal contentHeight = frameSize.height() * contentScale;
  const QPointF videoOrigin = video->mapToScene(QPointF(0, 0));
  const qreal contentLeft =
      videoOrigin.x() + (video->width() - contentWidth) / 2;
  const qreal contentTop =
      videoOrigin.y() + (video->height() - contentHeight) / 2;
  const QRect contentPixels(
      qFloor(contentLeft * pixelScaleX), qFloor(contentTop * pixelScaleY),
      qCeil(contentWidth * pixelScaleX), qCeil(contentHeight * pixelScaleY));
  if (videoCrop)
    *videoCrop = screenshot.copy(contentPixels.intersected(screenshot.rect()));

  static const QStringList barNames{
      QStringLiteral("white"), QStringLiteral("yellow"),
      QStringLiteral("cyan"), QStringLiteral("green"),
      QStringLiteral("magenta"), QStringLiteral("red"),
      QStringLiteral("blue")};
  const auto matchesBar = [](int index, const QColor &pixel) {
    switch (index) {
    case 0: return pixel.red() > 175 && pixel.green() > 175 && pixel.blue() > 175;
    case 1: return pixel.red() > 165 && pixel.green() > 145 && pixel.blue() < 115;
    case 2: return pixel.red() < 115 && pixel.green() > 155 && pixel.blue() > 155;
    case 3: return pixel.red() < 115 && pixel.green() > 135 && pixel.blue() < 115;
    case 4: return pixel.red() > 155 && pixel.green() < 115 && pixel.blue() > 155;
    case 5: return pixel.red() > 165 && pixel.green() < 115 && pixel.blue() < 115;
    case 6: return pixel.red() < 115 && pixel.green() < 115 && pixel.blue() > 155;
    default: return false;
    }
  };

  QStringList samples;
  bool matches = true;
  const qreal sampleY = (contentTop + contentHeight * 0.25) * pixelScaleY;
  for (int index = 0; index < barNames.size(); ++index) {
    const qreal sampleX =
        (contentLeft + contentWidth * (index + 0.5) / barNames.size()) *
        pixelScaleX;
    const QPoint point(qBound(0, qRound(sampleX), screenshot.width() - 1),
                       qBound(0, qRound(sampleY), screenshot.height() - 1));
    const QColor pixel = screenshot.pixelColor(point);
    const bool matched = matchesBar(index, pixel);
    matches = matches && matched;
    samples.append(QStringLiteral("%1=%2,%3,%4%5")
                       .arg(barNames[index])
                       .arg(pixel.red()).arg(pixel.green()).arg(pixel.blue())
                       .arg(matched ? QString() : QStringLiteral(" (mismatch)")));
  }
  if (diagnostic)
    *diagnostic = samples.join(QStringLiteral("; "));
  return matches;
}

QObject *named(QQuickWindow *w, const QString &name) {
  if (auto *object = w->findChild<QObject *>(name))
    return object;
  std::function<QQuickItem *(QQuickItem *)> visit =
      [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == name)
      return item;
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  return visit(w->contentItem());
}
QQuickItem *namedBelow(QQuickItem *root, const QString &name) {
  if (!root)
    return nullptr;
  if (root->objectName() == name)
    return root;
  for (auto *child : root->childItems())
    if (auto *found = namedBelow(child, name))
      return found;
  return nullptr;
}
bool dragSelectText(QQuickWindow *window, QQuickItem *item) {
  if (!window || !item || item->width() < 8 || item->height() < 8)
    return false;
  const auto y = item->height() / 2.0;
  const auto start = item->mapToScene({1.0, y});
  const auto end = item->mapToScene({qMin(item->width() - 1.0, 80.0), y});
  const auto globalStart = QPointF(window->mapToGlobal(start.toPoint()));
  const auto globalEnd = QPointF(window->mapToGlobal(end.toPoint()));
  QMouseEvent press(QEvent::MouseButtonPress, start, start, globalStart,
                    Qt::LeftButton, Qt::LeftButton, Qt::NoModifier);
  QGuiApplication::sendEvent(window, &press);
  QMouseEvent move(QEvent::MouseMove, end, end, globalEnd, Qt::NoButton,
                   Qt::LeftButton, Qt::NoModifier);
  QGuiApplication::sendEvent(window, &move);
  QMouseEvent release(QEvent::MouseButtonRelease, end, end, globalEnd,
                      Qt::LeftButton, Qt::NoButton, Qt::NoModifier);
  QGuiApplication::sendEvent(window, &release);
  return true;
}
QVariantMap itemRecord(QObject *item) {
  if (!item)
    return {};
  const auto value = item->property("record");
  return value.canConvert<QJSValue>()
             ? value.value<QJSValue>().toVariant().toMap()
             : value.toMap();
}
QQuickItem *chatRowForId(QQuickWindow *window, const QString &id) {
  if (!window || id.isEmpty())
    return nullptr;
  std::function<QQuickItem *(QQuickItem *)> visit = [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == "chatRow" || item->objectName() == "communityChildRow") {
      if (itemRecord(item).value("id").toString() == id)
        return item;
    }
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  return visit(window->contentItem());
}
QQuickItem *forwardChatRowForId(QQuickWindow *window, const QString &id) {
  if (!window || id.isEmpty())
    return nullptr;
  std::function<QQuickItem *(QQuickItem *)> visit = [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == "forwardChatRow" &&
        itemRecord(item).value("id").toString() == id)
      return item;
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  return visit(window->contentItem());
}
QQuickItem *messageBubble(QQuickWindow *window, const QString &id) {
  std::function<QQuickItem *(QQuickItem *)> visit = [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == "messageBubble") {
      const auto value = item->property("record");
      const auto record = value.canConvert<QJSValue>()
                              ? value.value<QJSValue>().toVariant().toMap()
                              : value.toMap();
      if (record.value("id").toString() == id)
        return item;
    }
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  return visit(window->contentItem());
}
bool invoke(QQuickWindow *w, const QString &name, const char *signal) {
  auto *object = named(w, name);
  return object && QMetaObject::invokeMethod(object, signal);
}
} // namespace

void runSettingsTest(QGuiApplication &app, QQuickWindow *window, Controller &c,
                     const QString &captureDir) {
  struct State {
    int stage = 0;
    int ticks = 0;
  };
  const auto expectedTheme =
      qEnvironmentVariable("WHATSFEATHER_SETTINGS_TEST_EXPECT_THEME");
  const auto setTheme =
      qEnvironmentVariable("WHATSFEATHER_SETTINGS_TEST_SET_THEME");
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(
      timer, &QTimer::timeout, &app,
      [&app, window, &c, state, timer, captureDir, expectedTheme, setTheme] {
        auto fail = [&](const QString &message) {
          qCritical() << "Native settings FAIL at stage" << state->stage
                      << message;
          timer->stop();
          app.exit(1);
        };
        if (++state->ticks > 200) {
          fail("timed out waiting for rendered Settings controls");
          return;
        }
        if (state->stage == 0) {
          if (!window || !c.demo()) {
            fail("fixture must run with the synthetic demo backend");
            return;
          }
          if ((expectedTheme != "dark" && expectedTheme != "light") ||
              window->property("dark").toBool() !=
                  (expectedTheme == "dark")) {
            fail("startup theme did not match the isolated Settings fixture");
            return;
          }
          if (!invoke(window, "settingsButton", "clicked")) {
            fail("Settings button was not available");
            return;
          }
          state->stage = 1;
          return;
        }
        if (state->stage == 2) {
          const bool expectedDark = setTheme == "dark";
          auto *light =
              qobject_cast<QQuickItem *>(named(window, "settingsThemeLight"));
          auto *dark =
              qobject_cast<QQuickItem *>(named(window, "settingsThemeDark"));
          if (!light || !dark ||
              window->property("dark").toBool() != expectedDark ||
              light->property("primary").toBool() != !expectedDark ||
              dark->property("primary").toBool() != expectedDark) {
            fail("selected theme did not update the rendered controls");
            return;
          }
          QSettings preferences;
          preferences.sync();
          if (preferences.status() != QSettings::NoError ||
              !preferences.contains("dark") ||
              preferences.value("dark").toBool() != expectedDark) {
            fail("selected theme was not written to QSettings");
            return;
          }
          QDir().mkpath(captureDir);
          if (!window->grabWindow().save(captureDir + "/settings-" +
                                         setTheme + ".png")) {
            fail("could not capture the selected theme");
            return;
          }
          qInfo() << "Native settings controls PASS: demo status, Connect and "
                     "Disconnect hidden, no live action invoked";
          qInfo().noquote() << "Native settings theme saved PASS:" << setTheme;
          timer->stop();
          app.exit(0);
          return;
        }

        auto *dialog = named(window, "settingsDialog");
        auto *scrollObject = named(window, "settingsScroll");
        auto *scroll = qobject_cast<QQuickItem *>(scrollObject);
        auto *light = qobject_cast<QQuickItem *>(named(window, "settingsThemeLight"));
        auto *dark = qobject_cast<QQuickItem *>(named(window, "settingsThemeDark"));
        auto *connectionHeading = named(window, "settingsConnectionHeading");
        auto *connectionStatus = named(window, "settingsConnectionStatus");
        auto *connectAction = named(window, "connectAccountAction");
        auto *disconnectAction = named(window, "localDisconnectAction");
        auto *riskDialog = named(window, "connectRiskDialog");
        auto *disconnectScope = named(window, "connectionDisconnectScope");
        auto *unlinkNotice = named(window, "remoteUnlinkAvailability");
        if (!dialog || !dialog->property("visible").toBool() || !scroll ||
            !light || !dark || !connectionHeading || !connectionStatus ||
            !connectAction || !disconnectAction || !riskDialog ||
            !disconnectScope || !unlinkNotice)
          return;
        if (!c.demo() ||
            window->property("dark").toBool() != (expectedTheme == "dark") ||
            !light->isVisible() || !dark->isVisible() ||
            light->property("primary").toBool() ==
                dark->property("primary").toBool() ||
            light->property("primary").toBool() !=
                (expectedTheme == "light") ||
            dark->property("primary").toBool() !=
                (expectedTheme == "dark")) {
          fail("theme controls or active selection are not truthful");
          return;
        }
        if (connectionHeading->property("text").toString() != "Connection" ||
            connectionStatus->property("text").toString() !=
                "Synthetic conversations and attachments stay on this computer." ||
            connectAction->property("visible").toBool() ||
            disconnectAction->property("visible").toBool() ||
            riskDialog->property("visible").toBool()) {
          fail("demo Settings exposed live actions or misleading connection copy");
          return;
        }

        auto capture = [&](const QString &name) {
          QDir().mkpath(captureDir);
          return window->grabWindow().save(captureDir + "/" + name + ".png");
        };
        const auto initialCapture =
            setTheme.isEmpty() ? "settings-restored-" + expectedTheme
                               : "settings-" + expectedTheme;
        if (!capture(initialCapture)) {
          fail("could not capture rendered theme controls");
          return;
        }
        const auto scrollHeight = scroll->property("height").toDouble();
        const auto contentHeight =
            scroll->property("contentHeight").toDouble();
        scroll->setProperty("contentY",
                            qMax(0.0, contentHeight - scrollHeight));
        QCoreApplication::processEvents();
        const auto withinScroll = [scroll](QObject *object) {
          auto *item = qobject_cast<QQuickItem *>(object);
          if (!item || !scroll || !item->isVisible())
            return false;
          const auto position = item->mapToItem(scroll, QPointF(0, 0));
          return position.x() >= -1 && position.y() >= -1 &&
                 position.x() + item->width() <= scroll->width() + 1 &&
                 position.y() + item->height() <= scroll->height() + 1;
        };
        if (!withinScroll(disconnectScope) || !withinScroll(unlinkNotice)) {
          fail("connection boundary notices are clipped from the Settings viewport");
          return;
        }
        const auto connectionCapture =
            setTheme.isEmpty() ? "settings-connection-restored-" + expectedTheme
                               : "settings-connection-" + expectedTheme;
        if (!capture(connectionCapture)) {
          fail("could not capture rendered connection boundary");
          return;
        }
        if (setTheme.isEmpty()) {
          QSettings preferences;
          preferences.sync();
          if (preferences.status() != QSettings::NoError ||
              !preferences.contains("dark") ||
              preferences.value("dark").toBool() !=
                  (expectedTheme == "dark")) {
            fail("restarted app did not load the persisted theme preference");
            return;
          }
          qInfo() << "Native settings controls PASS: demo status, Connect and "
                     "Disconnect hidden, no live action invoked";
          qInfo().noquote() << "Native settings preference restored PASS:"
                            << expectedTheme;
          timer->stop();
          app.exit(0);
          return;
        }
        if ((setTheme != "dark" && setTheme != "light") ||
            setTheme == expectedTheme) {
          fail("target theme must be a valid theme different from startup");
          return;
        }
        scroll->setProperty("contentY", 0.0);
        QCoreApplication::processEvents();
        auto *target = setTheme == "dark" ? dark : light;
        if (!target->isVisible() ||
            !invoke(window, target->objectName(), "clicked")) {
          fail("target theme control could not be activated");
          return;
        }
        state->stage = 2;
        return;
      });
  timer->start();
}

void runProductTest(QGuiApplication &app, QQuickWindow *window, Controller &c,
                    const QString &fixture, const QString &captureDir) {
  struct State {
    int stage = 0;
    int deadlineStage = -1;
    int stageTicks = 0;
    int audioLabelWaitTicks = 0;
    QString audioLabelWaitRecord;
    QString audioLabelWaitDiagnostic;
    QString mediaTarget;
    QString mediaChat;
    QStringList mediaRpcMethods;
    QString managedGroupId;
    bool disconnectBoundaryScrolled = false;
    bool compactDisconnectBoundaryScrolled = false;
    bool profileEditPassed = false;
  };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(100);
  QObject::connect(timer, &QTimer::timeout, &app, [&, window, fixture, captureDir, state, timer] {
    auto capture = [&](const QString &name) {
      if (!c.demo()) return;
      QDir().mkpath(captureDir);
      window->grabWindow().save(captureDir + "/" + name + ".png");
    };
    auto fail = [&](const QString &message) {
      qCritical() << "Native product FAIL at stage" << state->stage << message;
      timer->stop();
      app.exit(1);
    };
    const auto profileText = [window](const QString &name) {
      auto *label = named(window, name);
      return label ? label->property("text").toString() : QString();
    };
    if (state->deadlineStage != state->stage) {
      state->deadlineStage = state->stage;
      state->stageTicks = 0;
    }
    if (++state->stageTicks > 240) {
      if (state->stage == 20 && !state->audioLabelWaitDiagnostic.isEmpty()) {
        fail(QStringLiteral("timed out waiting for rendered timeline fixtures: %1")
                 .arg(state->audioLabelWaitRecord + QStringLiteral(" ") +
                      state->audioLabelWaitDiagnostic));
      } else if (state->stage == 50) {
        auto *player = named(window, "viewerPlayer");
        auto *video = named(window, "viewerVideoOutput");
        auto *sink = video ? qobject_cast<QVideoSink *>(video->property("videoSink").value<QObject *>()) : nullptr;
        const QString playbackState = player
                                          ? QString::number(player->property("playbackState").toInt())
                                          : QStringLiteral("missing");
        const QString position = player
                                     ? QString::number(player->property("position").toLongLong())
                                     : QStringLiteral("missing");
        const QString duration = player
                                     ? QString::number(player->property("duration").toLongLong())
                                     : QStringLiteral("missing");
        const QString error = player
                                  ? player->property("errorString").toString().left(160)
                                  : QStringLiteral("missing player");
        fail(QStringLiteral("timed out waiting for video frame after %1 stage ticks: playbackState=%2 position=%3 duration=%4 sink=%5 frameValid=%6 error=%7")
                 .arg(state->stageTicks)
                 .arg(playbackState)
                 .arg(position)
                 .arg(duration)
                 .arg(sink ? QStringLiteral("present") : QStringLiteral("missing"))
                 .arg(sink && sink->videoFrame().isValid() ? QStringLiteral("yes") : QStringLiteral("no"))
                 .arg(error));
      } else {
        fail(state->stage == 20 ? QStringLiteral("timed out waiting for rendered timeline fixtures")
                                : QStringLiteral("timed out after %1 stage ticks").arg(state->stageTicks));
      }
      return;
    }
    if (!c.demo()) { fail("non-demo controller"); return; }
    if (state->stage == 0 && c.messageCount() > 0) {
      auto *wallpaper = qobject_cast<QQuickItem *>(named(window, "chatWallpaper"));
      if (!wallpaper) { fail("default chat wallpaper missing"); return; }
      if (wallpaper->property("loadStatus").toInt() != 1) return;
      const auto expected = window->property("dark").toBool() ? QStringLiteral("chat-dark.webp") : QStringLiteral("chat-green.webp");
      if (wallpaper->width() <= 0 || wallpaper->height() <= 0 || !wallpaper->clip() ||
          !wallpaper->property("aspectPreserving").toBool() ||
          wallpaper->property("wallpaperSource").toUrl().fileName() != expected) {
        fail("default wallpaper geometry, theme or aspect policy"); return;
      }
      qInfo() << "Native chat wallpaper PASS: decoded, theme-matched, clipped and aspect-preserving";
      const auto message = [](const QString &id, const QString &body, bool outgoing, qint64 timestamp) {
        return QVariantMap{{"id", id}, {"chat_id", "demo:alex"}, {"body", body},
                           {"outgoing", outgoing}, {"timestamp", timestamp},
                           {"status", outgoing ? "demo_sent" : "received"}};
      };
      auto quoted = message("probe-5", "Replying to a photo", false, 1700086402000LL);
      quoted.insert("quoted_sender", "Synthetic sender");
      quoted.insert("quoted_body", "Photo");
      auto unsupported = message("probe-unsupported", "[Unsupported content — open the official app]", false, 1700086403000LL);
      unsupported.insert("kind", "unsupported");
      auto plainAudio = message("probe-audio", "", false, 1700086404000LL);
      plainAudio.insert("kind", "audio");
      plainAudio.insert("attachment", QVariantMap{{"kind", "audio"}, {"mime", "audio/ogg; codecs=opus"},
                                                   {"duration", 2}, {"state", "remote"},
                                                   {"is_voice_note", false}});
      auto voiceNote = message("probe-voice-note", "", false, 1700086405000LL);
      voiceNote.insert("kind", "audio");
      const bool oggVoiceFixture = fixture.endsWith(".ogg", Qt::CaseInsensitive);
      const bool readyVoiceFixture = oggVoiceFixture || fixture.endsWith(".wav", Qt::CaseInsensitive);
      QVariantMap voiceAttachment{{"kind", "audio"},
                                  {"mime", oggVoiceFixture ? QStringLiteral("audio/ogg; codecs=opus") : QStringLiteral("audio/wav")},
                                  {"duration", oggVoiceFixture ? 5 : 1}, {"state", "remote"},
                                  {"is_voice_note", true}};
      if (readyVoiceFixture) {
        voiceAttachment.insert("state", "ready");
        voiceAttachment.insert("url", QUrl::fromLocalFile(fixture).toString());
      }
      voiceNote.insert("attachment", voiceAttachment);
      auto readReceipt = message("probe-4", "https://example.com/" + QString(180, QLatin1Char('a')), true, 1700086401000LL);
      readReceipt.insert("status", "read");
      c.messages()->replace({message("probe-1", "p", true, 1700000000000LL),
                             message("probe-2", "x", false, 1700000001000LL),
                             message("probe-3", QString::fromUtf8("Line one\n日本語 — pesan berbaris"), false, 1700086400000LL),
                             readReceipt,
                             quoted, unsupported, plainAudio, voiceNote});
      state->stage = 20;
      return;
    }
    if (state->stage == 20) {
      auto *timeline = qobject_cast<QQuickItem *>(named(window, "messageTimeline"));
      auto *bubble = qobject_cast<QQuickItem *>(named(window, "bubbleSurface"));
      if (!timeline || !bubble || timeline->width() <= 0) { fail("message timeline geometry"); return; }
      // The real delegate items are used, not a screenshot-only assertion.
      if (timeline->width() < 300 || bubble->width() <= 0 ||
          bubble->width() > timeline->width() + 1 || bubble->height() <= 0) {
        fail("bubble geometry"); return;
      }
      int testedShort = 0;
      int testedQuote = 0;
      int testedUnsupported = 0;
      int testedAudio = 0;
      int testedVoiceNote = 0;
      int testedReceiptRead = 0;
      std::function<void(QQuickItem *)> verify = [&](QQuickItem *item) {
        if (item->objectName() == "messageBubble" && item->isVisible()) {
          const auto value = item->property("record");
          const auto record = value.canConvert<QJSValue>() ? value.value<QJSValue>().toVariant().toMap() : value.toMap();
          if (record.value("id").toString() == "probe-unsupported") {
            const auto expected = QStringLiteral("[Unsupported content — open the official app]");
            auto *body = namedBelow(item, "messageBody");
            const bool kindMatches = record.value("kind").toString() == QStringLiteral("unsupported");
            const bool bodyMatches = record.value("body").toString() == expected;
            const bool bodyVisible = body && body->isVisible();
            const QString renderedText = body ? body->property("text").toString() : QString{};
            const bool textMatches = body && renderedText.contains(expected);
            if (!kindMatches || !bodyMatches || !bodyVisible || !textMatches) {
              fail(QStringLiteral("unknown-content fallback mismatch: kind=%1 body=%2 item=%3 visible=%4 text=%5")
                       .arg(kindMatches).arg(bodyMatches).arg(body != nullptr).arg(bodyVisible).arg(textMatches));
              return;
            }
            ++testedUnsupported;
          }
          const auto recordID = record.value("id").toString();
          if (recordID == "probe-4") {
            auto *statusLabel = namedBelow(item, "messageStatusLabel");
            auto *statusAccessible = statusLabel ? QAccessible::queryAccessibleInterface(statusLabel) : nullptr;
            if (!statusLabel || !statusLabel->isVisible() ||
                statusLabel->property("text").toString() != QStringLiteral("Read") ||
                !statusAccessible ||
                statusAccessible->text(QAccessible::Name) != QStringLiteral("Message status: Read")) {
              fail("read receipt label/accessibility does not match the exact synthetic message state");
              return;
            }
            ++testedReceiptRead;
          }
          if (recordID == "probe-audio" || recordID == "probe-voice-note") {
            const bool isVoiceNote = recordID == "probe-voice-note";
            const bool ready = isVoiceNote && (fixture.endsWith(".wav", Qt::CaseInsensitive) || fixture.endsWith(".ogg", Qt::CaseInsensitive));
            const auto attachment = record.value("attachment").toMap();
            const bool typed = attachment.value("kind").toString() == QStringLiteral("audio") &&
                               attachment.value("is_voice_note").toBool() == isVoiceNote &&
                               attachment.value("state").toString() == (ready ? QStringLiteral("ready") : QStringLiteral("remote")) &&
                               (!ready || attachment.value("url").toString().startsWith(QStringLiteral("file:///")));
            const auto expectedLabel = isVoiceNote ? QStringLiteral("Voice note") : QStringLiteral("Audio");
            const auto expectedState = ready ? QString::number(attachment.value("duration").toInt()) + QStringLiteral(" s") : QStringLiteral("Not downloaded");
            QStringList renderedLabels;
            bool labelFound = false;
            std::function<void(QQuickItem *)> findAudioLabel = [&](QQuickItem *node) {
              const auto text = node->property("text").toString();
              if (!text.isEmpty()) {
                renderedLabels.append(QStringLiteral("%1 [%2] %3")
                                          .arg(node->objectName().isEmpty() ? QStringLiteral("<unnamed>") : node->objectName(),
                                               node->isVisible() ? QStringLiteral("visible") : QStringLiteral("hidden"),
                                               text.left(80)));
              }
              if (node->isVisible() && text.contains(expectedLabel) &&
                  text.contains(expectedState, Qt::CaseInsensitive)) {
                labelFound = true;
              }
              for (auto *child : node->childItems()) findAudioLabel(child);
            };
            findAudioLabel(item);
            if (!typed) {
              fail(QStringLiteral("audio identity label mismatch: voice=%1 typed=%2 label=%3 expected=%4/%5 rendered=%6")
                       .arg(isVoiceNote).arg(typed).arg(labelFound)
                       .arg(expectedLabel, expectedState, renderedLabels.join(QStringLiteral(" | "))));
              return;
            }
            if (!labelFound) {
              if (state->audioLabelWaitRecord != recordID) {
                state->audioLabelWaitRecord = recordID;
                state->audioLabelWaitTicks = 0;
              }
              ++state->audioLabelWaitTicks;
              state->audioLabelWaitDiagnostic =
                  QStringLiteral("voice=%1 expected=%2/%3 rendered=%4")
                      .arg(isVoiceNote)
                      .arg(expectedLabel, expectedState,
                           renderedLabels.join(QStringLiteral(" | ")));
              return;
            }
            if (state->audioLabelWaitRecord == recordID &&
                state->audioLabelWaitTicks > 0) {
              qInfo() << "Native product audio label settled after"
                      << state->audioLabelWaitTicks << "polls for" << recordID;
              state->audioLabelWaitTicks = 0;
              state->audioLabelWaitRecord.clear();
              state->audioLabelWaitDiagnostic.clear();
            }
            if (isVoiceNote) ++testedVoiceNote;
            else ++testedAudio;
          }
          if (record.value("id").toString() == "probe-5") {
            std::function<void(QQuickItem *)> verifyQuote = [&](QQuickItem *node) {
              if (node->property("text").toString() == QStringLiteral("Synthetic sender · Photo")) {
                if (!node->isVisible() || node->property("textFormat").toInt() != int(Qt::PlainText)) {
                  fail("quoted preview must be visible PlainText in the real message delegate"); return;
                }
                ++testedQuote;
              }
              for (auto *child : node->childItems()) verifyQuote(child);
            };
            verifyQuote(item);
          }
          if (record.value("id").toString() == "probe-1") {
            ++testedShort;
            for (auto *child : item->childItems()) {
              if (child->property("text").toString() == "p" && !child->isVisible() && child->property("textFormat").toInt() != int(Qt::PlainText)) {
                fail("intrinsic measurement must not interpret untrusted rich text"); return;
              }
              if (child->objectName() != "bubbleSurface") continue;
              QFont metadataFont = QGuiApplication::font();
              metadataFont.setPixelSize(10);
              const QFontMetricsF metadataMetrics(metadataFont);
              const auto requiredMetadataWidth = metadataMetrics.horizontalAdvance(c.timeLabel(1700000000000LL)) + metadataMetrics.horizontalAdvance("Saved locally") + 26;
              if (child->width() + 2 < requiredMetadataWidth) {
                fail("short message width clips receipt/time text"); return;
              }
              if (child->width() > 200 || qAbs(child->x() + child->width() - item->width()) > 1) {
                fail("short outgoing bubble is not naturally sized and right aligned"); return;
              }
              for (auto *label : child->findChildren<QQuickItem *>()) {
                if (label->isVisible() && label->property("text").toString() == "Saved locally" && label->mapToItem(child, {0,0}).x() < 8) {
                  fail("short outgoing message clips its timestamp/receipt footer"); return;
                }
              }
            }
          }
        }
        for (auto *child : item->childItems()) verify(child);
      };
      verify(window->contentItem());
      if (!timer->isActive()) return;
      if (testedShort > 1 || testedQuote > 1 || testedUnsupported > 1 || testedAudio > 1 || testedVoiceNote > 1 || testedReceiptRead > 1) {
        fail("duplicate native timeline fixture delegate"); return;
      }
      // ListView delegate creation/reuse follows modelReset asynchronously. Wait
      // for the real fixture rows to settle instead of failing on the first tick.
      if (testedShort != 1 || testedQuote != 1 || testedUnsupported != 1 ||
          testedAudio != 1 || testedVoiceNote != 1 || testedReceiptRead != 1) return;
      qInfo() << "Native receipt status UI PASS: synthetic read receipt is rendered accessibly and bound to its message";
      auto *newChat = named(window, "newChatButton");
      auto *mediaHub = named(window, "mediaLibraryButton");
      if (!newChat || !mediaHub || newChat->property("enabled").toBool() ||
          !mediaHub->property("enabled").toBool()) {
        fail("demo contact lookup must stay disabled; cached media library must be enabled"); return;
      }
      capture("message-layout");
      capture("unsupported-content");
      capture("incoming-audio-voice-note");
      state->stage = 1;
      if (!invoke(window, "settingsButton", "clicked")) { fail("settings button"); return; }
    } else if (state->stage == 1) {
      auto *dialog = named(window, "settingsDialog");
      auto *scroll = named(window, "settingsScroll");
      auto *footer = named(window, "settingsClose");
      if (!dialog || !dialog->property("visible").toBool() || !scroll || !footer) return;
      const auto dialogWidth = dialog->property("width").toDouble();
      const auto dialogHeight = dialog->property("height").toDouble();
      if (dialogWidth <= 0 || dialogHeight <= 0 ||
          dialogWidth > window->width() - 20 || dialogHeight > window->height() - 20) { fail("settings does not fit viewport"); return; }
      // QQuickPopup is a QObject, not a QQuickItem. Its background is the
      // actual scene item spanning the popup's outer bounds.
      auto dialogItem = qobject_cast<QQuickItem *>(dialog->property("background").value<QObject *>());
      auto footerItem = qobject_cast<QQuickItem *>(footer);
      if (!dialogItem || !footerItem) { fail("settings footer geometry unavailable"); return; }
      const auto dialogTop = dialogItem->mapToScene({0, 0}).y();
      const auto dialogBottom = dialogTop + dialogItem->height();
      const auto footerTop = footerItem->mapToScene({0, 0}).y();
      const auto footerBottom = footerTop + footerItem->height();
      if (footerTop < dialogTop || footerBottom > dialogBottom - 12 ||
          footerItem->height() < 36) {
        fail(QString("settings footer clipped/overlapping: dialog=%1..%2 footer=%3..%4 height=%5")
                 .arg(dialogTop).arg(dialogBottom).arg(footerTop).arg(footerBottom)
                 .arg(footerItem->height()));
        return;
      }
      auto *notificationToggle = named(window, "desktopNotificationsEnabled");
      auto *previewToggle = named(window, "desktopMessagePreviewsEnabled");
      auto *preferences = qobject_cast<QQuickItem *>(named(window, "notificationPreferences"));
      if (!notificationToggle || !previewToggle || !preferences ||
          notificationToggle->property("checked").toBool() || previewToggle->property("checked").toBool() ||
          previewToggle->property("enabled").toBool()) { fail("notification privacy defaults missing"); return; }
      auto *availabilityStatus =
          named(window, "notificationTrayAvailabilityStatus");
      if (!availabilityStatus || preferences->metaObject()->indexOfProperty(
                                     "notificationsAvailable") < 0) {
        fail(
            "settings do not expose desktop notification-service availability");
        return;
      }
      auto *disconnectAction = named(window, "localDisconnectAction");
      auto *disconnectScope = named(window, "connectionDisconnectScope");
      auto *remoteUnlinkNotice = named(window, "remoteUnlinkAvailability");
      const auto disconnectActionText =
          disconnectAction ? disconnectAction->property("text").toString()
                           : QString{};
      const auto disconnectScopeText =
          disconnectScope ? disconnectScope->property("text").toString()
                          : QString{};
      const auto remoteUnlinkText =
          remoteUnlinkNotice ? remoteUnlinkNotice->property("text").toString()
                             : QString{};
      if (disconnectActionText !=
              QStringLiteral("Disconnect here — keep device linked") ||
          !disconnectScopeText.contains(QStringLiteral("linked-device session")) ||
          !disconnectScopeText.contains(QStringLiteral("local history")) ||
          !disconnectScopeText.contains(QStringLiteral("does not unlink")) ||
          !remoteUnlinkText.contains(QStringLiteral("not available here")) ||
          !remoteUnlinkText.contains(QStringLiteral("Linked devices"))) {
        fail("disconnect and remote unlink/logout boundaries are unclear");
        return;
      }
      auto *scrollViewport = qobject_cast<QQuickItem *>(scroll);
      const auto visibleWithinScroll = [&](QObject *object) {
        auto *item = qobject_cast<QQuickItem *>(object);
        if (!item || !scrollViewport || !item->isVisible())
          return false;
        const auto position = item->mapToItem(scrollViewport, QPointF(0, 0));
        return position.x() >= -1 && position.y() >= -1 &&
               position.x() + item->width() <= scrollViewport->width() + 1 &&
               position.y() + item->height() <= scrollViewport->height() + 1;
      };
      if (!state->disconnectBoundaryScrolled) {
        const auto contentHeight = scroll->property("contentHeight").toDouble();
        const auto viewportHeight = scroll->property("height").toDouble();
        if (!scrollViewport || contentHeight <= viewportHeight + 1 ||
            !scroll->setProperty("contentY", contentHeight - viewportHeight)) {
          fail("settings scroll cannot reveal disconnect boundary notices");
          return;
        }
        state->disconnectBoundaryScrolled = true;
        return;
      }
      if (!visibleWithinScroll(disconnectScope) ||
          !visibleWithinScroll(remoteUnlinkNotice)) {
        fail("disconnect and remote unlink/logout notices remain outside the settings viewport after scrolling");
        return;
      }
      capture("settings-disconnect-boundary");
      scroll->setProperty("contentY", 0.0);
      QCoreApplication::processEvents();
      qInfo() << "Native disconnect/unlink boundary UI PASS";
      const bool initialTrayAvailable =
          preferences->property("trayAvailable").toBool();
      const bool initialNotificationsAvailable =
          preferences->property("notificationsAvailable").toBool();
      const auto verifyAvailability = [&](bool tray, bool notifications,
                                          const QString &expected) {
        if (!preferences->setProperty("trayAvailable", tray) ||
            !preferences->setProperty("notificationsAvailable",
                                      notifications)) {
          fail("could not set notification availability fixture");
          return false;
        }
        QCoreApplication::processEvents();
        const auto actual = availabilityStatus->property("text").toString();
        if (actual != expected) {
          fail(QString("notification availability copy mismatch: expected "
                       "'%1', got '%2'")
                   .arg(expected, actual));
          return false;
        }
        return true;
      };
      if (!verifyAvailability(
              false, false,
              QStringLiteral("System tray unavailable. Notifications cannot be "
                             "delivered on this desktop yet.")) ||
          !verifyAvailability(
              true, false,
              QStringLiteral("System tray available, but desktop notifications "
                             "are unavailable on this desktop.")) ||
          !verifyAvailability(
              true, true,
              QStringLiteral(
                  "System tray and desktop notifications are available."))) {
        return;
      }
      preferences->setProperty("trayAvailable", initialTrayAvailable);
      preferences->setProperty("notificationsAvailable", initialNotificationsAvailable);
      QCoreApplication::processEvents();
      auto toggle = [](QObject *object) {
        auto *item = qobject_cast<QQuickItem *>(object);
        if (!item) return;
        item->forceActiveFocus();
        QKeyEvent press(QEvent::KeyPress, Qt::Key_Space, Qt::NoModifier);
        QKeyEvent release(QEvent::KeyRelease, Qt::Key_Space, Qt::NoModifier);
        QCoreApplication::sendEvent(item, &press);
        QCoreApplication::sendEvent(item, &release);
      };
      toggle(notificationToggle);
      if (!notificationToggle->property("checked").toBool() || !previewToggle->property("enabled").toBool()) { fail("notification opt-in binding"); return; }
      toggle(previewToggle);
      if (!previewToggle->property("checked").toBool()) { fail("preview explicit opt-in binding"); return; }
      toggle(previewToggle); toggle(notificationToggle);
      if (notificationToggle->property("checked").toBool() || previewToggle->property("checked").toBool() || previewToggle->property("enabled").toBool()) { fail("notification opt-out binding"); return; }
      qInfo() << "Native notification preferences PASS: disabled defaults, explicit keyboard opt-ins, restored opt-out";
      capture("settings-desktop");
      invoke(window, "settingsDialog", "close");
      window->resize(720, 600);
      if (!invoke(window, "settingsButton", "clicked")) { fail("compact settings button"); return; }
      state->stage = 11;
    } else if (state->stage == 11) {
      auto *dialog = named(window, "settingsDialog");
      auto *scroll = named(window, "settingsScroll");
      auto *disconnectScope = named(window, "connectionDisconnectScope");
      auto *remoteUnlinkNotice = named(window, "remoteUnlinkAvailability");
      if (!dialog || !dialog->property("visible").toBool() || !scroll ||
          !disconnectScope || !remoteUnlinkNotice) return;
      const auto dialogWidth = dialog->property("width").toDouble();
      const auto dialogHeight = dialog->property("height").toDouble();
      if (dialogWidth > window->width() - 20 || dialogHeight > window->height() - 20) { fail("compact settings does not fit"); return; }
      auto *scrollViewport = qobject_cast<QQuickItem *>(scroll);
      const auto visibleWithinScroll = [&](QObject *object) {
        auto *item = qobject_cast<QQuickItem *>(object);
        if (!item || !scrollViewport || !item->isVisible())
          return false;
        const auto position = item->mapToItem(scrollViewport, QPointF(0, 0));
        return position.x() >= -1 && position.y() >= -1 &&
               position.x() + item->width() <= scrollViewport->width() + 1 &&
               position.y() + item->height() <= scrollViewport->height() + 1;
      };
      if (!state->compactDisconnectBoundaryScrolled) {
        const auto contentHeight = scroll->property("contentHeight").toDouble();
        const auto viewportHeight = scroll->property("height").toDouble();
        if (!scrollViewport || contentHeight <= viewportHeight + 1 ||
            !scroll->setProperty("contentY", contentHeight - viewportHeight)) {
          fail("compact settings scroll cannot reveal disconnect boundary notices");
          return;
        }
        state->compactDisconnectBoundaryScrolled = true;
        return;
      }
      if (!visibleWithinScroll(disconnectScope) ||
          !visibleWithinScroll(remoteUnlinkNotice)) {
        fail("compact disconnect and remote unlink/logout notices are outside the visible settings viewport after scrolling");
        return;
      }
      capture("settings-compact-disconnect-boundary");
      scroll->setProperty("contentY", 0.0);
      QCoreApplication::processEvents();
      qInfo() << "Native compact disconnect/unlink boundary UI PASS";
      capture("settings");
      invoke(window, "settingsDialog", "close");
      if (!invoke(window, "profileButton", "clicked")) { fail("profile button"); return; }
      state->stage = 2;
    } else if (state->stage == 2) {
      auto *dialog = named(window, "profileDialog");
      if (!dialog || !dialog->property("visible").toBool()) return;
      if (!state->profileEditPassed) {
        if (profileText("profileDisplayName") != "Demo Account") {
          fail("profile display name does not match the synthetic account projection"); return;
        }
        if (profileText("profileAccountState") != "Synthetic local profile") {
          fail("profile state does not disclose the synthetic local identity"); return;
        }
        if (profileText("profilePhone") != "+1 555 0100" ||
            profileText("profileUsername") != "@demo" ||
            profileText("profileAbout") != "Synthetic local identity") {
          fail("profile details do not match the synthetic account projection"); return;
        }
        auto *editButton = named(window, "editDemoProfileButton");
        auto *profileViewport = qobject_cast<QQuickItem *>(named(window, "profileContentViewport"));
        auto *editAction = qobject_cast<QQuickItem *>(editButton);
        if (!editButton || !editButton->property("visible").toBool()) {
          fail("demo profile edit action is missing or visible outside its guarded mode"); return;
        }
        if (!profileViewport || !editAction) {
          fail("profile edit action geometry cannot be checked against its scroll viewport"); return;
        }
        const auto editRect = editAction->mapRectToItem(profileViewport, editAction->boundingRect());
        if (editRect.top() < 0 || editRect.bottom() > profileViewport->height()) {
          fail("demo profile edit action is clipped by the profile dialog footer"); return;
        }
        capture("profile");
        if (!invoke(window, "editDemoProfileButton", "clicked")) {
          fail("demo profile editor did not open"); return;
        }
        state->stage = 2400;
        return;
      }
      if (profileText("profileDisplayName") != "Synthetic Demo Edited" ||
          profileText("profileAbout") != "Updated only in this local demo session") {
        fail("saved synthetic profile values were not rendered"); return;
      }
      if (profileText("profileAccountState") != "Synthetic local profile" ||
          !profileText("profileEditStatus").contains("not sent to WhatsApp")) {
        fail("edited profile no longer discloses its local-only scope"); return;
      }
      if (c.routeMethod() == "account.update" || c.hasCapability("account.update")) {
        fail("demo profile editing must not dispatch or advertise account.update"); return;
      }
      qInfo() << "Native demo profile edit PASS: synthetic values saved locally, cancel/blank guard verified, no account.update dispatched";
      capture("profile-edited");
      invoke(window, "profileDialog", "close");
      if (fixture.endsWith(".mp4", Qt::CaseInsensitive)) {
        if (!invoke(window, "attachmentButton", "clicked")) { fail("attachment menu button"); return; }
        state->stage = 21;
        return;
      }
      if (fixture.endsWith(".pdf", Qt::CaseInsensitive)) {
        if (!invoke(window, "attachmentButton", "clicked")) { fail("document attachment menu button"); return; }
        state->stage = 41;
        return;
      }
      if (fixture.endsWith(".ogg", Qt::CaseInsensitive)) {
        if (!invoke(window, "attachmentButton", "clicked")) { fail("audio attachment menu button"); return; }
        state->stage = 23;
        return;
      }
      if (fixture.endsWith(".wav", Qt::CaseInsensitive)) {
        state->stage = 4;
        return;
      }
      if (!fixture.isEmpty()) c.stageAttachmentAs(QUrl::fromLocalFile(fixture), fixture.endsWith(".gif", Qt::CaseInsensitive) ? QStringLiteral("original") : QStringLiteral("photo"));
      state->stage = fixture.isEmpty() ? 4 : 3;
    } else if (state->stage == 2400) {
      auto *editor = named(window, "demoProfileEditor");
      auto *nameInput = named(window, "demoProfileNameInput");
      auto *aboutInput = named(window, "demoProfileAboutInput");
      auto *save = named(window, "profileEditSave");
      if (!editor || !editor->property("visible").toBool() || !nameInput ||
          !aboutInput || !save ||
          !profileText("profileEditLocalNotice").contains("not sent to WhatsApp")) {
        fail("demo profile editor fields or local-only notice are missing"); return;
      }
      if (nameInput->property("text").toString() != "Demo Account" ||
          aboutInput->property("text").toString() != "Synthetic local identity") {
        fail("demo profile editor did not initialize from the synthetic projection"); return;
      }
      capture("profile-editing");
      nameInput->setProperty("text", "   ");
      aboutInput->setProperty("text", "Discarded draft");
      QCoreApplication::processEvents();
      if (save->property("enabled").toBool()) {
        fail("blank demo profile name must block local save"); return;
      }
      nameInput->setProperty("text", "Discarded Demo Name");
      aboutInput->setProperty("text", "Discarded local draft");
      if (!invoke(window, "profileEditCancel", "clicked")) {
        fail("demo profile edit cancel did not trigger"); return;
      }
      state->stage = 2401;
    } else if (state->stage == 2401) {
      auto *editor = named(window, "demoProfileEditor");
      if ((editor && editor->property("visible").toBool()) ||
          profileText("profileDisplayName") != "Demo Account" ||
          profileText("profileAbout") != "Synthetic local identity") {
        fail("cancelled profile draft changed the synthetic profile"); return;
      }
      if (!invoke(window, "editDemoProfileButton", "clicked")) {
        fail("demo profile editor could not reopen after cancel"); return;
      }
      state->stage = 2402;
    } else if (state->stage == 2402) {
      auto *editor = named(window, "demoProfileEditor");
      auto *nameInput = named(window, "demoProfileNameInput");
      auto *aboutInput = named(window, "demoProfileAboutInput");
      auto *save = named(window, "profileEditSave");
      if (!editor || !editor->property("visible").toBool() || !nameInput ||
          !aboutInput || !save) return;
      nameInput->setProperty("text", "Synthetic Demo Edited");
      aboutInput->setProperty("text", "Updated only in this local demo session");
      QCoreApplication::processEvents();
      if (!save->property("enabled").toBool() ||
          !invoke(window, "profileEditSave", "clicked")) {
        fail("valid synthetic profile edit could not be saved locally"); return;
      }
      state->profileEditPassed = true;
      state->stage = 2;
    } else if (state->stage == 21) {
      auto *videoAction = named(window, "videoAttachmentAction");
      if (!videoAction || !videoAction->property("visible").toBool()) return;
      if (!videoAction->property("enabled").toBool() ||
          videoAction->property("text").toString() != QStringLiteral("Video…")) {
        fail("native Video selection action is missing or disabled"); return;
      }
      if (!QMetaObject::invokeMethod(videoAction, "triggered")) { fail("native Video selection action did not trigger"); return; }
      state->stage = 22;
    } else if (state->stage == 22) {
      auto *picker = named(window, "attachmentFilePicker");
      if (!picker || !picker->property("visible").toBool()) return;
      const auto filters = picker->property("nameFilters").toStringList();
      if (!filters.contains(QStringLiteral("MP4 video (*.mp4)"))) {
        fail("video picker does not restrict selection to MP4"); return;
      }
      QMetaObject::invokeMethod(picker, "close");
      c.setDraft(QStringLiteral("synthetic video caption"));
      c.stageAttachmentAs(QUrl::fromLocalFile(fixture), QStringLiteral("video"));
      state->stage = 3;
    } else if (state->stage == 23) {
      auto *audioAction = named(window, "audioAttachmentAction");
      auto *voiceNoteAction = named(window, "voiceNoteAttachmentAction");
      if (!audioAction || !voiceNoteAction || !audioAction->property("visible").toBool() ||
          !voiceNoteAction->property("visible").toBool() ||
          !voiceNoteAction->property("text").toString().contains("from file")) {
        fail("audio file and file-based voice-note actions are missing"); return;
      }
      if (!QMetaObject::invokeMethod(audioAction, "triggered")) {
        fail("audio file action did not trigger"); return;
      }
      state->stage = 24;
    } else if (state->stage == 24) {
      auto *picker = named(window, "attachmentFilePicker");
      auto *appRoot = named(window, "appWindow");
      if (!appRoot) appRoot = window;
      if (!picker || !picker->property("visible").toBool()) return;
      const auto filters = picker->property("nameFilters").toStringList();
      if (appRoot->property("attachmentMode").toString() != "audio" ||
          picker->property("title").toString() != "Choose an OGG/Opus audio file" ||
          !filters.contains(QStringLiteral("OGG/Opus audio (*.ogg)"))) {
        fail("audio file picker must select regular-audio mode and OGG/Opus only"); return;
      }
      QMetaObject::invokeMethod(picker, "close");
      if (!invoke(window, "attachmentButton", "clicked")) { fail("reopen audio attachment menu"); return; }
      state->stage = 25;
    } else if (state->stage == 25) {
      auto *voiceNoteAction = named(window, "voiceNoteAttachmentAction");
      if (!voiceNoteAction || !voiceNoteAction->property("visible").toBool()) return;
      if (!QMetaObject::invokeMethod(voiceNoteAction, "triggered")) {
        fail("voice-note file action did not trigger"); return;
      }
      state->stage = 26;
    } else if (state->stage == 26) {
      auto *picker = named(window, "attachmentFilePicker");
      auto *appRoot = named(window, "appWindow");
      if (!appRoot) appRoot = window;
      if (!picker || !picker->property("visible").toBool()) return;
      const auto filters = picker->property("nameFilters").toStringList();
      if (appRoot->property("attachmentMode").toString() != "voice-note" ||
          picker->property("title").toString() != "Choose an OGG/Opus voice note" ||
          !filters.contains(QStringLiteral("OGG/Opus audio (*.ogg)"))) {
        fail("voice-note file picker must select voice-note mode and OGG/Opus only"); return;
      }
      QMetaObject::invokeMethod(picker, "close");
      c.setDraft(QStringLiteral("synthetic audio caption"));
      c.stageAttachmentAs(QUrl::fromLocalFile(fixture), QStringLiteral("voice-note"));
      state->stage = 3;
    } else if (state->stage == 41) {
      auto *documentAction = named(window, "documentAttachmentAction");
      if (!documentAction || !documentAction->property("visible").toBool() ||
          documentAction->property("text").toString() != QStringLiteral("Document…")) {
        fail("native Document selection action is missing"); return;
      }
      if (!QMetaObject::invokeMethod(documentAction, "triggered")) {
        fail("native Document selection action did not trigger"); return;
      }
      state->stage = 42;
    } else if (state->stage == 42) {
      auto *picker = named(window, "attachmentFilePicker");
      auto *appRoot = named(window, "appWindow");
      if (!appRoot) appRoot = window;
      if (!picker || !picker->property("visible").toBool()) return;
      const auto filters = picker->property("nameFilters").toStringList();
      if (appRoot->property("attachmentMode").toString() != "document" ||
          picker->property("title").toString() != "Choose an attachment" ||
          !filters.contains(QStringLiteral("All files (*)"))) {
        fail("document picker must preserve document mode and allow regular file types"); return;
      }
      QMetaObject::invokeMethod(picker, "close");
      c.setDraft(QStringLiteral("synthetic document caption"));
      c.stageAttachmentAs(QUrl::fromLocalFile(fixture), QStringLiteral("document"));
      state->stage = 3;
    } else if (state->stage == 3 && !c.attachment().isEmpty() && c.attachment().value("state").toString() != QStringLiteral("preparing")) {
      if (c.attachment().value("kind").toString().isEmpty()) { fail("media metadata"); return; }
      if (fixture.endsWith(".mp4", Qt::CaseInsensitive) && c.attachment().value("kind").toString() != QStringLiteral("video")) { fail("selected MP4 was not validated as video"); return; }
      if (fixture.endsWith(".pdf", Qt::CaseInsensitive) && c.attachment().value("kind").toString() != QStringLiteral("document")) { fail("selected PDF was not staged as a document"); return; }
      if (!invoke(window, "attachmentPreviewDialog", "open")) { fail("attachment preview missing"); return; }
      state->stage = 30;
    } else if (state->stage == 30) {
      auto *dialog = named(window, "attachmentPreviewDialog");
      if (!dialog || !dialog->property("opened").toBool()) return;
      auto *background = qobject_cast<QQuickItem *>(dialog->property("background").value<QObject *>());
      auto *caption = qobject_cast<QQuickItem *>(named(window, "attachmentCaption"));
      if (!background || !caption || caption->mapToScene({0,0}).x() - background->mapToScene({0,0}).x() < 20) { fail("attachment preview has no usable content padding"); return; }
      if (fixture.endsWith(".mp4", Qt::CaseInsensitive)) {
        if (caption->property("text").toString() != QStringLiteral("synthetic video caption")) { fail("video review did not retain the caption draft"); return; }
        auto *player = named(window, "videoReviewPlayer");
        auto *video = qobject_cast<QQuickItem *>(named(window, "videoReviewOutput"));
        auto *audioLoader = named(window, "videoReviewAudioOutputLoader");
        if (!player || !video || !audioLoader || !video->isVisible() ||
            player->property("playbackState").toInt() != 0 ||
            !dialog->property("videoAudioMuted").toBool() ||
            audioLoader->property("active").toBool()) {
          fail("video review must start paused, muted, and without audio output"); return;
        }
        if (c.attachment().value("video_codec").toString() != QStringLiteral("h264") ||
            c.attachment().value("mime").toString() != QStringLiteral("video/mp4")) {
          fail("video review metadata must report validated MP4/H.264"); return;
        }
        if (!invoke(window, "videoReviewPlayPause", "clicked")) { fail("video review Play control"); return; }
        state->stage = 31;
        return;
      }
      if (fixture.endsWith(".ogg", Qt::CaseInsensitive)) {
        auto *audioCard = named(window, "audioAttachmentReviewCard");
        auto *audioTitle = named(window, "audioAttachmentReviewTitle");
        auto *audioDetails = named(window, "audioAttachmentReviewDetails");
        auto *player = named(window, "audioReviewPlayer");
        auto *outputLoader = named(window, "audioReviewAudioOutputLoader");
        auto *captionError = named(window, "audioCaptionError");
        auto *send = named(window, "attachmentSend");
        auto *modeOptions = named(window, "attachmentModeOptions");
        if (!audioCard || !audioCard->property("visible").toBool() || !audioTitle || !audioDetails ||
            !player || !outputLoader || !captionError || !send || !modeOptions) {
          fail("audio attachment review controls are incomplete"); return;
        }
        if (!player->property("errorString").toString().isEmpty()) {
          fail("Qt Multimedia could not load staged OGG/Opus: " + player->property("errorString").toString()); return;
        }
        if (player->property("duration").toLongLong() <= 0) return;
        if (audioTitle->property("text").toString() != QStringLiteral("Voice note") ||
            !audioDetails->property("text").toString().contains("Validated OGG/Opus") ||
            !audioDetails->property("text").toString().contains("mono 48 kHz") ||
            !dialog->property("localAudioSource").toString().startsWith("file:///")) {
          fail("audio review must identify validated local OGG/Opus voice-note metadata"); return;
        }
        if (caption->property("text").toString() != QStringLiteral("synthetic audio caption") ||
            !captionError->property("visible").toBool() || send->property("enabled").toBool() ||
            modeOptions->property("visible").toBool() ||
            player->property("playbackState").toInt() != 0 ||
            !dialog->property("audioMuted").toBool() || outputLoader->property("active").toBool()) {
          fail("voice-note review must reject captions and start paused with no armed audio output"); return;
        }
        capture("attachment-audio-review");
        if (!invoke(window, "audioReviewPlayPause", "clicked")) { fail("audio review Play control"); return; }
        state->stage = 33;
        return;
      }
      if (fixture.endsWith(".pdf", Qt::CaseInsensitive)) {
        auto *documentCard = named(window, "documentAttachmentCard");
        auto *documentLabel = named(window, "documentAttachmentLabel");
        auto *description = named(window, "attachmentPreparationDescription");
        auto *modeButton = named(window, "documentModeButton");
        auto *send = named(window, "attachmentSend");
        if (!documentCard || !documentCard->property("visible").toBool() || !documentLabel ||
            documentLabel->property("text").toString() != QStringLiteral("Document · original bytes") ||
            !description || !description->property("text").toString().contains(QStringLiteral("No recompression")) ||
            !modeButton || !modeButton->property("primary").toBool() || !send || !send->property("enabled").toBool()) {
          fail("document review must show original bytes and an enabled send action"); return;
        }
        if (caption->property("text").toString() != QStringLiteral("synthetic document caption") ||
            c.attachment().value("mode").toString() != QStringLiteral("document") ||
            c.attachment().value("mime").toString() != QStringLiteral("application/pdf")) {
          fail("document review lost its type, MIME or caption"); return;
        }
        capture("attachment-document-review");
        if (!invoke(window, "attachmentSend", "clicked")) { fail("send document through native review"); return; }
        state->stage = 4;
        return;
      }
      capture("attachment-preview");
      invoke(window, "attachmentPreviewDialog", "close");
      c.send();
      state->stage = 4;
    } else if (state->stage == 31) {
      auto *dialog = named(window, "attachmentPreviewDialog");
      auto *player = named(window, "videoReviewPlayer");
      if (!dialog || !dialog->property("opened").toBool() || !player) return;
      if (player->property("playbackState").toInt() != 1) {
        if (!player->property("errorString").toString().isEmpty()) { fail("Qt Multimedia could not decode validated MP4: " + player->property("errorString").toString()); return; }
        return;
      }
      auto *video = named(window, "videoReviewOutput");
      auto *sink = video ? qobject_cast<QVideoSink *>(video->property("videoSink").value<QObject *>()) : nullptr;
      if (!sink) { fail("video review output has no QVideoSink"); return; }
      const auto frame = sink->videoFrame();
      if (!frame.isValid()) return;
      if (frame.size() != QSize(96, 64)) { fail("video review decoded frame size changed"); return; }
      if (!dialog->property("videoAudioMuted").toBool() || named(window, "videoReviewAudioOutput")) {
        fail("video review Play must not enable audio"); return;
      }
      if (!invoke(window, "videoReviewPlayPause", "clicked")) { fail("video review Pause control"); return; }
      state->stage = 32;
    } else if (state->stage == 32) {
      auto *dialog = named(window, "attachmentPreviewDialog");
      auto *player = named(window, "videoReviewPlayer");
      if (!dialog || !player || player->property("playbackState").toInt() != 2) return;
      if (!invoke(window, "videoReviewMute", "clicked")) { fail("video review Unmute control"); return; }
      if (dialog->property("videoAudioMuted").toBool() || !named(window, "videoReviewAudioOutput")) {
        fail("explicit Unmute did not arm the audio output"); return;
      }
      invoke(window, "videoReviewMute", "clicked");
      capture("attachment-video-review");
      invoke(window, "attachmentPreviewDialog", "close");
      c.send();
      state->stage = 4;
    } else if (state->stage == 33) {
      auto *player = named(window, "audioReviewPlayer");
      auto *dialog = named(window, "attachmentPreviewDialog");
      auto *outputLoader = named(window, "audioReviewAudioOutputLoader");
      if (!player || !dialog || !outputLoader) return;
      if (!player->property("errorString").toString().isEmpty()) { fail("audio review decode failed: " + player->property("errorString").toString()); return; }
      if (player->property("playbackState").toInt() != 1 || player->property("position").toLongLong() <= 0) return;
      if (!dialog->property("audioMuted").toBool() || dialog->property("audioOutputArmed").toBool() || outputLoader->property("active").toBool()) {
        fail("audio review Play must remain muted without arming an output"); return;
      }
      if (!invoke(window, "audioReviewPlayPause", "clicked")) { fail("audio review Pause control"); return; }
      state->stage = 34;
    } else if (state->stage == 34) {
      auto *player = named(window, "audioReviewPlayer");
      if (!player || player->property("playbackState").toInt() != 2) return;
      if (!invoke(window, "audioReviewMute", "clicked")) { fail("audio review Unmute control"); return; }
      state->stage = 35;
    } else if (state->stage == 35) {
      auto *dialog = named(window, "attachmentPreviewDialog");
      auto *output = named(window, "audioReviewAudioOutput");
      auto *outputLoader = named(window, "audioReviewAudioOutputLoader");
      auto *send = named(window, "attachmentSend");
      auto *captionError = named(window, "audioCaptionError");
      if (!dialog || !outputLoader || !send || !captionError) return;
      if (!output) return;
      if (dialog->property("audioMuted").toBool() || !dialog->property("audioOutputArmed").toBool() ||
          !outputLoader->property("active").toBool() || named(window, "audioReviewPlayer")->property("playbackState").toInt() != 2) {
        fail("audio review Unmute must arm output only after explicit action"); return;
      }
      if (!invoke(window, "audioReviewMute", "clicked")) { fail("audio review Mute control"); return; }
      if (!dialog->property("audioMuted").toBool() || !output->property("muted").toBool()) return;
      c.setDraft(QString());
      if (captionError->property("visible").toBool() || !send->property("enabled").toBool()) return;
      capture("attachment-audio-review-ready");
      if (!invoke(window, "attachmentSend", "clicked")) { fail("send voice note through native review"); return; }
      state->stage = 36;
    } else if (state->stage == 36) {
      if (c.sending()) return;
      QVariantMap audioRow;
      for (int i = 0; i < c.messageCount(); ++i) {
        const auto row = c.messages()->row(i);
        const auto attachment = row.value("attachment").toMap();
        if (row.value("outgoing").toBool() && row.value("status").toString() == "demo_sent" &&
            attachment.value("kind").toString() == "audio") {
          audioRow = row;
          break;
        }
      }
      if (audioRow.isEmpty()) {
        if (!c.error().isEmpty()) fail("voice-note send failed: " + c.error());
        return;
      }
      const auto audio = audioRow.value("attachment").toMap();
      if (!audioRow.value("body").toString().isEmpty() ||
          audio.value("mode").toString() != "voice-note" ||
          audio.value("mime").toString() != "audio/ogg; codecs=opus" ||
          audio.value("audio_codec").toString() != "opus" ||
          !audio.value("is_voice_note").toBool() ||
          audio.value("duration").toInt() <= 0 ||
          !audio.value("url").toString().startsWith("file:///")) {
        fail("demo voice-note send projection lost typed metadata or caption guard"); return;
      }
      qInfo() << "Native audio send PASS: OGG/Opus picker, validation, explicit voice-note identity, native review, caption guard and local persistence";
      state->stage = 4;
    } else if (state->stage == 4 && !c.sending()) {
      capture("media");
      if (fixture.isEmpty()) { fail("media interaction fixture is required"); return; }
      if (fixture.endsWith(".mp4", Qt::CaseInsensitive)) {
        bool foundVideo = false;
        for (int i = 0; i < c.messageCount(); ++i) {
          const auto row = c.messages()->row(i);
          const auto attachment = row.value("attachment").toMap();
          if (row.value("body").toString() == QStringLiteral("synthetic video caption") &&
              attachment.value("kind").toString() == QStringLiteral("video")) foundVideo = true;
        }
        if (!foundVideo) { fail("video send did not retain its caption in the timeline"); return; }
      }
      if (fixture.endsWith(".pdf", Qt::CaseInsensitive)) {
        bool foundDocument = false;
        for (int i = 0; i < c.messageCount(); ++i) {
          const auto row = c.messages()->row(i);
          const auto attachment = row.value("attachment").toMap();
          if (row.value("body").toString() == QStringLiteral("synthetic document caption") &&
              row.value("status").toString() == QStringLiteral("demo_sent") &&
              attachment.value("kind").toString() == QStringLiteral("document") &&
              attachment.value("mime").toString() == QStringLiteral("application/pdf") &&
              attachment.value("name").toString() == QFileInfo(fixture).fileName()) foundDocument = true;
        }
        if (!foundDocument) { fail("document send did not retain typed metadata and caption in the local timeline"); return; }
        qInfo() << "Native document send PASS: typed PDF metadata and caption persisted to local demo timeline";
        state->stage = 7;
        return;
      }
      QQuickItem *mediaButton = nullptr;
      std::function<void(QQuickItem *)> findVisible = [&](QQuickItem *item) {
        if (item->objectName() == "openMediaButton" && item->isVisible()) mediaButton = item;
        for (auto *child : item->childItems()) findVisible(child);
      };
      findVisible(window->contentItem());
      if (!mediaButton) return;
      if (!QMetaObject::invokeMethod(mediaButton, "clicked")) { fail("ready media has no clickable preview"); return; }
      state->stage = 5;
    } else if (state->stage == 5) {
      auto *viewer = named(window, "mediaViewer");
      if (!viewer || !viewer->property("opened").toBool()) return;
      const bool isVideoFixture = fixture.endsWith(".mp4", Qt::CaseInsensitive);
      const bool isAudioFixture = fixture.endsWith(".wav", Qt::CaseInsensitive) || fixture.endsWith(".ogg", Qt::CaseInsensitive);
      if (!isVideoFixture && !isAudioFixture && viewer->property("loadStatus").toInt() != 1) return;
      auto *player = named(window, "viewerPlayer");
      if (!player || named(window, "viewerAudioOutput") ||
          named(window, "viewerAudioInput")) { fail("native playback object or capture boundary"); return; }
      if (player->property("playbackState").toInt() != 0 ||
          !viewer->property("audioMuted").toBool() ||
          viewer->property("audioOutputArmed").toBool()) {
        fail("player must remain stopped with no audio device until explicit unmute"); return;
      }
      if (isAudioFixture && player->property("duration").toLongLong() <= 0) {
        const auto error = player->property("errorString").toString();
        if (!error.isEmpty()) { fail("Qt Multimedia could not load local OGG/Opus: " + error); return; }
        return;
      }
      if (isAudioFixture) {
        auto *panel = named(window, "audioReviewPanel");
        auto *title = named(window, "audioReviewTitle");
        if (!viewer->property("isAudio").toBool() || !panel || !panel->property("visible").toBool() || !title ||
            title->property("text").toString() != QStringLiteral("Voice note · local file") ||
            !viewer->property("localSource").toString().startsWith(QStringLiteral("file:///"))) {
          fail("ready PTT audio review must identify a local voice note"); return;
        }
        capture("voice-note-review");
        if (!invoke(window, "viewerPlayPause", "clicked")) { fail("voice-note Play control"); return; }
        state->stage = 52;
        return;
      }
      if (isVideoFixture) {
        auto *video = qobject_cast<QQuickItem *>(named(window, "viewerVideoOutput"));
        auto *audioLoader = named(window, "viewerAudioOutputLoader");
        auto *progress = named(window, "viewerProgress");
        if (!viewer->property("isVideo").toBool() || !video || !video->isVisible() ||
            !audioLoader || audioLoader->property("active").toBool() ||
            player->property("duration").toLongLong() <= 0 ||
            !progress || !progress->property("enabled").toBool()) {
          if (!player->property("errorString").toString().isEmpty()) { fail("Qt Multimedia could not load validated MP4: " + player->property("errorString").toString()); return; }
          return;
        }
        const qreal displayedRatio = viewer->property("naturalWidth").toReal() / qMax(qreal(1), viewer->property("naturalHeight").toReal());
        if (qAbs(displayedRatio - qreal(96) / 64) > 0.01) { fail("video aspect ratio changed"); return; }
        capture("media-viewer");
        if (!invoke(window, "viewerPlayPause", "clicked")) { fail("video viewer Play control"); return; }
        state->stage = 50;
        return;
      }
      const QSize sourceSize = QImageReader(fixture).size();
      const qreal displayedRatio = viewer->property("naturalWidth").toReal() / qMax(qreal(1), viewer->property("naturalHeight").toReal());
      const qreal sourceRatio = qreal(sourceSize.width()) / qMax(1, sourceSize.height());
      if (sourceSize.isValid() && qAbs(displayedRatio - sourceRatio) > 0.01) { fail("media aspect ratio changed"); return; }
      QImageReader animationReader(fixture);
      if (viewer->property("animated").toBool() && animationReader.imageCount() > 1) {
        auto *motion = named(window, "viewerAnimatedImage");
        if (!motion || !motion->property("currentFrame").isValid()) { fail("GIF frame state missing"); return; }
        viewer->setProperty("qaInitialFrame", motion->property("currentFrame"));
      }
      capture("media-viewer");
      if (!invoke(window, "mediaZoomIn", "clicked")) { fail("viewer zoom action"); return; }
      state->stage = 6;
    } else if (state->stage == 50) {
      auto *player = named(window, "viewerPlayer");
      if (!player) return;
      if (player->property("playbackState").toInt() != 1) {
        if (!player->property("errorString").toString().isEmpty()) { fail("native video playback failed: " + player->property("errorString").toString()); return; }
        return;
      }
      auto *video = named(window, "viewerVideoOutput");
      auto *sink = video ? qobject_cast<QVideoSink *>(video->property("videoSink").value<QObject *>()) : nullptr;
      if (!sink) { fail("media viewer output has no QVideoSink"); return; }
      const auto frame = sink->videoFrame();
      if (!frame.isValid()) return;
      if (frame.size() != QSize(96, 64)) { fail("media viewer decoded frame size changed"); return; }
      capture("media-viewer-playing");
      const QImage renderedScreenshot = window->grabWindow();
      QImage renderedVideoCrop;
      QString colorDiagnostics;
      const bool renderedBars = renderedSmpteBars(
          renderedScreenshot, window, qobject_cast<QQuickItem *>(video),
          frame.size(), &renderedVideoCrop, &colorDiagnostics);
      const QString videoCropPath = captureDir + "/media-viewer-playing-video.png";
      if (renderedVideoCrop.isNull() || !renderedVideoCrop.save(videoCropPath)) {
        fail("could not save the cropped rendered video area"); return;
      }
      if (!renderedBars) {
        fail("rendered video crop did not show the synthetic SMPTE color bars: " +
             colorDiagnostics);
        return;
      }
      qInfo() << "Native rendered video SMPTE color bars PASS:" << videoCropPath
              << colorDiagnostics;
      if (named(window, "viewerAudioOutput") || !named(window, "viewerAudioOutputLoader") ||
          !named(window, "mediaViewer")->property("audioMuted").toBool()) {
        fail("explicit Play must remain muted until Unmute"); return;
      }
      if (!invoke(window, "viewerPlayPause", "clicked")) { fail("video viewer Pause control"); return; }
      state->stage = 51;
    } else if (state->stage == 51) {
      auto *player = named(window, "viewerPlayer");
      auto *viewer = named(window, "mediaViewer");
      if (!player || !viewer || player->property("playbackState").toInt() != 2) return;
      if (!invoke(window, "viewerAudioMute", "clicked")) { fail("viewer Unmute control"); return; }
      if (viewer->property("audioMuted").toBool() || !named(window, "viewerAudioOutput")) {
        fail("explicit Unmute did not arm the native audio output"); return;
      }
      invoke(window, "viewerAudioMute", "clicked");
      invoke(window, "mediaViewerClose", "clicked");
      state->stage = 7;
    } else if (state->stage == 52) {
      auto *player = named(window, "viewerPlayer");
      auto *viewer = named(window, "mediaViewer");
      if (!player || !viewer) return;
      if (!player->property("errorString").toString().isEmpty()) {
        fail("native OGG/Opus playback failed: " + player->property("errorString").toString()); return;
      }
      if (player->property("playbackState").toInt() != 1 ||
          player->property("position").toLongLong() <= 0) return;
      if (!viewer->property("audioMuted").toBool() ||
          viewer->property("audioOutputArmed").toBool() ||
          named(window, "viewerAudioOutput")) {
        fail("voice-note playback must remain muted without an audio device until explicit unmute"); return;
      }
      if (!invoke(window, "viewerPlayPause", "clicked")) { fail("voice-note Pause control"); return; }
      state->stage = 53;
    } else if (state->stage == 53) {
      auto *player = named(window, "viewerPlayer");
      auto *viewer = named(window, "mediaViewer");
      if (!player || !viewer || player->property("playbackState").toInt() != 2) return;
      if (!viewer->property("audioMuted").toBool() ||
          viewer->property("audioOutputArmed").toBool() ||
          named(window, "viewerAudioOutput") ||
          player->property("position").toLongLong() <= 0) {
        fail("voice-note playback must pause with audio muted and no device armed"); return;
      }
      if (!invoke(window, "viewerAudioMute", "clicked")) { fail("voice-note Unmute control"); return; }
      state->stage = 54;
    } else if (state->stage == 54) {
      auto *player = named(window, "viewerPlayer");
      auto *viewer = named(window, "mediaViewer");
      auto *output = named(window, "viewerAudioOutput");
      if (!player || !viewer || !output) return;
      if (viewer->property("audioMuted").toBool() ||
          !viewer->property("audioOutputArmed").toBool() ||
          player->property("playbackState").toInt() != 2) {
        fail("voice-note Unmute must arm output only while paused"); return;
      }
      if (!invoke(window, "viewerAudioMute", "clicked")) { fail("voice-note Mute control"); return; }
      state->stage = 55;
    } else if (state->stage == 55) {
      auto *player = named(window, "viewerPlayer");
      auto *viewer = named(window, "mediaViewer");
      auto *output = named(window, "viewerAudioOutput");
      if (!player || !viewer || !output) return;
      if (!viewer->property("audioMuted").toBool() ||
          !output->property("muted").toBool() ||
          player->property("playbackState").toInt() != 2) {
        fail("voice-note Mute must silence the armed output while paused"); return;
      }
      capture("voice-note-playback");
      qInfo() << "Native voice-note playback PASS: silent OGG/Opus decode, Play/Pause, initially muted, explicit Unmute/Mute without starting playback";
      invoke(window, "mediaViewerClose", "clicked");
      state->stage = 7;
    } else if (state->stage == 6) {
      auto *viewer = named(window, "mediaViewer");
      auto *viewport = named(window, "mediaViewport");
      if (!viewer || viewer->property("zoom").toDouble() <= 1 || !viewport ||
          viewport->property("contentWidth").toDouble() < viewport->property("width").toDouble()) { fail("viewer does not zoom/pan"); return; }
      if (viewer->property("qaInitialFrame").isValid()) {
        auto *motion = named(window, "viewerAnimatedImage");
        if (motion->property("currentFrame").toInt() == viewer->property("qaInitialFrame").toInt()) return;
      }
      invoke(window, "mediaFit", "clicked");
      if (!qFuzzyCompare(viewer->property("zoom").toDouble(), 1.0)) { fail("viewer fit"); return; }
      invoke(window, "mediaViewerClose", "clicked");
      state->stage = 7;
    } else if (state->stage == 7) {
      if (named(window, "mediaViewer")->property("visible").toBool()) return;
      auto *wallpaper = qobject_cast<QQuickItem *>(named(window, "chatWallpaper"));
      if (!wallpaper || wallpaper->width() <= 0 || wallpaper->height() <= 0 ||
          wallpaper->width() > window->width() || !wallpaper->property("aspectPreserving").toBool()) {
        fail("compact chat wallpaper geometry"); return;
      }
      capture("wallpaper-compact");
      invoke(window, "conversationInfoDialog", "open");
      state->stage = 80;
    } else if (state->stage == 80) {
      auto *dialog = named(window, "conversationInfoDialog");
      if (!dialog || !dialog->property("opened").toBool() || c.routeLoading()) return;
      if (!c.routeError().isEmpty()) { fail("cached chat info: " + c.routeError()); return; }
      const auto data = c.routeData();
      const auto contactDetails = data.value("contact_details").toMap();
      auto *title = named(window, "cachedConversationTitle");
      auto *scope = named(window, "cachedConversationScope");
      auto *provenance = named(window, "cachedDetailsProvenance");
      auto *phone = named(window, "cachedContactPhone");
      auto *about = named(window, "cachedContactAbout");
      auto *presence = named(window, "cachedContactPresence");
      if (data.value("id").toString() != c.selectedId() ||
          data.value("is_group").toBool() ||
          data.value("details_provenance").toString() != "demo_synthetic" ||
          contactDetails.value("display_name").toString() != "Alex Morgan" ||
          contactDetails.value("phone").toString() != "+1-202-555-0147" ||
          data.value("media_count").toInt() < 1 || !title || !scope || !provenance || !phone || !about || !presence ||
          title->property("text").toString() != data.value("title").toString() ||
          title->property("textFormat").toInt() != 0 ||
          !scope->property("text").toString().contains("Synthetic local") ||
          !provenance->property("text").toString().contains("Synthetic demo") ||
          !phone->property("text").toString().contains("+1-202-555-0147") ||
          !about->property("text").toString().contains("Synthetic demo contact") ||
          !presence->property("visible").toBool() ||
          !presence->property("text").toString().contains("Synthetic presence · Online")) {
        fail("native synthetic contact details/disclosure missing"); return;
      }
      capture("cached-conversation-info");
      invoke(window, "conversationInfoDialog", "close");
      state->stage = 81;
    } else if (state->stage == 81) {
      if (named(window, "conversationInfoDialog")->property("visible").toBool()) return;
      c.selectChat(QStringLiteral("demo:studio"));
      if (c.selectedId() != QStringLiteral("demo:studio")) return;
      if (!invoke(window, "conversationInfoDialog", "open")) { fail("synthetic group info open action"); return; }
      state->stage = 84;
    } else if (state->stage == 84) {
      auto *dialog = named(window, "conversationInfoDialog");
      if (!dialog || !dialog->property("opened").toBool() || c.routeLoading()) return;
      if (!c.routeError().isEmpty()) { fail("synthetic group info: " + c.routeError()); return; }
      const auto data = c.routeData();
      const auto groupDetails = data.value("group_details").toMap();
      const auto participants = groupDetails.value("participants").toList();
      auto *description = named(window, "cachedGroupDescription");
      auto *participantCount = named(window, "cachedGroupParticipantCount");
      auto *participantList = named(window, "cachedGroupParticipants");
      if (data.value("id").toString() != QStringLiteral("demo:studio") ||
          !data.value("is_group").toBool() ||
          data.value("details_provenance").toString() != QStringLiteral("demo_synthetic") ||
          groupDetails.value("description").toString() != QStringLiteral("Synthetic group for local UI tests.") ||
          participants.size() != 3 || !description || !participantCount || !participantList ||
          !description->property("text").toString().contains("Synthetic group") ||
          !participantCount->property("text").toString().contains("3") ||
          !participantList->property("text").toString().contains("Alex Morgan") ||
          !participantList->property("text").toString().contains("Maya Chen") ||
          !participantList->property("text").toString().contains("Synthetic presence · Typing") ||
          !participantList->property("text").toString().contains("Synthetic presence · Last seen") ||
          !participantList->property("text").toString().contains("Sam Rivera")) {
        fail("native synthetic group participants/disclosure missing"); return;
      }
      capture("cached-group-info");
      auto *presenceInfo = named(window, "cachedConversationInfo");
      if (!presenceInfo) { fail("synthetic presence expiry test seam missing"); return; }
      presenceInfo->setProperty("presenceClockMs", presenceInfo->property("presenceClockMs").toDouble() + 60000.0);
      state->stage = 86;
    } else if (state->stage == 86) {
      auto *participantList = named(window, "cachedGroupParticipants");
      if (!participantList || !participantList->property("text").toString().contains("Synthetic presence · snapshot expired")) {
        fail("expired synthetic presence snapshot remained current"); return;
      }
      capture("cached-group-info-expired");
      qInfo() << "Native presence expiry PASS: stale online/typing snapshots are labeled expired";
      invoke(window, "conversationInfoDialog", "close");
      state->stage = 85;
    } else if (state->stage == 85) {
      if (named(window, "conversationInfoDialog")->property("visible").toBool()) return;
      if (!c.hasCapability(QStringLiteral("group.create"))) { fail("demo group creation capability missing"); return; }
      if (!invoke(window, "chatListOptionsButton", "clicked")) { fail("chat-list options for synthetic group creation"); return; }
      state->stage = 200;
    } else if (state->stage == 200) {
      if (!named(window, "createSyntheticGroupAction")) { fail("synthetic group creation menu action missing"); return; }
      if (!invoke(window, "createSyntheticGroupAction", "triggered")) { fail("synthetic group creation menu action"); return; }
      state->stage = 201;
    } else if (state->stage == 201) {
      auto *dialog = named(window, "createSyntheticGroupDialog");
      auto *title = named(window, "syntheticGroupTitleInput");
      auto *description = named(window, "syntheticGroupDescriptionInput");
      auto *maya = named(window, "syntheticGroupMemberMaya");
      if (!dialog || !dialog->property("opened").toBool() || !title || !description || !maya) return;
      title->setProperty("text", "Synthetic meetup");
      description->setProperty("text", "Created only in the local demo profile.");
      maya->setProperty("checked", true);
      auto *confirm = qobject_cast<QQuickItem *>(named(window, "confirmSyntheticGroupCreation"));
      if (!confirm || !confirm->isEnabled()) { fail("synthetic group create enablement did not follow selected participants"); return; }
      if (!invoke(window, "confirmSyntheticGroupCreation", "clicked")) { fail("synthetic group create confirmation"); return; }
      state->stage = 202;
    } else if (state->stage == 202) {
      if (!c.routeLoading() && c.routeMethod() == QStringLiteral("group.create") && !c.routeError().isEmpty()) {
        fail("synthetic group create: " + c.routeError()); return;
      }
      if (!c.selectedId().startsWith(QStringLiteral("demo:group:"))) {
        if (!c.routeLoading() && c.routeMethod() != QStringLiteral("group.create"))
          fail(QString("synthetic group create did not select a new group (method=%1 error=%2)").arg(c.routeMethod(), c.routeError()));
        return;
      }
      state->managedGroupId = c.selectedId();
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("created synthetic group info: " + c.routeError()); return; }
      const auto data = c.routeData();
      const auto details = data.value("group_details").toMap();
      auto *title = named(window, "managedGroupTitleInput");
      auto *description = named(window, "managedGroupDescriptionInput");
      if (data.value("id").toString() != state->managedGroupId ||
          details.value("managed").toBool() != true ||
          details.value("membership_state").toString() != QStringLiteral("joined") ||
          details.value("description").toString() != QStringLiteral("Created only in the local demo profile.") ||
          !title || !description) { fail("synthetic group create projection and local-only disclosure"); return; }
      title->setProperty("text", "Updated meetup");
      description->setProperty("text", "Edited locally; no WhatsApp group was changed.");
      if (!invoke(window, "saveManagedGroupDetails", "clicked")) { fail("synthetic group details save action"); return; }
      state->stage = 203;
    } else if (state->stage == 203) {
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("synthetic group update: " + c.routeError()); return; }
      const auto data = c.routeData();
      const auto details = data.value("group_details").toMap();
      if (data.value("title").toString() != QStringLiteral("Updated meetup") ||
          details.value("description").toString() != QStringLiteral("Edited locally; no WhatsApp group was changed.")) {
        fail("synthetic group edits did not return through chat.info"); return;
      }
      auto *picker = named(window, "syntheticGroupParticipantChoice");
      if (!picker) { fail("synthetic participant selector missing"); return; }
      picker->setProperty("currentIndex", 0);
      if (!invoke(window, "addSyntheticGroupParticipant", "clicked")) { fail("synthetic participant add action"); return; }
      state->stage = 204;
    } else if (state->stage == 204) {
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("synthetic participant add: " + c.routeError()); return; }
      const auto participants = c.routeData().value("group_details").toMap().value("participants").toList();
      bool alexFound = false;
      for (const auto &entry : participants)
        if (entry.toMap().value("display_name").toString() == QStringLiteral("Alex Morgan")) alexFound = true;
      if (participants.size() != 3 || !alexFound) { fail("synthetic participant add projection"); return; }
      if (!invoke(window, "promoteSyntheticParticipant_demo_participant_alex", "clicked")) { fail("synthetic participant promote action"); return; }
      state->stage = 205;
    } else if (state->stage == 205) {
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("synthetic participant promotion: " + c.routeError()); return; }
      const auto participants = c.routeData().value("group_details").toMap().value("participants").toList();
      bool alexAdmin = false;
      for (const auto &entry : participants)
        if (entry.toMap().value("display_name").toString() == QStringLiteral("Alex Morgan") && entry.toMap().value("role").toString() == QStringLiteral("Admin")) alexAdmin = true;
      if (!alexAdmin) { fail("synthetic participant role promotion projection"); return; }
      if (!invoke(window, "leaveSyntheticGroup", "clicked")) { fail("local synthetic group leave action"); return; }
      state->stage = 206;
    } else if (state->stage == 206) {
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("local synthetic group leave: " + c.routeError()); return; }
      const auto details = c.routeData().value("group_details").toMap();
      if (details.value("membership_state").toString() != QStringLiteral("left") || details.value("can_manage").toBool()) {
        fail("local leave state must disable group management"); return;
      }
      if (!invoke(window, "joinSyntheticGroup", "clicked")) { fail("local synthetic group rejoin action"); return; }
      state->stage = 207;
    } else if (state->stage == 207) {
      if (c.routeLoading() || c.routeMethod() != QStringLiteral("chat.info")) return;
      if (!c.routeError().isEmpty()) { fail("local synthetic group rejoin: " + c.routeError()); return; }
      if (c.routeData().value("group_details").toMap().value("membership_state").toString() != QStringLiteral("joined")) {
        fail("local rejoin state did not persist"); return;
      }
      auto *infoScroll = qobject_cast<QQuickItem *>(named(window, "conversationInfoScroll"));
      auto *scrollContent = infoScroll ? infoScroll->property("contentItem").value<QObject *>() : nullptr;
      if (!infoScroll || !scrollContent ||
          scrollContent->property("contentHeight").toDouble() <= infoScroll->height()) {
        fail("synthetic group controls must be reachable in the conversation-info scroll view"); return;
      }
      const double maxContentY = scrollContent->property("contentHeight").toDouble() - infoScroll->height();
      if (maxContentY <= 0) {
        fail("conversation-info scroll view has no scrollable group actions"); return;
      }
      scrollContent->setProperty("contentY", maxContentY);
      if (qAbs(scrollContent->property("contentY").toDouble() - maxContentY) > 1) {
        fail("conversation-info scroll view did not move to the group actions"); return;
      }
      auto *leave = qobject_cast<QQuickItem *>(named(window, "leaveSyntheticGroup"));
      const QRectF leaveRect = leave ? leave->mapRectToItem(infoScroll, leave->boundingRect()) : QRectF{};
      if (!leave || !leave->isVisible() || !leaveRect.intersects(QRectF(0, 0, infoScroll->width(), infoScroll->height()))) {
        fail("local group membership action is not reachable after scrolling"); return;
      }
      capture("managed-group");
      capture("managed-group-actions");
      qInfo() << "Native group management PASS: explicit synthetic create, edit, participant add/promote, local leave/rejoin; no live group mutation";
      invoke(window, "conversationInfoDialog", "close");
      state->stage = 208;
    } else if (state->stage == 208) {
      if (named(window, "conversationInfoDialog")->property("visible").toBool()) return;
      c.selectChat(QStringLiteral("demo:alex"));
      if (c.selectedId() != QStringLiteral("demo:alex")) return;
      state->stage = 209;
      return;
    } else if (state->stage == 209) {
      if (c.selectedId() != QStringLiteral("demo:alex")) return;
      const int previewIndex = c.messages()->indexOf(QStringLiteral("seed-0-119"));
      if (previewIndex < 0) return;
      const auto record = c.messages()->row(previewIndex);
      const auto preview = record.value("link_preview").toMap();
      const QStringList expectedProjectionKeys{"description", "site", "title"};
      if (record.value("chat_id").toString() != QStringLiteral("demo:alex") ||
          !record.value("body").toString().contains(QStringLiteral("https://example.invalid/native-demo")) ||
          preview.keys() != expectedProjectionKeys ||
          preview.value("site").toString() != QStringLiteral("example.invalid") ||
          preview.value("title").toString() != QStringLiteral("Native demo preview") ||
          preview.value("description").toString() != QStringLiteral("Synthetic message metadata only. No page or thumbnail is fetched.")) {
        fail("seeded link preview projection did not survive daemon, RPC and native model"); return;
      }
      QQuickItem *previewBubble = nullptr;
      std::function<void(QQuickItem *)> findPreviewBubble = [&](QQuickItem *item) {
        if (item->objectName() == "messageBubble") {
          const auto value = item->property("record");
          const auto message = value.canConvert<QJSValue>() ? value.value<QJSValue>().toVariant().toMap() : value.toMap();
          if (message.value("id").toString() == QStringLiteral("seed-0-119")) previewBubble = item;
        }
        for (auto *child : item->childItems()) findPreviewBubble(child);
      };
      findPreviewBubble(window->contentItem());
      if (!previewBubble) return;
      auto *timeline = qobject_cast<QQuickItem *>(named(window, "messageTimeline"));
      auto *card = namedBelow(previewBubble, "linkPreviewCard");
      auto *site = card ? namedBelow(card, "linkPreviewSite") : nullptr;
      auto *title = card ? namedBelow(card, "linkPreviewTitle") : nullptr;
      auto *description = card ? namedBelow(card, "linkPreviewDescription") : nullptr;
      auto *privacy = card ? namedBelow(card, "linkPreviewPrivacy") : nullptr;
      if (!timeline || !card || !card->isVisible() || !site || !title || !description || !privacy ||
          site->property("text").toString() != QStringLiteral("example.invalid") ||
          title->property("text").toString() != QStringLiteral("Native demo preview") ||
          description->property("text").toString() != QStringLiteral("Synthetic message metadata only. No page or thumbnail is fetched.") ||
          privacy->property("text").toString() != QStringLiteral("Page and thumbnail not fetched") ||
          site->property("textFormat").toInt() != int(Qt::PlainText) ||
          title->property("textFormat").toInt() != int(Qt::PlainText) ||
          description->property("textFormat").toInt() != int(Qt::PlainText)) {
        fail("native link preview card must render bounded PlainText metadata and disclose the no-fetch policy"); return;
      }
      const QRectF cardRect = card->mapRectToItem(timeline, card->boundingRect());
      if (!cardRect.intersects(QRectF(0, 0, timeline->width(), timeline->height()))) return;
      capture("link-preview-card");
      qInfo() << "Native link preview PASS: exact synthetic message projection, visible PlainText card, explicit no-fetch disclosure";
      if (!QMetaObject::invokeMethod(window, "openMediaLibrary")) { fail("cached media open action"); return; }
      state->stage = 82;
    } else if (state->stage == 82) {
      auto *drawer = named(window, "mediaLibraryDrawer");
      if (!drawer || !drawer->property("opened").toBool() || c.routeLoading()) return;
      if (!c.routeError().isEmpty()) { fail("shared media library: " + c.routeError()); return; }
      if (qEnvironmentVariableIsSet("WHATSFEATHER_TEST_GLOBAL_MEDIA_LIBRARY")) {
        auto *scope = qobject_cast<QQuickItem *>(named(window, "sharedMediaScopeSelector"));
        if (!scope) {
          fail("All chats / This conversation scope selector is missing"); return;
        }
        state->mediaRpcMethods.clear();
        QObject::connect(&c, &Controller::rpcRequestSent, &c,
                         [state](const QString &method) { state->mediaRpcMethods.append(method); });
        if (scope->property("currentIndex").toInt() != 0) {
          scope->setProperty("currentIndex", 0);
          if (!QMetaObject::invokeMethod(scope, "activated", Q_ARG(int, 0))) {
            fail("All chats scope could not be selected"); return;
          }
        }
        state->stage = 3000;
        return;
      }
      const auto items = c.routeData().value("messages").toList();
      if (c.routeData().value("chat_id").toString() != c.selectedId() || items.isEmpty()) {
        fail("shared media list must contain persisted selected-chat attachment"); return;
      }
      QQuickItem *row = nullptr;
      std::function<void(QQuickItem *)> findRow = [&](QQuickItem *item) {
        if (item->objectName() == "sharedMediaRow" && item->isVisible()) row = item;
        for (auto *child : item->childItems()) findRow(child);
      };
      findRow(window->contentItem());
      if (!row) return;
      const auto record = row->property("record").toMap();
      state->mediaTarget = record.value("id").toString();
      if (record.value("chat_id").toString() != c.selectedId() || state->mediaTarget.isEmpty()) {
        fail("shared media native row lost its exact identity"); return;
      }
      capture("shared-media-library");
      if (!QMetaObject::invokeMethod(row, "clicked")) { fail("shared media reveal action"); return; }
      state->stage = 83;
    } else if (state->stage == 83) {
      auto *drawer = named(window, "mediaLibraryDrawer");
      if (!drawer || drawer->property("visible").toBool() || c.loading()) return;
      if (qEnvironmentVariableIsSet("WHATSFEATHER_TEST_GLOBAL_MEDIA_LIBRARY")) {
        if (c.selectedId() != state->mediaChat) return;
        const int index = c.messages()->indexOf(state->mediaTarget);
        if (index < 0) return;
        const auto target = c.messages()->row(index);
        if (target.value("chat_id").toString() != state->mediaChat ||
            target.value("body").toString() != QStringLiteral("Caption from Maya")) {
          fail("cross-chat media reveal resolved the duplicate ID in the wrong conversation"); return;
        }
        const int historyRequest = state->mediaRpcMethods.indexOf(QStringLiteral("message.list"));
        const int contextRequest = state->mediaRpcMethods.indexOf(QStringLiteral("message.context"));
        if (historyRequest < 0 || contextRequest <= historyRequest ||
            state->mediaRpcMethods.contains(QStringLiteral("message.download"))) {
          fail("cross-chat reveal must wait for selected history and never request attachment download"); return;
        }
        qInfo() << "Native global media library PASS: exact cross-chat identity, reveal after history transition, no download";
      } else {
        if (c.messages()->indexOf(state->mediaTarget) < 0) return;
      }
      qInfo() << "Native local hubs PASS: synthetic contact/group presence and typing snapshots, explicit expiry, local-only disclosure, persisted selected-chat media, exact reveal without download";
      if (fixture.endsWith(".pdf", Qt::CaseInsensitive)) {
        qInfo() << "Native document PASS: native selection, original-byte review, caption and local persistence";
      } else if (fixture.endsWith(".mp4", Qt::CaseInsensitive)) {
        qInfo() << "Native video PASS: selection, review, muted playback, explicit audio, send and viewer controls";
      } else {
        qInfo() << "Native product PASS: deterministic timeline, typed unsupported-content fallback, incoming voice-note label, capability-gated contact/media hubs, settings fit, profile account view, muted native media controls, media click/zoom/fit/close";
      }
      timer->stop();
      app.quit();
    } else if (state->stage == 3000) {
      if (c.routeMethod() != QStringLiteral("media.library") || c.routeLoading()) return;
      if (!c.routeError().isEmpty()) { fail("global media library: " + c.routeError()); return; }
      const auto entries = c.routeData().value("entries").toList();
      if (entries.isEmpty() || c.routeData().contains("chat_id") ||
          !c.routeData().value("disclosure").toString().contains(QStringLiteral("not a complete history"))) {
        fail("global media result must be unselected, explicitly scoped, and honestly disclosed"); return;
      }
      QVariantMap selectedEntry;
      for (const auto &item : entries) {
        const auto candidate = item.toMap();
        if (candidate.value("chat_id").toString() == QStringLiteral("demo:maya") &&
            candidate.value("id").toString() == QStringLiteral("media-library-collision")) {
          selectedEntry = candidate;
          break;
        }
      }
      if (selectedEntry.isEmpty() ||
          selectedEntry.value("chat_title").toString() != QStringLiteral("Maya Chen") ||
          selectedEntry.value("body").toString() != QStringLiteral("Caption from Maya")) {
        fail("global library lost the exact cached chat/message identity or caption"); return;
      }
      const auto attachment = selectedEntry.value("attachment").toMap();
      const auto safeEntry = QJsonDocument::fromVariant(selectedEntry).toJson(QJsonDocument::Compact);
      for (const auto &forbidden : {"url", "media_key", "direct_path", "prepared_path", "source_url"}) {
        if (attachment.contains(QString::fromLatin1(forbidden)) || safeEntry.contains("PRIVATE_DESCRIPTOR")) {
          fail("global media projection exposed a private descriptor or URL"); return;
        }
      }
      QQuickItem *row = nullptr;
      QVariantMap record;
      std::function<void(QQuickItem *)> findRow = [&](QQuickItem *item) {
        if (item->objectName() == "sharedMediaRow" && item->isVisible()) {
          const auto value = item->property("record");
          const auto candidate = value.canConvert<QJSValue>() ? value.value<QJSValue>().toVariant().toMap() : value.toMap();
          if (candidate.value("chat_id").toString() == QStringLiteral("demo:maya") &&
              candidate.value("id").toString() == QStringLiteral("media-library-collision")) {
            row = item;
            record = candidate;
          }
        }
        for (auto *child : item->childItems()) findRow(child);
      };
      findRow(window->contentItem());
      auto *scopeLabel = named(window, "sharedMediaScope");
      auto *title = row ? namedBelow(row, "sharedMediaEntryName") : nullptr;
      auto *chatTitle = row ? namedBelow(row, "sharedMediaChatTitle") : nullptr;
      auto *caption = row ? namedBelow(row, "sharedMediaCaption") : nullptr;
      if (!row || !row->property("enabled").toBool() || !scopeLabel ||
          !scopeLabel->property("text").toString().contains(QStringLiteral("All chats")) ||
          record != selectedEntry || !title || !chatTitle || !caption ||
          title->property("textFormat").toInt() != int(Qt::PlainText) ||
          chatTitle->property("textFormat").toInt() != int(Qt::PlainText) ||
          caption->property("textFormat").toInt() != int(Qt::PlainText) ||
          chatTitle->property("text").toString() != QStringLiteral("Maya Chen") ||
          caption->property("text").toString() != QStringLiteral("Caption from Maya")) {
        fail("global native row must expose the exact enabled identity and render untrusted text as PlainText"); return;
      }
      state->mediaTarget = QStringLiteral("media-library-collision");
      state->mediaChat = QStringLiteral("demo:maya");
      state->mediaRpcMethods.clear();
      capture("shared-media-library-global");
      if (!QMetaObject::invokeMethod(row, "clicked")) { fail("global media reveal action"); return; }
      state->stage = 83;
      return;
    }
  });
  timer->start();
}

void runPolishTest(QGuiApplication &app, QQuickWindow *window, Controller &c,
                   const QString &fixture, const QString &stickerFixture,
                   const QStringList &albumFixtures) {
  const QStringList organizationActionNames{"archiveChatAction", "pinChatAction",
                                           "muteChatAction", "markUnreadChatAction",
                                           "favoriteChatAction"};
  const QStringList organizationInitialLabels{"Archive chat", "Pin chat", "Mute chat",
                                              "Mark unread", "Add to Favorites"};
  const QStringList organizationEnabledLabels{"Unarchive chat", "Unpin chat", "Unmute chat",
                                              "Clear unread mark", "Remove from Favorites"};
  const QStringList organizationFields{"archived", "pinned", "muted", "marked_unread",
                                       "favorite"};
  const bool verifyPersistedExpressions =
      qEnvironmentVariableIsSet("WHATSFEATHER_TEST_EXPECT_PERSISTED_EXPRESSIONS");
  struct State {
    int stage = 0, ticks = 0;
    QString target;
    QString stickerTarget;
    QString searchSelectedId;
    QString organizationChatId;
    QString forwardSourceChatId;
    QString forwardDestination;
    QString forwardBody;
    int organizationAction = 0;
    int messagesBeforeLocalSticker = -1;
    int albumInitialMessages = -1;
    bool closingFind = false;
    bool revealedAfterLatest = false;
    bool revealedAfterForward = false;
  };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(100);
  QObject::connect(
      timer, &QTimer::timeout, &app,
      [&, window, fixture, stickerFixture, albumFixtures, state, timer,
       organizationActionNames, organizationInitialLabels,
       organizationEnabledLabels, organizationFields, verifyPersistedExpressions] {
        auto capture = [&](const QString &name) {
          auto dir = qEnvironmentVariable("WHATSFEATHER_QA_CAPTURE_DIR");
          if (!dir.isEmpty() && c.demo()) {
            QDir().mkpath(dir);
            window->grabWindow().save(dir + "/" + name + ".png");
          }
        };
        auto fail = [&](const QString &message) {
          qCritical() << "Native polish FAIL at stage" << state->stage
                      << message;
          timer->stop();
          app.exit(1);
        };
        if (++state->ticks > 300) {
          if (state->stage == 57) {
            const auto attachment = c.attachment();
            fail(QString("album send timed out: sending=%1 attachment_state=%2 attachment_kind=%3 error=%4 messages=%5")
                     .arg(c.sending())
                     .arg(attachment.value("state").toString())
                     .arg(attachment.value("kind").toString())
                     .arg(c.error())
                     .arg(c.messageCount()));
          } else {
            fail(QString("timed out at stage %1 tick %2")
                     .arg(state->stage)
                     .arg(state->ticks));
          }
          return;
        }
        if (state->stage == -1) {
          auto *picker = named(window, "expressionPicker");
          auto *tabs = named(window, "expressionTabs");
          auto *category = named(window, "emojiCategory");
          if (!picker || !picker->property("opened").toBool() || !tabs || !category)
            return;
          tabs->setProperty("currentIndex", 0);
          category->setProperty("currentIndex", 1); // Recent
          state->stage = -2;
          return;
        } else if (state->stage == -2) {
          auto *category = named(window, "emojiCategory");
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!category || !grid || !first ||
              category->property("currentText").toString() != "Recent" ||
              grid->property("count").toInt() != 2 ||
              first->property("text").toString() != QString::fromUtf8("☕")) {
            fail(QString("persisted recent emojis did not render after profile restart (category=%1 count=%2 first=%3 recent=%4)")
                     .arg(category ? category->property("currentText").toString() : QStringLiteral("missing"))
                     .arg(grid ? grid->property("count").toInt() : -1)
                     .arg(first ? first->property("text").toString() : QStringLiteral("missing"))
                     .arg(c.recentEmojis().join(",")));
            return;
          }
          capture("emoji-recent-restored");
          category->setProperty("currentIndex", 2); // Favorites
          state->stage = -3;
          return;
        } else if (state->stage == -3) {
          auto *category = named(window, "emojiCategory");
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!category || !grid || !first ||
              category->property("currentText").toString() != "Favorites" ||
              grid->property("count").toInt() != 1 ||
              first->property("text").toString() != QString::fromUtf8("👎")) {
            fail("persisted favorite emoji did not render after profile restart");
            return;
          }
          capture("emoji-favorites-restored");
          qInfo() << "Native expression preferences restored PASS: recent and favorite emoji rendered after profile restart";
          timer->stop();
          app.exit(0);
          return;
        } else if (state->stage == 0 && c.messageCount() > 0) {
          if (verifyPersistedExpressions) {
            if (c.recentEmojis() != QStringList{QString::fromUtf8("☕"), QString::fromUtf8("👎")} ||
                c.favoriteEmojis() != QStringList{QString::fromUtf8("👎")} ||
                !c.expressionStoreError().isEmpty()) {
              fail("profile restart did not restore expression preferences into Controller");
              return;
            }
            if (!invoke(window, "expressionButton", "clicked")) {
              fail("expression picker could not open after profile restart");
              return;
            }
            state->stage = -1;
            return;
          }
          auto *surface = qobject_cast<QQuickItem *>(
              named(window, "floatingComposerSurface"));
          auto *pane =
              qobject_cast<QQuickItem *>(named(window, "conversationPane"));
          auto *rail =
              qobject_cast<QQuickItem *>(named(window, "navigationRail"));
          if (!surface || !pane || !rail || !rail->isVisible()) {
            fail("missing rail/composer");
            return;
          }
          auto *voiceRecordButton = qobject_cast<QQuickItem *>(
              named(window, "voiceNoteButton"));
          auto *voiceCancelButton = qobject_cast<QQuickItem *>(
              named(window, "cancelVoiceNoteButton"));
          auto *voiceStatus = qobject_cast<QQuickItem *>(
              named(window, "voiceRecordingStatus"));
          if (!voiceRecordButton || !voiceRecordButton->isVisible() ||
              !voiceRecordButton->isEnabled() || voiceRecordButton->width() < 40 ||
              voiceRecordButton->height() < 40 ||
              voiceRecordButton->property("iconName").toString() != "mic" ||
              voiceRecordButton->property("label").toString() !=
                  QStringLiteral("Record a voice note") ||
              !voiceCancelButton || voiceCancelButton->isVisible() ||
              !voiceStatus || voiceStatus->isVisible() || c.voiceRecording() ||
              c.voiceEncoding() || !c.voiceRecordingError().isEmpty()) {
            fail("idle voice-note controls must be available without activating the microphone");
            return;
          }
          auto *historyAction = named(window, "requestOlderHistoryAction");
          if (!historyAction ||
              historyAction->property("text").toString() !=
                  QStringLiteral("Request up to 50 older messages from phone") ||
              historyAction->property("enabled").toBool()) {
            fail("remote history action must state its bound and stay disabled in demo mode");
            return;
          }
          auto *appRoot = named(window, "appWindow");
          if (!appRoot) appRoot = window;
          appRoot->setProperty("chatFilter", "unread");
          c.chats()->mergeTail({
              QVariantMap{{"id", "probe-community-unread"}, {"title", "Unread community child"},
                          {"is_group", true}, {"community_id", "probe-community"},
                          {"community_title", "Probe community"}, {"unread", 4}, {"favorite", false}},
              QVariantMap{{"id", "probe-community-favorite"}, {"title", "Favorite community child"},
                          {"is_group", true}, {"community_id", "probe-community"},
                          {"community_title", "Probe community"}, {"unread", 0}, {"favorite", true}},
              QVariantMap{{"id", "probe-community-empty"}, {"title", "Quiet community child"},
                          {"is_group", true}, {"community_id", "probe-empty"},
                          {"community_title", "Empty community"}, {"unread", 0}, {"favorite", false}}
          });
          auto folderMatches = [appRoot](const QString &id) {
            QVariant result;
            const QVariantMap folder{{"row_type", "community"}, {"community_id", id}};
            const bool invoked = QMetaObject::invokeMethod(
                appRoot, "communityRowMatches", Q_RETURN_ARG(QVariant, result),
                Q_ARG(QVariant, QVariant::fromValue(folder)));
            return invoked && result.toBool();
          };
          if (!folderMatches("probe-community") || folderMatches("probe-empty")) {
            fail("unread filter must retain only folders with matching descendants");
            return;
          }
          appRoot->setProperty("chatFilter", "favorites");
          if (!folderMatches("probe-community") || folderMatches("probe-empty")) {
            fail("favorite filter must retain only folders with matching descendants");
            return;
          }
          appRoot->setProperty("chatFilter", "unread");
          auto unreadMatches = [appRoot](const QVariantMap &record) {
            QVariant result;
            const bool invoked = QMetaObject::invokeMethod(
                appRoot, "chatMatches", Q_RETURN_ARG(QVariant, result),
                Q_ARG(QVariant, QVariant::fromValue(record)));
            return invoked && result.toBool();
          };
          if (!unreadMatches({{"marked_unread", true}, {"unread", 0}}) ||
              !unreadMatches({{"marked_unread", false}, {"unread", 3}}) ||
              unreadMatches({{"marked_unread", false}, {"unread", 0}})) {
            fail("unread filter must distinguish marked_unread from the unread count");
            return;
          }
          auto pos = surface->mapToScene({0, 0});
          auto parentPos = pane->mapToScene({0, 0});
          if (pos.x() - parentPos.x() < 10 ||
              window->height() - pos.y() - surface->height() < 10 ||
              surface->property("radius").toInt() < 20) {
            fail("composer is not floating/inset");
            return;
          }
          c.chats()->mergeTail({
              QVariantMap{{"id", "probe-unread-count"}, {"title", "Unread count"},
                          {"initials", "U"}, {"color", "#526a63"}, {"preview", "synthetic"},
                          {"timestamp", 1700000000000LL}, {"unread", 7}, {"marked_unread", false}},
              QVariantMap{{"id", "probe-manual-unread"}, {"title", "Manual unread"},
                          {"initials", "M"}, {"color", "#526a63"}, {"preview", "synthetic"},
                          {"timestamp", 1700000000000LL}, {"unread", 0}, {"marked_unread", true}}
          });
          appRoot->setProperty("chatFilter", "unread");
          state->stage = 10;
          return;
        } else if (state->stage == 10) {
          auto *countBadge = named(window, "chatUnreadCountBadge_probe-unread-count");
          auto *manualBadge = named(window, "chatManualUnreadBadge_probe-manual-unread");
          if (!countBadge || !manualBadge)
            return;
          if (!countBadge->property("visible").toBool() ||
              countBadge->property("text").toString() != "7" ||
              !manualBadge->property("visible").toBool() ||
              manualBadge->property("text").toString() != "Unread") {
            fail("inbox must show count and manual unread mark as separate badges");
            return;
          }
          auto *filterRow = named(window, "unreadFavoritesFilters");
          auto *unreadFilter = named(window, "unreadChatFilterButton");
          auto *favoriteFilter = named(window, "favoriteChatFilterButton");
          if (!filterRow || !filterRow->property("visible").toBool() ||
              !unreadFilter || !unreadFilter->property("visible").toBool() ||
              !favoriteFilter || !favoriteFilter->property("visible").toBool()) {
            fail("local Unread/Favorites filter controls are unavailable");
            return;
          }
          auto *appRoot = named(window, "appWindow");
          if (!appRoot) appRoot = window;
          if (appRoot->property("chatFilter").toString() != "unread" ||
              !QMetaObject::invokeMethod(unreadFilter, "clicked") ||
              appRoot->property("chatFilter").toString() != "all") {
            fail("Unread filter button did not clear the active filter");
            return;
          }
          if (!QMetaObject::invokeMethod(favoriteFilter, "clicked") ||
              appRoot->property("chatFilter").toString() != "favorites") {
            fail("Favorites filter button did not activate its local filter");
            return;
          }
          auto favoriteMatches = [appRoot](const QVariantMap &record) {
            QVariant result;
            const bool invoked = QMetaObject::invokeMethod(
                appRoot, "chatMatches", Q_RETURN_ARG(QVariant, result),
                Q_ARG(QVariant, QVariant::fromValue(record)));
            return invoked && result.toBool();
          };
          if (!favoriteMatches({{"favorite", true}}) ||
              favoriteMatches({{"favorite", false}})) {
            fail("Favorites filter did not apply only to locally favorited chats");
            return;
          }
          if (!QMetaObject::invokeMethod(favoriteFilter, "clicked") ||
              appRoot->property("chatFilter").toString() != "all") {
            fail("Favorites filter button did not restore the All list");
            return;
          }
          window->setProperty("chatFilter", "all");
          auto *navChats = named(window, "navChats");
          auto *navFind = named(window, "navFind");
          auto *chatSearch = named(window, "chatSearch");
          if (!navChats || !navFind || !chatSearch ||
              !navChats->property("visible").toBool() ||
              !navFind->property("enabled").toBool()) {
            fail("navigation rail Chats/Find controls missing or disabled");
            return;
          }
          state->searchSelectedId = c.selectedId();
          if (state->searchSelectedId.isEmpty()) {
            fail("chat search requires a selected demo conversation");
            return;
          }
          chatSearch->setProperty("text", "mAyA");
          state->stage = 88;
          return;
        } else if (state->stage == 88) {
          if (c.chats()->rowCount() != 1)
            return;
          const auto row = c.chats()->row(0);
          auto *chatList = named(window, "chatList");
          if (row.value("id").toString() != "demo:maya" ||
              row.value("title").toString() != "Maya Chen") {
            fail("case-insensitive chat search returned the wrong conversation");
            return;
          }
          if (c.selectedId() != state->searchSelectedId) {
            fail("chat search changed the selected conversation");
            return;
          }
          if (!chatList || chatList->property("count").toInt() != 1)
            return;
          named(window, "chatSearch")->setProperty("text", "Neighborhood Circle");
          state->stage = 89;
        } else if (state->stage == 89) {
          if (c.chats()->rowCount() != 1)
            return;
          const auto row = c.chats()->row(0);
          if (row.value("id").toString() != "demo:studio")
            return;
          auto *chatList = named(window, "chatList");
          auto *folder = named(window, "communityFolderRow");
          auto *child = named(window, "communityChildRow");
          if (!chatList || chatList->property("count").toInt() != 2 ||
              !folder || !child)
            return;
          auto recordMap = [](QObject *item) {
            const auto value = item->property("record");
            return value.canConvert<QJSValue>()
                       ? value.value<QJSValue>().toVariant().toMap()
                       : value.toMap();
          };
          if (recordMap(folder).value("title").toString() != "Neighborhood Circle" ||
              recordMap(child).value("id").toString() != "demo:studio") {
            fail("community-name search did not reveal its matching group");
            return;
          }
          if (c.selectedId() != state->searchSelectedId) {
            fail("community search changed the selected conversation");
            return;
          }
          named(window, "chatSearch")->setProperty("text", "no-such-chat");
          state->stage = 94;
        } else if (state->stage == 94) {
          if (c.chats()->rowCount() != 0)
            return;
          auto *chatList = named(window, "chatList");
          auto *emptyState = named(window, "chatListEmptyState");
          if (!emptyState) {
            fail("chat search empty-state label test hook missing");
            return;
          }
          if (!chatList || chatList->property("count").toInt() != 0 ||
              !emptyState->property("visible").toBool() ||
              emptyState->property("text").toString() != "No conversations found")
            return;
          if (c.selectedId() != state->searchSelectedId) {
            fail("no-result chat search changed the selected conversation");
            return;
          }
          if (!invoke(window, "navFind", "clicked")) {
            fail("rail Find route");
            return;
          }
          state->stage = 90;
          return;
        } else if (state->stage == 90) {
          auto *drawer = named(window, "findDrawer");
          if (!drawer || !drawer->property("opened").toBool())
            return;
          if (!invoke(window, "navChats", "clicked")) {
            fail("rail Chats route");
            return;
          }
          state->stage = 91;
        } else if (state->stage == 91) {
          auto *drawer = named(window, "findDrawer");
          auto *chatSearch = named(window, "chatSearch");
          if (drawer && drawer->property("opened").toBool())
            return;
          if (!chatSearch || !chatSearch->property("text").toString().isEmpty()) {
            fail("Chats route did not close Find and clear chat search");
            return;
          }
          state->stage = 92;
        } else if (state->stage == 92) {
          if (c.chats()->rowCount() != 4)
            return;
          auto *chatList = named(window, "chatList");
          if (!chatList || chatList->property("count").toInt() != 4)
            return;
          if (c.chats()->indexOf(state->searchSelectedId) < 0 ||
              c.selectedId() != state->searchSelectedId) {
            fail("clearing chat search did not restore chats without changing selection");
            return;
          }
          if (!invoke(window, "chatFilterGroupsButton", "clicked")) {
            fail("Groups chat filter button");
            return;
          }
          state->stage = 95;
          return;
        } else if (state->stage == 95) {
          if (!window->property("groupsOnly").toBool()) {
            fail("Groups filter did not activate");
            return;
          }
          if (c.chats()->rowCount() != 4)
            return;
          auto *chatList = named(window, "chatList");
          auto *folder = named(window, "communityFolderRow");
          if (!chatList || chatList->property("count").toInt() != 1 || !folder ||
              !folder->property("visible").toBool())
            return;
          const auto folderRecord = folder->property("record");
          const auto folderMap = folderRecord.canConvert<QJSValue>()
                                     ? folderRecord.value<QJSValue>().toVariant().toMap()
                                     : folderRecord.toMap();
          if (folderMap.value("title").toString() != "Neighborhood Circle" ||
              c.selectedId() != state->searchSelectedId) {
            fail("Groups filter hid the community group or changed selection");
            return;
          }
          if (!invoke(window, "chatFilterAllButton", "clicked")) {
            fail("All chat filter button");
            return;
          }
          state->stage = 96;
          return;
        } else if (state->stage == 96) {
          if (window->property("groupsOnly").toBool()) {
            fail("All filter did not deactivate Groups-only mode");
            return;
          }
          if (c.chats()->rowCount() != 4)
            return;
          auto *chatList = named(window, "chatList");
          if (!chatList || chatList->property("count").toInt() != 4)
            return;
          if (c.selectedId() != state->searchSelectedId) {
            fail("All filter did not restore the list without changing selection");
            return;
          }
          state->organizationChatId = QStringLiteral("demo:alex");
          if (c.chats()->indexOf(state->organizationChatId) < 0) {
            fail("chat organization fixture is missing its target chat");
            return;
          }
          auto *row = chatRowForId(window, state->organizationChatId);
          if (!row)
            return;
          auto *actions = namedBelow(row, "chatActionsButton");
          if (!actions || !QMetaObject::invokeMethod(actions, "clicked")) {
            fail("chat management menu button did not open");
            return;
          }
          state->organizationAction = 0;
          state->stage = 97;
          return;
        } else if (state->stage == 97) {
          auto *menu = named(window, "chatManagementMenu");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (state->organizationAction == organizationActionNames.size()) {
            for (int i = 0; i < organizationActionNames.size(); ++i) {
              auto *item = named(window, organizationActionNames.at(i));
              if (!item) {
                fail("chat organization menu item test hook missing");
                return;
              }
              if (item->property("text").toString() != organizationEnabledLabels.at(i)) {
                fail("chat management menu did not reflect persisted local flags");
                return;
              }
            }
            if (!QMetaObject::invokeMethod(menu, "close")) {
              fail("chat management menu could not close");
              return;
            }
            state->stage = 100;
            return;
          }
          const int action = state->organizationAction;
          auto *item = named(window, organizationActionNames.at(action));
          if (!item) {
            fail("chat organization menu item test hook missing");
            return;
          }
          if (item->property("text").toString() != organizationInitialLabels.at(action)) {
            fail("chat management action label does not match current flag state");
            return;
          }
          if (action > 0) {
            auto *previous = named(window, organizationActionNames.at(action - 1));
            if (!previous || previous->property("text").toString() !=
                                 organizationEnabledLabels.at(action - 1)) {
              fail("chat management menu did not retain the previous local flag");
              return;
            }
          }
          if (!QMetaObject::invokeMethod(item, "triggered")) {
            fail("chat organization action did not trigger");
            return;
          }
          state->stage = 98;
          return;
        } else if (state->stage == 98) {
          const int index = c.chats()->indexOf(state->organizationChatId);
          if (index < 0) {
            fail("chat organization target disappeared from the local roster");
            return;
          }
          const auto row = c.chats()->row(index);
          for (int i = 0; i <= state->organizationAction; ++i) {
            if (!row.value(organizationFields.at(i)).toBool())
              return;
          }
          if (row.value("organization_provenance").toString() != "local_only" ||
              c.chats()->rowCount() != 4 || c.selectedId() != state->searchSelectedId) {
            fail("chat organization changed selection/source roster or lost local-only provenance");
            return;
          }
          auto *menu = named(window, "chatManagementMenu");
          if (menu && menu->property("visible").toBool())
            return;
          ++state->organizationAction;
          state->stage = 99;
          return;
        } else if (state->stage == 99) {
          auto *row = chatRowForId(window, state->organizationChatId);
          auto *actions = namedBelow(row, "chatActionsButton");
          if (!row || !actions)
            return;
          if (!QMetaObject::invokeMethod(actions, "clicked")) {
            fail("chat management menu could not reopen for the next local action");
            return;
          }
          state->stage = 97;
          return;
        } else if (state->stage == 100) {
          auto *menu = named(window, "chatManagementMenu");
          if (menu && menu->property("visible").toBool())
            return;
          if (!invoke(window, "expressionButton", "clicked")) {
            fail("expression button");
            return;
          }
          state->stage = 1;
        } else if (state->stage == 1) {
          auto *picker = named(window, "expressionPicker");
          auto *tabs = named(window, "expressionTabs");
          auto *category = named(window, "emojiCategory");
          auto *search = named(window, "emojiSearch");
          if (!picker || !picker->property("opened").toBool() || !tabs || !category || !search)
            return;
          capture("emoji-panel");
          category->setProperty("currentIndex", 4); // People
          state->stage = 12;
        } else if (state->stage == 12) {
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!grid || !first || grid->property("count").toInt() != 8 ||
              first->property("text").toString() != QString::fromUtf8("👍")) {
            fail("People category did not filter the emoji grid");
            return;
          }
          named(window, "emojiSearch")->setProperty("text", "thumbs down");
          state->stage = 13;
        } else if (state->stage == 13) {
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!grid || !first || grid->property("count").toInt() != 1 ||
              first->property("text").toString() != QString::fromUtf8("👎")) {
            fail("emoji search did not filter within the selected category");
            return;
          }
          if (!QMetaObject::invokeMethod(first, "clicked") ||
              !c.draft().contains(QString::fromUtf8("👎"))) {
            fail("filtered emoji selection did not insert into the draft");
            return;
          }
          auto *favorite = named(window, "emojiFavoriteToggle");
          if (!favorite || !QMetaObject::invokeMethod(favorite, "clicked")) {
            fail("favorite control unavailable after selecting emoji");
            return;
          }
          named(window, "emojiCategory")->setProperty("currentIndex", 2); // Favorites
          state->stage = 14;
        } else if (state->stage == 14) {
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!grid || !first || grid->property("count").toInt() != 1 ||
              first->property("text").toString() != QString::fromUtf8("👎")) {
            fail("Favorites category did not retain selected emoji");
            return;
          }
          named(window, "emojiCategory")->setProperty("currentIndex", 1); // Recent
          state->stage = 15;
        } else if (state->stage == 15) {
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!grid || !first || grid->property("count").toInt() != 1 ||
              first->property("text").toString() != QString::fromUtf8("👎")) {
            fail("Recent category did not retain selected emoji");
            return;
          }
          named(window, "emojiCategory")->setProperty("currentIndex", 0); // All
          named(window, "emojiSearch")->setProperty("text", "coffee");
          state->stage = 16;
        } else if (state->stage == 16) {
          auto *grid = named(window, "emojiGrid");
          auto *first = named(window, "firstEmoji");
          if (!grid || !first || grid->property("count").toInt() != 1 ||
              first->property("text").toString() != QString::fromUtf8("☕")) {
            fail("search by emoji name did not filter the full catalog");
            return;
          }
          if (!QMetaObject::invokeMethod(first, "clicked") ||
              !c.draft().contains(QString::fromUtf8("👎")) ||
              !c.draft().contains(QString::fromUtf8("☕"))) {
            fail("search result selection did not insert into the draft");
            return;
          }
          named(window, "emojiSearch")->setProperty("text", "");
          auto *tabs = named(window, "expressionTabs");
          tabs->setProperty("currentIndex", 2); // Stickers
          state->stage = 17;
        } else if (state->stage == 17) {
          auto *notice = named(window, "stickerCapabilityNotice");
          auto *choose = named(window, "chooseStickerButton");
          if (!notice || !choose) {
            fail("static sticker controls are missing");
            return;
          }
          const auto noticeText = notice->property("text").toString().toLower();
          if (!noticeText.contains("static") || !noticeText.contains("webp") ||
              !noticeText.contains("animated") || !noticeText.contains("profile-local")) {
            fail("sticker panel must describe static WebP support and limits");
            return;
          }
          if (!c.capabilitiesReady() || !choose->property("enabled").toBool())
            return;
          capture("sticker-panel");
          if (!QMetaObject::invokeMethod(choose, "clicked")) {
            fail("choose sticker action");
            return;
          }
          state->stage = 80;
        } else if (state->stage == 80) {
          auto *filePicker = named(window, "attachmentFilePicker");
          auto *appRoot = named(window, "appWindow");
          if (!appRoot)
            appRoot = window;
          if (!filePicker) {
            fail("sticker file picker object missing");
            return;
          }
          const auto filters = filePicker->property("nameFilters").toStringList();
          if (appRoot->property("attachmentMode").toString() != "sticker" ||
              !filters.join(" ").contains("webp", Qt::CaseInsensitive)) {
            fail("sticker picker did not select sticker mode and WebP filter");
            return;
          }
          if (filePicker->property("visible").toBool())
            QMetaObject::invokeMethod(filePicker, "close");
          c.setDraft(QString());
          c.stageAttachmentAs(QUrl::fromLocalFile(stickerFixture), "sticker");
          state->stage = 81;
        } else if (state->stage == 81) {
          const auto meta = c.attachment();
          if (meta.value("state").toString() == "preparing")
            return;
          if (meta.value("state").toString() != "ready") {
            fail("sticker staging failed: " + meta.value("error").toString());
            return;
          }
          if (meta.value("kind").toString() != "sticker" ||
              meta.value("mime").toString() != "image/webp" ||
              meta.value("width").toInt() != 512 ||
              meta.value("height").toInt() != 512 ||
              meta.value("size").toLongLong() <= 0 ||
              meta.value("size").toLongLong() > 100000) {
            fail("sticker metadata did not match the static WebP contract");
            return;
          }
          auto *dialog = named(window, "attachmentPreviewDialog");
          if (!dialog || !QMetaObject::invokeMethod(dialog, "open")) {
            fail("open sticker review dialog");
            return;
          }
          state->stage = 82;
        } else if (state->stage == 82) {
          auto *dialog = named(window, "attachmentPreviewDialog");
          if (!dialog || !dialog->property("opened").toBool())
            return;
          auto *reviewImage = named(window, "stickerReviewImage");
          auto *modeOptions = named(window, "attachmentModeOptions");
          auto *caption = named(window, "attachmentCaption");
          auto *send = named(window, "attachmentSend");
          auto *packName = named(window, "stickerPackName");
          auto *savePack = named(window, "saveStickerToLocalPack");
          auto *localOnly = named(window, "stickerPackLocalOnly");
          if (!reviewImage || !modeOptions || !caption || !send || !packName ||
              !savePack || !localOnly) {
            fail("sticker review controls are incomplete");
            return;
          }
          if (reviewImage->property("status").toInt() == 2)
            return;
          const auto localOnlyText = localOnly->property("text").toString();
          if (reviewImage->property("status").toInt() != 1 ||
              modeOptions->property("visible").toBool() ||
              !caption->property("text").toString().isEmpty() ||
              !caption->property("readOnly").toBool() ||
              caption->property("placeholderText").toString() != "Stickers cannot include captions" ||
              !send->property("enabled").toBool() ||
              !localOnlyText.contains("nothing is uploaded or sent") ||
              !localOnlyText.contains("reuse one")) {
            fail("sticker review must decode locally, disclose local-only pack storage, hide image modes, and make caption unavailability explicit");
            return;
          }
          capture("sticker-review");
          state->messagesBeforeLocalSticker = c.messageCount();
          packName->setProperty("text", "Synthetic pack");
          if (!savePack->property("enabled").toBool() ||
              !QMetaObject::invokeMethod(savePack, "clicked")) {
            fail("save-to-local-pack review action is unavailable");
            return;
          }
          state->stage = 821;
        } else if (state->stage == 821) {
          auto *saved = named(window, "stickerPackSaveResult");
          if (!saved || !saved->property("visible").toBool())
            return;
          const auto packs = c.localStickerPacks();
          if (packs.size() != 1 || c.sending() ||
              c.messageCount() != state->messagesBeforeLocalSticker) {
            fail("saving a sticker must create one local pack without sending it");
            return;
          }
          const auto pack = packs.first().toMap();
          const auto stickers = pack.value("stickers").toList();
          if (pack.value("name").toString() != "Synthetic pack" ||
              pack.value("count").toInt() != 1 || stickers.size() != 1) {
            fail("local sticker pack metadata is not bounded and readable");
            return;
          }
          const auto sticker = stickers.first().toMap();
          if (sticker.value("name").toString() != QFileInfo(stickerFixture).fileName()) {
            fail("local sticker pack must retain the readable source filename");
            return;
          }
          const auto copiedPath = QUrl(sticker.value("url").toString()).toLocalFile();
          const QFileInfo copiedInfo(copiedPath);
          if (!copiedInfo.isFile() || copiedInfo.isSymLink() ||
              !copiedPath.contains("/local-expressions/sticker-packs/") ||
              copiedInfo.absoluteFilePath() == QFileInfo(stickerFixture).absoluteFilePath()) {
            fail("local sticker pack must own a private profile copy, not the selected source path");
            return;
          }
          if (saved->property("text").toString() != "Saved to the local sticker pack.") {
            fail("local sticker save confirmation is missing");
            return;
          }
          capture("sticker-pack-review");
          c.setDraft("   ");
          state->stage = 83;
        } else if (state->stage == 83) {
          auto *warning = named(window, "stickerCaptionError");
          auto *send = named(window, "attachmentSend");
          auto *caption = named(window, "attachmentCaption");
          if (!warning || !send || !caption) {
            fail("sticker caption guard controls are missing");
            return;
          }
          if (!warning->property("visible").toBool() ||
              send->property("enabled").toBool() ||
              caption->property("readOnly").toBool()) {
            fail("whitespace-only sticker caption bypasses the caption guard or cannot be cleared");
            return;
          }
          c.setDraft("Synthetic sticker caption must be rejected");
          state->stage = 183;
        } else if (state->stage == 183) {
          auto *warning = named(window, "stickerCaptionError");
          auto *send = named(window, "attachmentSend");
          if (!warning || !send) {
            fail("sticker caption guard controls are missing");
            return;
          }
          if (!warning->property("visible").toBool() ||
              send->property("enabled").toBool()) {
            fail("non-empty sticker caption must be rejected before send");
            return;
          }
          c.setDraft(QString());
          state->stage = 84;
        } else if (state->stage == 84) {
          auto *send = named(window, "attachmentSend");
          if (!send || !send->property("enabled").toBool() || !c.draft().isEmpty())
            return;
          if (!QMetaObject::invokeMethod(send, "clicked")) {
            fail("send sticker via native review action");
            return;
          }
          state->stage = 85;
        } else if (state->stage == 85) {
          if (c.sending())
            return;
          QVariantMap stickerRow;
          for (int i = 0; i < c.messages()->rowCount(); ++i) {
            const auto row = c.messages()->row(i);
            const auto attachment = row.value("attachment").toMap();
            if (row.value("outgoing").toBool() &&
                attachment.value("kind").toString() == "sticker") {
              stickerRow = row;
              break;
            }
          }
          if (stickerRow.isEmpty())
            return;
          const auto attachment = stickerRow.value("attachment").toMap();
          if (!stickerRow.value("body").toString().isEmpty() ||
              attachment.value("mime").toString() != "image/webp" ||
              attachment.value("width").toInt() != 512 ||
              attachment.value("height").toInt() != 512 ||
              attachment.value("state").toString() != "ready") {
            fail("demo sticker send projection is not typed or caption-free");
            return;
          }
          state->stickerTarget = stickerRow.value("id").toString();
          auto *bubble = messageBubble(window, state->stickerTarget);
          auto *preview = namedBelow(bubble, "messageMediaPreview");
          if (!preview) {
            fail("sticker timeline preview is missing");
            return;
          }
          if (preview->property("status").toInt() == 2)
            return;
          if (preview->property("status").toInt() != 1) {
            fail("sticker timeline preview failed to decode");
            return;
          }
          capture("sticker-timeline");
          if (!invoke(window, "expressionButton", "clicked")) {
            fail("reopen expression picker after sticker send");
            return;
          }
          state->stage = 86;
        } else if (state->stage == 86) {
          auto *picker = named(window, "expressionPicker");
          auto *tabs = named(window, "expressionTabs");
          if (!picker || !picker->property("opened").toBool() || !tabs)
            return;
          tabs->setProperty("currentIndex", 2);
          state->stage = 861;
        } else if (state->stage == 861) {
          auto *grid = named(window, "localStickerGrid");
          auto *selector = named(window, "localStickerPackSelector");
          auto *disclosure = named(window, "localStickerPackDisclosure");
          auto *first = named(window, "firstLocalPackSticker");
          if (!grid || !selector || !disclosure || !first ||
              grid->property("count").toInt() != 1 ||
              grid->property("height").toDouble() < 70 ||
              selector->property("currentText").toString() != "Synthetic pack" ||
              !disclosure->property("text").toString().contains("not send it")) {
            fail("saved local sticker pack is not browsable with a clear no-send disclosure");
            return;
          }
          state->messagesBeforeLocalSticker = c.messageCount();
          capture("sticker-pack-panel");
          if (!QMetaObject::invokeMethod(first, "clicked")) {
            fail("local sticker selection did not open review");
            return;
          }
          state->stage = 862;
        } else if (state->stage == 862) {
          const auto meta = c.attachment();
          if (meta.value("state").toString() == "preparing")
            return;
          if (meta.value("state").toString() != "ready" ||
              meta.value("kind").toString() != "sticker" ||
              meta.value("mime").toString() != "image/webp" ||
              c.sending() || c.messageCount() != state->messagesBeforeLocalSticker) {
            fail("choosing a local pack sticker must stage it for review without sending");
            return;
          }
          const auto packs = c.localStickerPacks();
          const auto packStickers = packs.isEmpty()
                                        ? QVariantList{}
                                        : packs.first().toMap().value("stickers").toList();
          const auto expectedStickerName = packStickers.isEmpty()
                                               ? QString()
                                               : packStickers.first().toMap().value("name").toString();
          if (expectedStickerName.isEmpty() ||
              meta.value("display_name").toString() != expectedStickerName) {
            fail("local pack review must show the saved sticker name rather than its content hash");
            return;
          }
          auto *dialog = named(window, "attachmentPreviewDialog");
          if (!dialog || !QMetaObject::invokeMethod(dialog, "open")) {
            fail("local pack sticker review dialog did not open");
            return;
          }
          state->stage = 863;
        } else if (state->stage == 863) {
          auto *dialog = named(window, "attachmentPreviewDialog");
          auto *reviewImage = named(window, "stickerReviewImage");
          auto *send = named(window, "attachmentSend");
          if (!dialog || !dialog->property("opened").toBool() || !reviewImage || !send)
            return;
          if (reviewImage->property("status").toInt() == 2)
            return;
          if (reviewImage->property("status").toInt() != 1 ||
              !send->property("enabled").toBool() || c.sending() ||
              c.messageCount() != state->messagesBeforeLocalSticker) {
            fail("local sticker review must decode while leaving Send explicit and untouched");
            return;
          }
          capture("sticker-pack-local-review");
          if (!QMetaObject::invokeMethod(dialog, "cancel")) {
            fail("local pack sticker review could not be cancelled");
            return;
          }
          state->stage = 864;
        } else if (state->stage == 864) {
          auto *dialog = named(window, "attachmentPreviewDialog");
          if (dialog && dialog->property("visible").toBool())
            return;
          if (c.sending() || c.messageCount() != state->messagesBeforeLocalSticker) {
            fail("cancelling local pack sticker review must not dispatch a send");
            return;
          }
          qInfo() << "Native local sticker packs PASS: saved, browsed, reviewed, and cancelled without a send";
          if (!invoke(window, "expressionButton", "clicked")) {
            fail("reopen expression picker after local sticker review");
            return;
          }
          state->stage = 865;
        } else if (state->stage == 865) {
          auto *picker = named(window, "expressionPicker");
          auto *tabs = named(window, "expressionTabs");
          if (!picker || !picker->property("opened").toBool() || !tabs)
            return;
          tabs->setProperty("currentIndex", 1);
          state->stage = 2;
        } else if (state->stage == 2) {
          auto *browse = named(window, "browseGifButton");
          auto *providerLabel = named(window, "gifProviderName");
          auto *privacyNotice = named(window, "gifPrivacyNotice");
          auto *searchButton = named(window, "gifSearch");
          if (!browse || !browse->property("visible").toBool())
            return;
          if (named(window, "gifProviderSelector") || !providerLabel ||
              providerLabel->property("text").toString() != QStringLiteral("Wikimedia Commons") ||
              !privacyNotice || !privacyNotice->property("visible").toBool() ||
              !privacyNotice->property("text").toString().contains(QStringLiteral("only after you press Search")) ||
              !searchButton || searchButton->property("enabled").toBool()) {
            fail("GIF search must disclose its provider/privacy boundary and not advertise an unconfigured provider");
            return;
          }
          capture("gif-panel");
          invoke(window, "expressionPicker", "close");
          if (!invoke(window, "findMessagesButton", "clicked")) {
            fail("find button");
            return;
          }
          auto *search = named(window, "messageSearch");
          if (!search) {
            fail("find field");
            return;
          }
          search->setProperty("text", "Morning!");
          state->stage = 3;
        } else if (state->stage == 3 && c.searchResults()->rowCount() > 0) {
          auto *drawer = named(window, "findDrawer");
          if (!drawer || !drawer->property("opened").toBool())
            return;
          auto *pane =
              qobject_cast<QQuickItem *>(named(window, "conversationPane"));
          if (!pane ||
              pane->mapToScene({0, 0}).x() + pane->width() >
                  window->width() - drawer->property("width").toDouble() + 1) {
            fail(QString("search drawer covers conversation instead of "
                         "reflowing it: window=%1 paneX=%2 paneWidth=%3 "
                         "drawerWidth=%4 position=%5")
                     .arg(window->width())
                     .arg(pane ? pane->mapToScene({0, 0}).x() : -1)
                     .arg(pane ? pane->width() : -1)
                     .arg(drawer->property("width").toDouble())
                     .arg(drawer->property("position").toDouble()));
            return;
          }
          state->target = c.searchResults()
                              ->row(c.searchResults()->rowCount() - 1)
                              .value("id")
                              .toString();
          c.revealMessage(state->target);
          state->stage = 4;
        } else if (state->stage == 4 && !c.loading() &&
                   c.messages()->indexOf(state->target) >= 0) {
          if (!state->closingFind) {
            capture("search-panel");
            invoke(window, "findDrawer", "close");
            state->closingFind = true;
            return;
          }
          if (auto *drawer = named(window, "findDrawer");
              drawer && drawer->property("visible").toBool())
            return;
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button)
            return;
          if (!QMetaObject::invokeMethod(button, "clicked")) {
            fail("per-target message menu button");
            return;
          }
          state->stage = 5;
        } else if (state->stage == 5) {
          auto *menu = named(window, "messageMenu");
          if (!menu || !menu->property("visible").toBool())
            return;
          capture("message-menu");
          auto record = window->property("menuMessage");
          auto map = record.canConvert<QJSValue>()
                         ? record.value<QJSValue>().toVariant().toMap()
                         : record.toMap();
          if (map.value("id").toString() != state->target) {
            fail(QString("message menu selected %1 instead of search target %2")
                     .arg(map.value("id").toString(), state->target));
            return;
          }
          auto *bubble = messageBubble(window, state->target);
          auto *bodyItem = namedBelow(bubble, "messageBody");
          if (!bodyItem || !bodyItem->property("selectByMouse").toBool() ||
              !dragSelectText(window, bodyItem)) {
            fail("message body cannot select text with a mouse drag");
            return;
          }
          auto selectedValue = window->property("selectedMessageIds");
          auto selectedIds = selectedValue.canConvert<QJSValue>()
                                 ? selectedValue.value<QJSValue>().toVariant().toList()
                                 : selectedValue.toList();
          if (bodyItem->property("selectedText").toString().trimmed().isEmpty() ||
              window->property("selectionMode").toBool() ||
              !selectedIds.isEmpty()) {
            fail("selecting message text entered bulk-message selection mode");
            return;
          }
          if (!QMetaObject::invokeMethod(bodyItem, "deselect")) {
            fail("clear selected message text");
            return;
          }
          if (!invoke(window, "selectMessageAction", "triggered")) {
            fail("message-level Select action");
            return;
          }
          auto *selectionToolbar = named(window, "messageSelectionToolbar");
          selectedValue = window->property("selectedMessageIds");
          selectedIds = selectedValue.canConvert<QJSValue>()
                            ? selectedValue.value<QJSValue>().toVariant().toList()
                            : selectedValue.toList();
          if (!selectionToolbar ||
              !selectionToolbar->property("visible").toBool() ||
              !window->property("selectionMode").toBool() ||
              selectedIds.size() != 1 ||
              selectedIds.first().toString() != state->target) {
            fail("Select message did not select exactly the context-menu target");
            return;
          }
          auto *cancelSelection =
              named(window, "cancelMessageSelectionButton");
          if (!cancelSelection ||
              !QMetaObject::invokeMethod(cancelSelection, "clicked")) {
            fail("cancel message-level selection");
            return;
          }
          selectedValue = window->property("selectedMessageIds");
          selectedIds = selectedValue.canConvert<QJSValue>()
                            ? selectedValue.value<QJSValue>().toVariant().toList()
                            : selectedValue.toList();
          if (window->property("selectionMode").toBool() ||
              selectionToolbar->property("visible").toBool() ||
              !selectedIds.isEmpty()) {
            fail("Cancel did not clear bulk-message selection state");
            return;
          }
          auto body = map.value("body").toString();
          invoke(window, "copyMessageAction", "triggered");
          if (!QGuiApplication::clipboard() ||
              QGuiApplication::clipboard()->text() != body) {
            fail("message copy");
            return;
          }
          invoke(window, "quoteMessageAction", "triggered");
          if (!c.draft().startsWith("> ")) {
            fail("quote draft");
            return;
          }
          invoke(window, "messageInfoAction", "triggered");
          auto *info = named(window, "messageInfoDialog");
          if (!info || !info->property("visible").toBool()) {
            fail("message info");
            return;
          }
          auto *conversationInfo = named(window, "messageInfoConversation");
          auto *statusInfo = named(window, "messageInfoStatus");
          auto *timeInfo = named(window, "messageInfoTime");
          auto *idInfo = named(window, "messageInfoId");
          const auto rawStatus = map.value("status").toString();
          const auto displayedStatus =
              rawStatus == "demo_sent"
                  ? QStringLiteral("Saved locally · not sent to WhatsApp")
                  : rawStatus;
          const auto timeText = timeInfo ? timeInfo->property("text").toString()
                                         : QString();
          if (!conversationInfo || !statusInfo || !timeInfo || !idInfo ||
              conversationInfo->property("text").toString() !=
                  QStringLiteral("Conversation: ") + c.selectedTitle() ||
              statusInfo->property("text").toString() !=
                  QStringLiteral("Status: ") + displayedStatus ||
              !timeText.startsWith(QStringLiteral("Time: ")) ||
              timeText.size() <= QStringLiteral("Time: ").size() ||
              idInfo->property("text").toString() !=
                  QStringLiteral("ID: ") + state->target ||
              !idInfo->property("readOnly").toBool() ||
              !idInfo->property("selectByMouse").toBool()) {
            fail("message info fields do not describe the selected row accurately");
            return;
          }
          invoke(window, "messageInfoDialog", "close");
          auto *reactionMenu = named(window, "messageReactionMenu");
          if (!reactionMenu || !invoke(window, "messageReactionMenu", "open")) {
            fail("reaction submenu open");
            return;
          }
          state->stage = 20;
        } else if (state->stage == 20) {
          auto *reaction = named(window, "reactThumbsUpAction");
          if (!reaction) {
            fail("thumbs-up reaction action");
            return;
          }
          if (!QMetaObject::invokeMethod(reaction, "triggered")) {
            fail("thumbs-up reaction trigger");
            return;
          }
          state->stage = 21;
        } else if (state->stage == 21) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          const auto reactions = row.value("reactions").toList();
          if (reactions.isEmpty())
            return;
          if (reactions.first().toMap().value("emoji").toString() != "👍") {
            fail("reaction projection");
            return;
          }
          auto *jump = named(window, "jumpToLatestButton");
          if (!jump || !jump->property("visible").toBool())
            return;
          if (!invoke(window, "jumpToLatestButton", "clicked")) {
            fail("jump to latest action");
            return;
          }
          state->stage = 60;
        } else if (state->stage == 60) {
          if (c.loading())
            return;
          if (!state->revealedAfterLatest) {
            if (c.historyWindow())
              return;
            c.revealMessage(state->target);
            state->revealedAfterLatest = true;
            return;
          }
          if (c.messages()->indexOf(state->target) < 0)
            return;
          auto *targetBubble = messageBubble(window, state->target);
          auto *menuButton = namedBelow(targetBubble, "messageMenuButton");
          if (!targetBubble || !menuButton)
            return;
          if (!QMetaObject::invokeMethod(menuButton, "clicked")) {
            fail("open star target message menu");
            return;
          }
          state->stage = 61;
        } else if (state->stage == 61) {
          auto *menu = named(window, "messageMenu");
          auto *star = named(window, "starMessageAction");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!star || !star->property("visible").toBool() ||
              star->property("text").toString() != "Star message") {
            fail("star menu action is unavailable or mislabeled");
            return;
          }
          if (!QMetaObject::invokeMethod(star, "triggered")) {
            fail("star message action trigger");
            return;
          }
          state->stage = 62;
        } else if (state->stage == 62) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (!row.value("starred").toBool())
            return;
          if (row.value("mutation_provenance").toString() != "local_only") {
            fail("star state is not marked local-only");
            return;
          }
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button ||
              !QMetaObject::invokeMethod(button, "clicked"))
            return;
          state->stage = 63;
        } else if (state->stage == 63) {
          auto *menu = named(window, "messageMenu");
          auto *star = named(window, "starMessageAction");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!star || star->property("text").toString() != "Unstar message") {
            fail("starred message menu does not offer Unstar");
            return;
          }
          if (!QMetaObject::invokeMethod(star, "triggered")) {
            fail("unstar message action trigger");
            return;
          }
          state->stage = 64;
        } else if (state->stage == 64) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (row.value("starred").toBool())
            return;
          if (row.value("mutation_provenance").toString() != "local_only") {
            fail("unstar state lost its local-only provenance");
            return;
          }
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button ||
              !QMetaObject::invokeMethod(button, "clicked"))
            return;
          state->stage = 65;
        } else if (state->stage == 65) {
          auto *menu = named(window, "messageMenu");
          auto *pin = named(window, "pinMessageAction");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!pin || !pin->property("visible").toBool() ||
              pin->property("text").toString() != "Pin message") {
            fail("pin menu action is unavailable or mislabeled");
            return;
          }
          if (!QMetaObject::invokeMethod(pin, "triggered")) {
            fail("pin message action trigger");
            return;
          }
          state->stage = 66;
        } else if (state->stage == 66) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (!row.value("pinned").toBool())
            return;
          if (row.value("mutation_provenance").toString() != "local_only") {
            fail("pin state is not marked local-only");
            return;
          }
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button ||
              !QMetaObject::invokeMethod(button, "clicked"))
            return;
          state->stage = 67;
        } else if (state->stage == 67) {
          auto *menu = named(window, "messageMenu");
          auto *pin = named(window, "pinMessageAction");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!pin || pin->property("text").toString() != "Unpin message") {
            fail("pinned message menu does not offer Unpin");
            return;
          }
          if (!QMetaObject::invokeMethod(pin, "triggered")) {
            fail("unpin message action trigger");
            return;
          }
          state->stage = 68;
        } else if (state->stage == 68) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (row.value("pinned").toBool())
            return;
          if (row.value("mutation_provenance").toString() != "local_only") {
            fail("unpin state lost its local-only provenance");
            return;
          }
          qInfo() << "Native message flags PASS: star/unstar and pin/unpin persist as local-only state";
          state->forwardSourceChatId = c.selectedId();
          state->forwardBody = row.value("body").toString();
          if (state->forwardBody.trimmed().isEmpty() || row.value("deleted").toBool() ||
              !row.value("attachment").toMap().isEmpty() ||
              (!row.value("kind").toString().isEmpty() && row.value("kind").toString() != "text")) {
            fail("forward fixture must be an undeleted plain-text message");
            return;
          }
          for (int i = 0; i < c.chats()->rowCount(); ++i) {
            const auto candidate = c.chats()->row(i).value("id").toString();
            if (!candidate.isEmpty() && candidate != state->forwardSourceChatId) {
              state->forwardDestination = candidate;
              break;
            }
          }
          if (state->forwardDestination.isEmpty()) {
            fail("no distinct synthetic forwarding destination");
            return;
          }
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button || !QMetaObject::invokeMethod(button, "clicked"))
            return;
          state->stage = 69;
        } else if (state->stage == 69) {
          auto *menu = named(window, "messageMenu");
          auto *forward = named(window, "forwardMessageAction");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!forward || !forward->property("visible").toBool() ||
              !QMetaObject::invokeMethod(forward, "triggered")) {
            fail("open Forward message dialog");
            return;
          }
          state->stage = 70;
        } else if (state->stage == 70) {
          auto *dialog = named(window, "forwardMessageDialog");
          if (!dialog || !dialog->property("visible").toBool())
            return;
          auto *confirm = named(window, "confirmForwardMessage");
          auto *list = named(window, "forwardChatList");
          if (!confirm || !list || confirm->property("enabled").toBool() ||
              !window->property("forwardTargetChatId").toString().isEmpty()) {
            fail("Forward must require explicit destination selection");
            return;
          }
          auto *destination = forwardChatRowForId(window, state->forwardDestination);
          if (!destination) {
            fail("forward destination row lacks a stable rendered test seam");
            return;
          }
          if (!QMetaObject::invokeMethod(destination, "clicked")) {
            fail("select forward destination");
            return;
          }
          state->stage = 71;
        } else if (state->stage == 71) {
          auto *dialog = named(window, "forwardMessageDialog");
          auto *confirm = named(window, "confirmForwardMessage");
          if (!dialog || !dialog->property("visible").toBool() || !confirm)
            return;
          if (window->property("forwardTargetChatId").toString() != state->forwardDestination ||
              !confirm->property("enabled").toBool()) {
            fail("selected destination did not enable Forward");
            return;
          }
          if (!QMetaObject::invokeMethod(confirm, "clicked")) {
            fail("confirm Forward action");
            return;
          }
          state->stage = 72;
        } else if (state->stage == 72) {
          auto *dialog = named(window, "forwardMessageDialog");
          if (dialog && dialog->property("visible").toBool())
            return;
          if (c.selectedId() != state->forwardSourceChatId) {
            fail("forwarding changed the source conversation");
            return;
          }
          c.selectChat(state->forwardDestination);
          state->stage = 73;
        } else if (state->stage == 73) {
          if (c.selectedId() != state->forwardDestination || c.loading())
            return;
          QVariantMap forwarded;
          for (int i = c.messageCount() - 1; i >= 0; --i) {
            const auto candidate = c.messages()->row(i);
            if (candidate.value("forwarded").toBool() &&
                candidate.value("body").toString() == state->forwardBody) {
              forwarded = candidate;
              break;
            }
          }
          if (forwarded.isEmpty())
            return;
          if (!forwarded.value("outgoing").toBool() ||
              forwarded.value("status").toString() != "demo_sent" ||
              forwarded.value("mutation_provenance").toString() != "local_only") {
            fail("forward projection does not truthfully describe a local demo copy");
            return;
          }
          qInfo() << "Native forward PASS: rendered destination selection persisted the text in a different synthetic chat; local-only demo status retained";
          c.selectChat(state->forwardSourceChatId);
          state->stage = 74;
        } else if (state->stage == 74) {
          if (c.selectedId() != state->forwardSourceChatId || c.loading())
            return;
          state->stage = 22;
        } else if (state->stage == 22) {
          if (c.loading())
            return;
          if (!state->revealedAfterForward) {
            c.revealMessage(state->target);
            state->revealedAfterForward = true;
            return;
          }
          if (c.messages()->indexOf(state->target) < 0)
            return;
          auto *targetBubble = messageBubble(window, state->target);
          auto *menuButton = namedBelow(targetBubble, "messageMenuButton");
          if (!targetBubble || !menuButton)
            return;
          if (!QMetaObject::invokeMethod(menuButton, "clicked")) {
            fail("reopen target message menu");
            return;
          }
          state->stage = 23;
        } else if (state->stage == 23) {
          auto *menu = named(window, "messageMenu");
          if (!menu || !menu->property("visible").toBool())
            return;
          if (!invoke(window, "deleteLocalMessageAction", "triggered")) {
            fail("local delete menu action");
            return;
          }
          state->stage = 24;
        } else if (state->stage == 24) {
          auto *dialog = named(window, "deleteMessageConfirm");
          if (!dialog || !dialog->property("visible").toBool())
            return;
          if (!invoke(window, "confirmMessageDelete", "clicked")) {
            fail("confirm local delete");
            return;
          }
          state->stage = 25;
        } else if (state->stage == 25) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (!row.value("deleted").toBool())
            return;
          if (!row.value("reactions").toList().isEmpty()) {
            fail("deleted message retained reactions");
            return;
          }
          c.setDraft("Synthetic message before edit");
          if (!invoke(window, "sendButton", "clicked")) {
            fail("send edit fixture");
            return;
          }
          state->stage = 26;
        } else if (state->stage == 26) {
          if (c.sending() || c.messageCount() == 0)
            return;
          const auto row = c.messages()->row(c.messageCount() - 1);
          if (row.value("body").toString() != "Synthetic message before edit")
            return;
          if (!row.value("outgoing").toBool()) {
            fail("edit fixture must be an outgoing message");
            return;
          }
          state->target = row.value("id").toString();
          auto *bubble = messageBubble(window, state->target);
          auto *button = namedBelow(bubble, "messageMenuButton");
          if (!bubble || !button)
            return;
          if (!QMetaObject::invokeMethod(button, "clicked")) {
            fail("open edit target menu");
            return;
          }
          state->stage = 27;
        } else if (state->stage == 27) {
          auto *menu = named(window, "messageMenu");
          if (!menu || !menu->property("visible").toBool())
            return;
          auto *edit = named(window, "editMessageAction");
          if (!edit) {
            fail("edit action object missing");
            return;
          }
          if (!edit->property("visible").toBool()) {
            capture("edit-menu-debug");
            const auto value = window->property("menuMessage");
            const auto menuRecord = value.canConvert<QJSValue>()
                                        ? value.value<QJSValue>().toVariant().toMap()
                                        : value.toMap();
            const auto targetIndex = c.messages()->indexOf(state->target);
            const auto targetRow = targetIndex >= 0
                                       ? c.messages()->row(targetIndex)
                                       : QVariantMap{};
            auto *deleteAction = named(window, "deleteLocalMessageAction");
            auto *replyAction = named(window, "protocolReplyAction");
            fail(QString("edit unavailable: target=%1 menu=%2 outgoing=%3 rowOutgoing=%4 capability=%5 deleteVisible=%6 replyVisible=%7")
                     .arg(state->target, menuRecord.value("id").toString())
                     .arg(menuRecord.value("outgoing").toBool())
                     .arg(targetRow.value("outgoing").toBool())
                     .arg(c.hasCapability("message.edit"))
                     .arg(deleteAction && deleteAction->property("visible").toBool())
                     .arg(replyAction && replyAction->property("visible").toBool()));
            return;
          }
          if (!QMetaObject::invokeMethod(edit, "triggered")) {
            fail("open edit dialog");
            return;
          }
          state->stage = 28;
        } else if (state->stage == 28) {
          auto *dialog = named(window, "editMessageDialog");
          if (!dialog || !dialog->property("visible").toBool())
            return;
          auto *input = named(window, "editMessageText");
          if (!input) {
            fail("edit text field");
            return;
          }
          input->setProperty("text", "Synthetic edited message");
          if (!invoke(window, "saveMessageEdit", "clicked")) {
            fail("save message edit");
            return;
          }
          state->stage = 29;
        } else if (state->stage == 29) {
          const auto index = c.messages()->indexOf(state->target);
          if (index < 0)
            return;
          const auto row = c.messages()->row(index);
          if (row.value("body").toString() != "Synthetic edited message" ||
              !row.value("edited").toBool()) {
            if (!c.error().isEmpty()) {
              fail("edit action result: " + c.error());
              return;
            }
            return;
          }
          c.stageAttachment(QUrl::fromLocalFile(fixture));
          state->stage = 6;
        } else if (state->stage == 6 && !c.attachment().isEmpty()) {
          if (c.attachment().value("kind").toString() != "gif") {
            fail("GIF inspect");
            return;
          }
          c.latest();
          state->stage = 7;
        } else if (state->stage == 7 && !c.loading() && !c.historyWindow()) {
          if (!invoke(window, "sendButton", "clicked")) {
            fail("attachment send control");
            return;
          }
          state->stage = 8;
        } else if (state->stage == 8 && !c.sending() &&
                   c.attachment().isEmpty()) {
          auto last = c.messages()->row(c.messageCount() - 1);
          if (last.value("attachment").toMap().value("kind").toString() !=
              "gif") {
            fail("attachment not in timeline");
            return;
          }
          window->resize(720, 600);
          state->stage = 9;
        } else if (state->stage == 9) {
          auto *rail =
              qobject_cast<QQuickItem *>(named(window, "navigationRail"));
          auto *surface = qobject_cast<QQuickItem *>(
              named(window, "floatingComposerSurface"));
          if (rail && rail->isVisible()) {
            fail("compact rail did not hide");
            return;
          }
          if (!surface || surface->width() > 720 || surface->height() < 40) {
            fail("compact composer geometry");
            return;
          }
          window->resize(1180, 780);
          state->stage = 50;
        } else if (state->stage == 50) {
          auto *rail = qobject_cast<QQuickItem *>(named(window, "navigationRail"));
          auto *aboutButton = named(window, "aboutButton");
          if (!rail || !rail->isVisible() || !aboutButton ||
              !aboutButton->property("visible").toBool())
            return;
          if (!invoke(window, "aboutButton", "clicked")) {
            fail("rail About route");
            return;
          }
          state->stage = 51;
        } else if (state->stage == 51) {
          auto *dialog = named(window, "aboutDialog");
          if (!dialog || !dialog->property("visible").toBool())
            return;
          invoke(window, "aboutDialog", "close");
          if (albumFixtures.size() >= 2) {
            if (!c.hasCapability(QStringLiteral("attachment.album.prepare")) ||
                !c.hasCapability(QStringLiteral("message.album.send")) ||
                !c.hasCapability(QStringLiteral("attachment.album.release"))) {
              fail("local demo album capabilities are unavailable");
              return;
            }
            state->albumInitialMessages = c.messageCount();
            if (!invoke(window, "attachmentButton", "clicked")) {
              fail("open attachment menu for album selection");
              return;
            }
            state->stage = 52;
            return;
          }
          qInfo() << "Native polish PASS: navigation Chats/Find/Settings/Profile/About, "
                     "floating composer, emoji/GIF controls, static WebP sticker "
                     "selection/review/send/render, local search/reveal, inbound "
                     "quoted-context preview, copy/quote/info menu, GIF send and "
                     "compact layout";
          timer->stop();
          app.quit();
        } else if (state->stage == 52) {
          auto *menu = named(window, "attachmentMenu");
          if (!menu || !menu->property("visible").toBool())
            return;
          auto *action = named(window, "photoAlbumAttachmentAction");
          if (!action || !action->property("enabled").toBool() ||
              !action->property("text").toString().contains("album", Qt::CaseInsensitive)) {
            fail("photo album action missing or disabled in local demo");
            return;
          }
          if (!invoke(window, "photoAlbumAttachmentAction", "triggered")) {
            fail("trigger photo album file chooser");
            return;
          }
          state->stage = 53;
          return;
        } else if (state->stage == 53) {
          auto *picker = named(window, "albumFilePicker");
          if (!picker) {
            fail("album multi-file chooser is missing");
            return;
          }
          const auto filters = picker->property("nameFilters").toStringList();
          const auto filterText = filters.join(QLatin1Char(' '));
          if (picker->property("fileMode").toInt() != 1 ||
              filters.isEmpty() || !filterText.contains("*.png") ||
              !filterText.contains("*.jpg") || !filterText.contains("*.webp") ||
              filterText.contains("*.gif")) {
            fail(QString("album chooser must open multiple still images only (mode=%1 filters=%2)")
                     .arg(picker->property("fileMode").toInt())
                     .arg(filterText));
            return;
          }
          if (!QMetaObject::invokeMethod(picker, "close")) {
            fail("close album file chooser without selecting files");
            return;
          }
          QVariantList files;
          for (const auto &path : albumFixtures)
            files.append(QUrl::fromLocalFile(path));
          c.stageAttachmentAlbum(files);
          state->stage = 54;
          return;
        } else if (state->stage == 54) {
          const auto attachment = c.attachment();
          if (attachment.value("state").toString() == "error") {
            fail("album preparation failed: " + attachment.value("error").toString());
            return;
          }
          if (attachment.value("state").toString() != "ready")
            return;
          const auto items = attachment.value("items").toList();
          auto *dialog = named(window, "attachmentPreviewDialog");
          auto *grid = named(window, "albumReviewGrid");
          const auto qmlAlbumItems =
              dialog ? dialog->property("albumItems").toList() : QVariantList{};
          const auto gridCount = grid ? grid->property("count").toInt() : -1;
          if (attachment.value("kind").toString() != "album" ||
              attachment.value("selected_count").toInt() != 2 || items.size() != 2 ||
              !dialog || !dialog->property("visible").toBool() || !grid ||
              !grid->property("visible").toBool() || gridCount != 2) {
            const auto firstItem = items.isEmpty() ? QVariantMap{} : items.first().toMap();
            const QUrl firstItemUrl(firstItem.value("url").toString());
            const auto detail =
                QStringLiteral("kind=%1 state=%2 provenance=%3 selected=%4 responseItems=%5 qmlReview=%6 qmlItems=%7 gridVisible=%8 gridCount=%9 firstKind=%10 firstMode=%11 firstState=%12 firstMime=%13 firstUrlScheme=%14 firstLocal=%15 firstSize=%16 firstPrepared=%17 reportedTotal=%18")
                    .arg(attachment.value("kind").toString())
                    .arg(attachment.value("state").toString())
                    .arg(attachment.value("provenance").toString())
                    .arg(attachment.value("selected_count").toInt())
                    .arg(items.size())
                    .arg(dialog && dialog->property("albumReview").toBool())
                    .arg(qmlAlbumItems.size())
                    .arg(grid && grid->property("visible").toBool())
                    .arg(gridCount)
                    .arg(firstItem.value("kind").toString())
                    .arg(firstItem.value("mode").toString())
                    .arg(firstItem.value("state").toString())
                    .arg(firstItem.value("mime").toString())
                    .arg(firstItemUrl.scheme())
                    .arg(firstItemUrl.isLocalFile())
                    .arg(firstItem.value("size").toLongLong())
                    .arg(firstItem.value("prepared_size").toLongLong())
                    .arg(attachment.value("total_prepared_size").toLongLong());
            fail("album review did not expose the exact two prepared still images (" + detail + ")");
            return;
          }
          auto *send = named(window, "attachmentSend");
          if (!send || !send->property("enabled").toBool() ||
              send->property("text").toString() != "Send album") {
            fail("album review lacks an explicit enabled Send album action");
            return;
          }
          auto *first = named(window, "albumReviewImage0");
          auto *second = named(window, "albumReviewImage1");
          auto *localOnly = named(window, "albumLocalOnlyDisclosure");
          if (!localOnly || !localOnly->property("visible").toBool() ||
              !localOnly->property("text").toString().contains("Local demo only") ||
              !localOnly->property("text").toString().contains("Nothing is uploaded to WhatsApp")) {
            fail("album review must disclose its local-demo-only boundary");
            return;
          }
          if (!first || !second || first->property("status").toInt() != 1 ||
              second->property("status").toInt() != 1) {
            return;
          }
          auto *caption = named(window, "attachmentCaption");
          auto *cancel = named(window, "attachmentCancel");
          if (!caption || !cancel) {
            fail("album review caption or cancel action missing");
            return;
          }
          caption->setProperty("text", QStringLiteral("Synthetic album caption"));
          capture("album-review");
          if (!QMetaObject::invokeMethod(cancel, "clicked")) {
            fail("cancel album review");
            return;
          }
          state->stage = 55;
          return;
        } else if (state->stage == 55) {
          if (!c.attachment().isEmpty() || c.sending())
            return;
          if (c.messageCount() != state->albumInitialMessages ||
              c.draft() != QStringLiteral("Synthetic album caption")) {
            fail("canceling album review sent a message or discarded the caption");
            return;
          }
          capture("album-after-cancel");
          QVariantList files;
          for (const auto &path : albumFixtures)
            files.append(QUrl::fromLocalFile(path));
          c.stageAttachmentAlbum(files);
          state->stage = 56;
          return;
        } else if (state->stage == 56) {
          const auto attachment = c.attachment();
          if (attachment.value("state").toString() == "error") {
            fail("album re-preparation failed: " + attachment.value("error").toString());
            return;
          }
          if (attachment.value("state").toString() != "ready")
            return;
          auto *caption = named(window, "attachmentCaption");
          auto *send = named(window, "attachmentSend");
          if (attachment.value("kind").toString() != "album" ||
              attachment.value("items").toList().size() != 2 || !caption ||
              caption->property("text").toString() != QStringLiteral("Synthetic album caption") ||
              !send || !send->property("enabled").toBool() ||
              send->property("text").toString() != "Send album") {
            fail("reopened album review did not preserve the selected images and caption");
            return;
          }
          if (!QMetaObject::invokeMethod(send, "clicked")) {
            fail("activate explicit Send album action");
            return;
          }
          state->stage = 57;
          state->ticks = 0;
          return;
        } else if (state->stage == 57) {
          const auto attachment = c.attachment();
          if (!c.sending() && !attachment.isEmpty() &&
              attachment.value("state").toString() == "ready") {
            fail(c.error().isEmpty()
                     ? "Send album did not start while the reviewed album remained ready."
                     : "Album send failed: " + c.error());
            return;
          }
          if (c.sending() || !attachment.isEmpty())
            return;
          if (c.messageCount() != state->albumInitialMessages + 1) {
            fail(QString("one explicit Send album should append exactly one row (before=%1 after=%2 error=%3)")
                     .arg(state->albumInitialMessages)
                     .arg(c.messageCount())
                     .arg(c.error()));
            return;
          }
          QVariantMap albumMessage;
          int albumRows = 0;
          for (int i = 0; i < c.messageCount(); ++i) {
            const auto row = c.messages()->row(i);
            if (row.value("kind").toString() == "album") {
              ++albumRows;
              albumMessage = row;
            }
          }
          const auto items = albumMessage.value("album").toList();
          const auto id = albumMessage.value("id").toString();
          auto *bubble = messageBubble(window, id);
          auto *grid = namedBelow(bubble, "albumPreviewGrid");
          auto *first = namedBelow(bubble, "albumPreviewImage0");
          auto *second = namedBelow(bubble, "albumPreviewImage1");
          auto *body = namedBelow(bubble, "messageBody");
          auto *localOnly = namedBelow(bubble, "albumLocalOnlyTimelineDisclosure");
          const int firstStatus = first ? first->property("status").toInt() : -1;
          const int secondStatus = second ? second->property("status").toInt() : -1;
          // QML Image.status=Loading (2) is transient; keep polling within the
          // existing bounded stage timeout before treating it as a failed preview.
          if (firstStatus == 2 || secondStatus == 2)
            return;
          if (albumRows != 1 || id.isEmpty() ||
              albumMessage.value("chat_id").toString() != c.selectedId() ||
              albumMessage.value("body").toString() != QStringLiteral("Synthetic album caption") ||
              albumMessage.value("status").toString() != "demo_sent" ||
              items.size() != 2 || !grid || !grid->isVisible() ||
              grid->property("albumItemCount").toInt() != 2 ||
              !first || !second || firstStatus != 1 || secondStatus != 1 || !body ||
              !body->property("text").toString().contains(QStringLiteral("Synthetic album caption")) ||
              !localOnly || !localOnly->property("visible").toBool() ||
              !localOnly->property("text").toString().contains("not sent to WhatsApp")) {
            fail(QStringLiteral("synthetic album timeline did not render one local row with two prepared previews and a local-only disclosure (rows=%1 items=%2 grid=%3/%4 images=%5/%6 caption=%7 disclosure=%8)")
                     .arg(albumRows)
                     .arg(items.size())
                     .arg(grid && grid->isVisible())
                     .arg(grid ? grid->property("albumItemCount").toInt() : -1)
                     .arg(firstStatus)
                     .arg(secondStatus)
                     .arg(body && body->property("text").toString().contains(QStringLiteral("Synthetic album caption")))
                     .arg(localOnly && localOnly->property("visible").toBool() &&
                          localOnly->property("text").toString().contains("not sent to WhatsApp")));
            return;
          }
          capture("album-timeline");
          qInfo() << "Native album attachment PASS: multi-file chooser, bounded image review, "
                     "explicit cancel, caption preservation, one local demo send and timeline grid";
          qInfo() << "Native polish PASS: navigation Chats/Find/Settings/Profile/About, "
                     "floating composer, emoji/GIF controls, static WebP sticker "
                     "selection/review/send/render, local search/reveal, inbound "
                     "quoted-context preview, copy/quote/info menu, GIF send, photo album, "
                     "and compact layout";
          timer->stop();
          app.quit();
        }
      });
  timer->start();
}
