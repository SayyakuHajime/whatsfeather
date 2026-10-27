#include "legaldocuments.h"

#include <QColor>
#include <QCoreApplication>
#include <QCryptographicHash>
#include <QDebug>
#include <QDir>
#include <QEvent>
#include <QGuiApplication>
#include <QKeyEvent>
#include <QMouseEvent>
#include <QQuickItem>
#include <QQuickWindow>
#include <QTimer>
#include <QVariantMap>
#include <cmath>
#include <functional>
#include <memory>

namespace {
QObject *named(QQuickWindow *window, const QString &objectName) {
  if (window && window->objectName() == objectName)
    return window;
  if (auto *object = window->findChild<QObject *>(objectName))
    return object;

  std::function<QQuickItem *(QQuickItem *)> visit =
      [&](QQuickItem *item) -> QQuickItem * {
    if (!item)
      return nullptr;
    if (item->objectName() == objectName)
      return item;
    for (auto *child : item->childItems())
      if (auto *match = visit(child))
        return match;
    return nullptr;
  };
  return visit(window->contentItem());
}

bool popupOpen(QObject *popup) {
  return popup && (popup->property("opened").toBool() ||
                   popup->property("visible").toBool());
}

bool click(QQuickWindow *window, const QString &objectName) {
  auto *item = qobject_cast<QQuickItem *>(named(window, objectName));
  if (!item || !item->isVisible() || !item->isEnabled() || item->width() <= 0 ||
      item->height() <= 0)
    return false;

  const QPointF local = item->mapToScene(
      QPointF(item->width() / 2.0, item->height() / 2.0));
  const QPoint global = window->mapToGlobal(local.toPoint());
  QMouseEvent press(QEvent::MouseButtonPress, local, local, global,
                    Qt::LeftButton, Qt::LeftButton, Qt::NoModifier);
  QCoreApplication::sendEvent(window, &press);
  QMouseEvent release(QEvent::MouseButtonRelease, local, local, global,
                      Qt::LeftButton, Qt::NoButton, Qt::NoModifier);
  QCoreApplication::sendEvent(window, &release);
  return true;
}

void key(QQuickWindow *window, int key, Qt::KeyboardModifiers modifiers = Qt::NoModifier) {
  QObject *receiver = window->activeFocusItem();
  if (!receiver)
    receiver = window;
  QKeyEvent press(QEvent::KeyPress, key, modifiers);
  QCoreApplication::sendEvent(receiver, &press);
  QKeyEvent release(QEvent::KeyRelease, key, modifiers);
  QCoreApplication::sendEvent(receiver, &release);
}

double luminance(const QColor &color) {
  const auto linear = [](double channel) {
    channel /= 255.0;
    return channel <= 0.04045 ? channel / 12.92
                              : std::pow((channel + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * linear(color.red()) + 0.7152 * linear(color.green()) +
         0.0722 * linear(color.blue());
}

double contrastRatio(const QColor &foreground, const QColor &background) {
  if (!foreground.isValid() || !background.isValid())
    return 0.0;
  const double foregroundLuminance = luminance(foreground);
  const double backgroundLuminance = luminance(background);
  return (qMax(foregroundLuminance, backgroundLuminance) + 0.05) /
         (qMin(foregroundLuminance, backgroundLuminance) + 0.05);
}

QString captureName(const QString &state, const QString &theme) {
  return QStringLiteral("%1-%2.png").arg(state, theme);
}
} // namespace

void runAboutLegalTest(QGuiApplication &app, QQuickWindow *window,
                       const LegalDocuments &documents) {
  struct State {
    int stage = 0;
    int ticks = 0;
    int wideDocument = 0;
    QString theme;
  };
  auto state = std::make_shared<State>();
  const QStringList documentIds{QStringLiteral("project-license"),
                                QStringLiteral("asset-licenses"),
                                QStringLiteral("third-party-notices")};
  const QStringList documentButtons{QStringLiteral("projectLicenseButton"),
                                    QStringLiteral("assetLicensesButton"),
                                    QStringLiteral("thirdPartyNoticesButton")};
  const QString expectedDisclaimer = QStringLiteral(
      "A native Linux messaging preview. Unofficial and not affiliated with WhatsApp or Meta.");
  const QString expectedSummary = QStringLiteral(
      "Project code is licensed under GPL-3.0-only. Its full text grants permission to redistribute copies under its terms and states that the software comes with no warranty. Third-party components may have different terms.");
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [&app, window, &documents, state, timer, documentIds,
                    documentButtons, expectedDisclaimer, expectedSummary] {
    auto fail = [&](const QString &message) {
      qCritical().noquote() << "Native About legal FAIL at stage" << state->stage
                            << message;
      timer->stop();
      app.exit(1);
    };
    auto capture = [&](const QString &name) {
      const QString directory =
          qEnvironmentVariable("WHATSFEATHER_QA_CAPTURE_DIR");
      if (directory.isEmpty())
        return true;
      if (!QDir().mkpath(directory))
        return false;
      const QString path = QDir(directory).filePath(
          captureName(name, state->theme));
      return window->grabWindow().save(path);
    };
    auto bounded = [&](QObject *dialog) {
      const qreal width = dialog ? dialog->property("width").toReal() : 0;
      const qreal height = dialog ? dialog->property("height").toReal() : 0;
      return dialog && width > 0 && height > 0 &&
             width <= window->width() + 0.5 &&
             height <= window->height() + 0.5 &&
             dialog->property("x").toReal() >= -0.5 &&
             dialog->property("y").toReal() >= -0.5 &&
             dialog->property("x").toReal() + width <= window->width() + 0.5 &&
             dialog->property("y").toReal() + height <= window->height() + 0.5;
    };
    auto checkText = [&](const QString &expectedId, bool logHash) {
      auto *dialog = named(window, QStringLiteral("legalDocumentDialog"));
      auto *body = named(window, QStringLiteral("legalDocumentText"));
      auto *scroll = named(window, QStringLiteral("legalDocumentScroll"));
      auto *documentError = named(window, QStringLiteral("legalDocumentError"));
      auto *offlineHint = named(window, QStringLiteral("legalDocumentOfflineHint"));
      auto *closeButton = qobject_cast<QQuickItem *>(
          named(window, QStringLiteral("legalDocumentCloseButton")));
      if (!popupOpen(dialog) || !body || !scroll || !documentError ||
          !offlineHint ||
          offlineHint->property("text").toString() !=
              QStringLiteral("Offline copy embedded with this application. Read-only and selectable. Scroll with the mouse wheel or Page Up/Page Down.") ||
          !closeButton || !closeButton->isVisible() || !closeButton->isEnabled() ||
          dialog->property("documentId").toString() != expectedId ||
          documentError->property("visible").toBool()) {
        fail(QStringLiteral("document dialog did not open cleanly for %1").arg(expectedId));
        return false;
      }
      if (!closeButton->hasActiveFocus())
        return false;
      const QVariantMap loaded = documents.load(expectedId);
      const QString text = body->property("text").toString();
      const QByteArray utf8 = text.toUtf8();
      auto *scrollItem = qobject_cast<QQuickItem *>(scroll);
      auto *bodyItem = qobject_cast<QQuickItem *>(body);
      const qreal contentHeight = scroll->property("contentHeight").toReal();
      const qreal viewportHeight = scroll->property("height").toReal();
      if (!loaded.value(QStringLiteral("ok")).toBool() || text.isEmpty() ||
          text != loaded.value(QStringLiteral("text")).toString() ||
          body->property("readOnly").toBool() != true ||
          body->property("selectByMouse").toBool() != true ||
          body->property("selectByKeyboard").toBool() != true ||
          body->property("textFormat").toInt() != int(Qt::PlainText) ||
          !scrollItem || !bodyItem ||
          scroll->property("width").toReal() > window->width() + 0.5 ||
          viewportHeight <= 0 || contentHeight <= viewportHeight ||
          !bounded(dialog)) {
        fail(QStringLiteral("plain-text selection or bounded-scroll contract failed for %1")
                 .arg(expectedId));
        return false;
      }
      if (logHash) {
        const QByteArray digest =
            QCryptographicHash::hash(utf8, QCryptographicHash::Sha256).toHex();
        qInfo().noquote()
            << QStringLiteral("About legal document PASS: id=%1 bytes=%2 sha256=%3")
                   .arg(expectedId)
                   .arg(utf8.size())
                   .arg(QString::fromLatin1(digest));
      }
      return true;
    };

    if (++state->ticks > 600) {
      fail(QStringLiteral("timed out"));
      return;
    }

    if (state->stage == 0) {
      if (!window->isVisible())
        return;
      auto *appRoot = named(window, QStringLiteral("appWindow"));
      const bool dark = appRoot && appRoot->property("dark").toBool();
      state->theme = dark ? QStringLiteral("dark") : QStringLiteral("light");
      const QColor ink = appRoot ? appRoot->property("ink").value<QColor>() : QColor();
      const QColor chrome = appRoot ? appRoot->property("chrome").value<QColor>() : QColor();
      const QColor muted = appRoot ? appRoot->property("muted").value<QColor>() : QColor();
      const QColor surface = appRoot ? appRoot->property("surface").value<QColor>() : QColor();
      const QColor active = appRoot ? appRoot->property("active").value<QColor>() : QColor();
      const QColor accent = appRoot ? appRoot->property("accent").value<QColor>() : QColor();
      const double inkContrast = contrastRatio(ink, chrome);
      const double mutedContrast = contrastRatio(muted, surface);
      const double selectedTextContrast = contrastRatio(ink, active);
      const double focusContrast = contrastRatio(accent, active);
      if (window->width() < 900 || !appRoot || inkContrast < 4.5 ||
          mutedContrast < 4.5 || selectedTextContrast < 4.5 ||
          focusContrast < 3.0) {
        fail(QStringLiteral("initial theme, geometry, or token contrast is invalid (width=%1 dark=%2 ink=%3 chrome=%4 inkRatio=%5 muted=%6 surface=%7 mutedRatio=%8 selectionRatio=%9 focusRatio=%10)")
                 .arg(window->width())
                 .arg(dark)
                 .arg(ink.name())
                 .arg(chrome.name())
                 .arg(inkContrast)
                 .arg(muted.name())
                 .arg(surface.name())
                 .arg(mutedContrast)
                 .arg(selectedTextContrast)
                 .arg(focusContrast));
        return;
      }
      qInfo().noquote()
          << QStringLiteral("About theme contrast PASS: theme=%1 ink/chrome=%2 muted/surface=%3 selected/active=%4 focus/active=%5")
                 .arg(state->theme)
                 .arg(inkContrast)
                 .arg(mutedContrast)
                 .arg(selectedTextContrast)
                 .arg(focusContrast);
      if (!click(window, QStringLiteral("aboutButton"))) {
        fail(QStringLiteral("wide About rail button could not be clicked"));
        return;
      }
      state->stage = 1;
      return;
    }

    if (state->stage == 1) {
      auto *about = named(window, QStringLiteral("aboutDialog"));
      auto *disclaimer = named(window, QStringLiteral("aboutDisclaimer"));
      auto *summary = named(window, QStringLiteral("aboutLicenseSummary"));
      auto *projectButton = qobject_cast<QQuickItem *>(
          named(window, QStringLiteral("projectLicenseButton")));
      if (!popupOpen(about) || !disclaimer || !summary || !projectButton)
        return;
      if (!bounded(about) ||
          disclaimer->property("text").toString() != expectedDisclaimer ||
          summary->property("text").toString() != expectedSummary ||
          !projectButton->hasActiveFocus()) {
        fail(QStringLiteral("About disclaimer, license summary, geometry, or initial keyboard focus failed"));
        return;
      }
      if (!capture(QStringLiteral("about-wide"))) {
        fail(QStringLiteral("could not save synthetic wide About capture"));
        return;
      }
      if (!click(window, QStringLiteral("projectLicenseButton"))) {
        fail(QStringLiteral("focused project-license action could not be clicked"));
        return;
      }
      state->stage = 2;
      return;
    }

    if (state->stage == 2) {
      auto *dialog = named(window, QStringLiteral("legalDocumentDialog"));
      if (!popupOpen(dialog))
        return;
      const QString expectedId = documentIds[state->wideDocument];
      if (!checkText(expectedId, true))
        return;
      if (expectedId == QStringLiteral("third-party-notices") &&
          !capture(QStringLiteral("legal-notices-wide"))) {
        fail(QStringLiteral("could not save synthetic wide notices capture"));
        return;
      }
      auto *body = qobject_cast<QQuickItem *>(
          named(window, QStringLiteral("legalDocumentText")));
      if (!body) {
        fail(QStringLiteral("document body is not a keyboard-focusable item"));
        return;
      }
      key(window, Qt::Key_Tab);
      if (!body->hasActiveFocus()) {
        fail(QStringLiteral("Tab did not move keyboard focus from Close into the document"));
        return;
      }
      key(window, Qt::Key_PageDown);
      state->stage = 3;
      return;
    }

    if (state->stage == 3) {
      auto *scroll = named(window, QStringLiteral("legalDocumentScroll"));
      if (!scroll || scroll->property("contentY").toReal() <= 0)
        return;
      key(window, Qt::Key_PageUp);
      state->stage = 4;
      return;
    }

    if (state->stage == 4) {
      auto *scroll = named(window, QStringLiteral("legalDocumentScroll"));
      if (!scroll || scroll->property("contentY").toReal() > 0.5)
        return;
      key(window, Qt::Key_A, Qt::ControlModifier);
      state->stage = 5;
      return;
    }

    if (state->stage == 5) {
      auto *body = named(window, QStringLiteral("legalDocumentText"));
      if (!body)
        return;
      const QString text = body->property("text").toString();
      QString selected = body->property("selectedText").toString();
      selected.replace(QChar(0x2029), QLatin1Char('\n'));
      if (selected != text) {
        fail(QStringLiteral("Ctrl+A did not select the full read-only document"));
        return;
      }
      if (state->wideDocument == 0) {
        if (!click(window, QStringLiteral("legalDocumentCloseButton"))) {
          fail(QStringLiteral("visible legal Close button could not be activated"));
          return;
        }
      } else {
        key(window, Qt::Key_Escape);
      }
      state->stage = 6;
      return;
    }

    if (state->stage == 6) {
      auto *dialog = named(window, QStringLiteral("legalDocumentDialog"));
      if (popupOpen(dialog))
        return;
      if (state->wideDocument + 1 < documentIds.size()) {
        ++state->wideDocument;
        if (!click(window, documentButtons[state->wideDocument])) {
          fail(QStringLiteral("About legal document action could not be clicked: %1")
                   .arg(documentButtons[state->wideDocument]));
          return;
        }
        state->stage = 2;
        return;
      }
      auto *about = named(window, QStringLiteral("aboutDialog"));
      if (!about || !QMetaObject::invokeMethod(about, "close")) {
        fail(QStringLiteral("could not close the About dialog after wide checks"));
        return;
      }
      state->stage = 7;
      return;
    }

    if (state->stage == 7) {
      if (popupOpen(named(window, QStringLiteral("aboutDialog"))))
        return;
      window->resize(720, 520);
      state->stage = 8;
      return;
    }

    if (state->stage == 8) {
      auto *appRoot = named(window, QStringLiteral("appWindow"));
      auto *rail = named(window, QStringLiteral("navigationRail"));
      if (!appRoot || window->width() >= 900 ||
          !rail || rail->property("visible").toBool())
        return;
      if (!click(window, QStringLiteral("chatListOptionsButton"))) {
        fail(QStringLiteral("compact chat options button could not be clicked"));
        return;
      }
      state->stage = 9;
      return;
    }

    if (state->stage == 9) {
      auto *menu = named(window, QStringLiteral("sidebarMenu"));
      if (!popupOpen(menu))
        return;
      if (!click(window, QStringLiteral("aboutMenuItem"))) {
        fail(QStringLiteral("compact About menu item could not be clicked"));
        return;
      }
      state->stage = 10;
      return;
    }

    if (state->stage == 10) {
      auto *about = named(window, QStringLiteral("aboutDialog"));
      if (!popupOpen(about))
        return;
      if (!bounded(about) || !capture(QStringLiteral("about-compact"))) {
        fail(QStringLiteral("compact About geometry or capture failed"));
        return;
      }
      if (!click(window, QStringLiteral("thirdPartyNoticesButton"))) {
        fail(QStringLiteral("compact notices action could not be clicked"));
        return;
      }
      state->stage = 11;
      return;
    }

    if (state->stage == 11) {
      auto *dialog = named(window, QStringLiteral("legalDocumentDialog"));
      if (!popupOpen(dialog))
        return;
      if (!checkText(QStringLiteral("third-party-notices"), false) ||
          !capture(QStringLiteral("legal-notices-compact")))
        return;
      auto *body = qobject_cast<QQuickItem *>(
          named(window, QStringLiteral("legalDocumentText")));
      if (!body) {
        fail(QStringLiteral("compact notices body is not keyboard accessible"));
        return;
      }
      key(window, Qt::Key_Tab);
      if (!body->hasActiveFocus()) {
        fail(QStringLiteral("compact Tab did not move focus into the document"));
        return;
      }
      key(window, Qt::Key_PageDown);
      state->stage = 12;
      return;
    }

    if (state->stage == 12) {
      auto *scroll = named(window, QStringLiteral("legalDocumentScroll"));
      if (!scroll || scroll->property("contentY").toReal() <= 0)
        return;
      key(window, Qt::Key_A, Qt::ControlModifier);
      state->stage = 13;
      return;
    }

    if (state->stage == 13) {
      auto *body = named(window, QStringLiteral("legalDocumentText"));
      if (!body)
        return;
      const QString text = body->property("text").toString();
      QString selected = body->property("selectedText").toString();
      selected.replace(QChar(0x2029), QLatin1Char('\n'));
      if (selected != text) {
        fail(QStringLiteral("compact Ctrl+A did not select the complete notices"));
        return;
      }
      key(window, Qt::Key_Escape);
      state->stage = 14;
      return;
    }

    if (state->stage == 14) {
      if (popupOpen(named(window, QStringLiteral("legalDocumentDialog"))))
        return;
      const QVariantMap refused = documents.load(QStringLiteral("../LICENSE"));
      if (refused.value(QStringLiteral("ok")).toBool() ||
          !refused.value(QStringLiteral("text")).toString().isEmpty()) {
        fail(QStringLiteral("provider accepted a non-whitelisted document ID"));
        return;
      }
      auto *appRoot = named(window, QStringLiteral("appWindow"));
      QVariant opened;
      const bool invoked = appRoot && QMetaObject::invokeMethod(
          appRoot, "openLegalDocument", Qt::DirectConnection,
          Q_RETURN_ARG(QVariant, opened),
          Q_ARG(QVariant, QVariant(QStringLiteral("../LICENSE"))));
      if (!invoked) {
        fail(QStringLiteral("could not exercise invalid-ID error view"));
        return;
      }
      if (opened.toBool()) {
        fail(QStringLiteral("QML accepted a non-whitelisted document ID"));
        return;
      }
      state->stage = 15;
      return;
    }

    if (state->stage == 15) {
      auto *dialog = named(window, QStringLiteral("legalDocumentDialog"));
      auto *body = named(window, QStringLiteral("legalDocumentText"));
      auto *error = named(window, QStringLiteral("legalDocumentError"));
      if (!popupOpen(dialog))
        return;
      if (!body || !error || !error->property("visible").toBool() ||
          error->property("text").toString().isEmpty() ||
          !body->property("text").toString().isEmpty()) {
        fail(QStringLiteral("invalid resource ID did not fail closed with an explicit error"));
        return;
      }
      key(window, Qt::Key_Escape);
      state->stage = 16;
      return;
    }

    if (state->stage == 16) {
      if (popupOpen(named(window, QStringLiteral("legalDocumentDialog"))))
        return;
      auto *about = named(window, QStringLiteral("aboutDialog"));
      if (!about || !QMetaObject::invokeMethod(about, "close")) {
        fail(QStringLiteral("could not close the compact About dialog after checks"));
        return;
      }
      state->stage = 17;
      return;
    }

    if (state->stage == 17) {
      if (popupOpen(named(window, QStringLiteral("aboutDialog"))))
        return;
      qInfo().noquote()
          << QStringLiteral("Native About legal access PASS: full offline documents, plain-text selection, bounded keyboard scrolling, wide/compact routes and both-theme tokens (%1)")
                 .arg(state->theme);
      timer->stop();
      app.exit(0);
    }
  });
  timer->start();
}
