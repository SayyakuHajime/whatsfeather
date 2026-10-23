// Private in-process RPC fixture and keyboard interaction probe for New chat.
#include "controller.h"
#include <QCoreApplication>
#include <QDebug>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QKeyEvent>
#include <QLocalServer>
#include <QLocalSocket>
#include <QQuickItem>
#include <QQuickWindow>
#include <QTemporaryDir>
#include <QTimer>
#include <functional>
#include <memory>

namespace {
constexpr qint64 MaxFrame = 256 * 1024;
const QString VerifiedPhone = QStringLiteral("+15551234567");
const QString ChatID = QStringLiteral("fixture:verified-chat");
const QString ChatTitle = QStringLiteral("Synthetic Verified Contact");

QObject *named(QObject *root, const QString &name) {
  return root ? root->findChild<QObject *>(name, Qt::FindChildrenRecursively)
              : nullptr;
}

QObject *namedVisual(QQuickWindow *window, const QString &name) {
  std::function<QQuickItem *(QQuickItem *)> visit =
      [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == name)
      return item;
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  return window ? visit(window->contentItem()) : nullptr;
}

bool keyboardActivate(QObject *object) {
  auto *item = qobject_cast<QQuickItem *>(object);
  if (!item || !item->isVisible() || !item->property("enabled").toBool())
    return false;
  item->forceActiveFocus(Qt::TabFocusReason);
  QKeyEvent press(QEvent::KeyPress, Qt::Key_Space, Qt::NoModifier);
  QCoreApplication::sendEvent(item, &press);
  QKeyEvent release(QEvent::KeyRelease, Qt::Key_Space, Qt::NoModifier);
  QCoreApplication::sendEvent(item, &release);
  return true;
}

class SyntheticRosterRPC final : public QObject {
public:
  explicit SyntheticRosterRPC(QObject *parent = nullptr) : QObject(parent) {}

  ~SyntheticRosterRPC() override {
    QObject::disconnect(&m_server, nullptr, this, nullptr);
    m_server.close();
    const auto sockets = m_server.findChildren<QLocalSocket *>();
    for (auto *socket : sockets) {
      QObject::disconnect(socket, nullptr, this, nullptr);
      socket->abort();
    }
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() +
                                             QStringLiteral("/wf-roster-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private fixture directory");
      return false;
    }
    const auto privatePermissions = QFileDevice::ReadOwner |
                                    QFileDevice::WriteOwner |
                                    QFileDevice::ExeOwner;
    if (!QFile::setPermissions(m_temp->path(), privatePermissions)) {
      m_error = QStringLiteral("cannot restrict private fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    m_server.setSocketOptions(QLocalServer::UserAccessOption);
    if (!m_server.listen(m_socketPath)) {
      m_error = m_server.errorString();
      return false;
    }
    const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                           QFileDevice::ExeGroup | QFileDevice::ReadOther |
                           QFileDevice::WriteOther | QFileDevice::ExeOther;
    if (QFileInfo(m_socketPath).permissions() & forbidden) {
      m_error = QStringLiteral("fixture socket permissions are not private");
      return false;
    }
    connect(&m_server, &QLocalServer::newConnection, this, [this] {
      while (m_server.hasPendingConnections()) {
        auto *socket = m_server.nextPendingConnection();
        m_input.insert(socket, {});
        connect(socket, &QLocalSocket::readyRead, this, [this, socket] {
          auto &buffer = m_input[socket];
          buffer += socket->readAll();
          while (true) {
            const auto newline = buffer.indexOf('\n');
            if (newline < 0)
              break;
            if (newline + 1 > MaxFrame) {
              socket->abort();
              return;
            }
            const auto line = buffer.left(newline);
            buffer.remove(0, newline + 1);
            QJsonParseError error;
            const auto document = QJsonDocument::fromJson(line, &error);
            if (error.error != QJsonParseError::NoError || !document.isObject()) {
              socket->abort();
              return;
            }
            handle(socket, document.object());
          }
          if (buffer.size() >= MaxFrame)
            socket->abort();
        });
        connect(socket, &QLocalSocket::disconnected, this, [this, socket] {
          m_input.remove(socket);
          socket->deleteLater();
        });
      }
    });
    return true;
  }

  QString socketPath() const { return m_socketPath; }
  QString error() const { return m_error; }
  int openCount() const { return m_openCount; }
  QString openedRecipient() const { return m_openedRecipient; }
  int contactLookupCount() const { return m_contactLookupCount; }
  bool contactQueryProvided() const { return m_contactQueryProvided; }
  QString contactQuery() const { return m_contactQuery; }
  QString contactCursor() const { return m_contactCursor; }

private:
  QJsonObject contact() const {
    return {{"jid", QStringLiteral("15551234567@s.whatsapp.net")},
            {"name", ChatTitle},
            {"phone", QStringLiteral("15551234567")},
            {"verified", true}};
  }

  QJsonObject secondContact() const {
    return {{"jid", QStringLiteral("15551234568@s.whatsapp.net")},
            {"name", QStringLiteral("Synthetic Contact Two")},
            {"phone", QStringLiteral("15551234568")},
            {"verified", true}};
  }

  QJsonObject chat() const {
    return {{"id", ChatID}, {"title", ChatTitle}, {"initials", "S"}};
  }

  QJsonObject envelope(const QString &id, const QJsonObject &result) const {
    return {{"v", 1}, {"id", id}, {"result", result}};
  }

  QJsonObject failure(const QString &id, const QString &code,
                      const QString &text) const {
    return {{"v", 1}, {"id", id},
            {"error", QJsonObject{{"code", code}, {"message", text}}}};
  }

  void send(QLocalSocket *socket, const QJsonObject &response) {
    if (!socket || socket->state() != QLocalSocket::ConnectedState)
      return;
    socket->write(QJsonDocument(response).toJson(QJsonDocument::Compact) + '\n');
    socket->flush();
  }

  void handle(QLocalSocket *socket, const QJsonObject &request) {
    const auto id = request.value("id").toString();
    const auto method = request.value("method").toString();
    const auto params = request.value("params").toObject();
    if (id.isEmpty() || request.value("v").toInt() != 1) {
      send(socket, failure(id, QStringLiteral("invalid_request"),
                           QStringLiteral("Expected synthetic v1 RPC")));
      return;
    }
    if (method == QStringLiteral("app.status")) {
      send(socket, envelope(id, {{"mode", "demo"}, {"state", "demo"},
                                 {"revision", 1},
                                 {"message", "Synthetic roster fixture"}}));
    } else if (method == QStringLiteral("app.capabilities")) {
      send(socket, envelope(
                       id, {{"version", 1},
                            {"methods", QJsonArray{
                                 QStringLiteral("app.status"),
                                 QStringLiteral("app.capabilities"),
                                 QStringLiteral("account.get"),
                                 QStringLiteral("chat.list"),
                                 QStringLiteral("chat.open"),
                                 QStringLiteral("contact.list"),
                                 QStringLiteral("draft.get"),
                                 QStringLiteral("draft.set"),
                                 QStringLiteral("message.list")}},
                            {"features", QJsonObject{}}}));
    } else if (method == QStringLiteral("account.get")) {
      send(socket, envelope(id, {{"available", false}, {"state", "demo"}}));
    } else if (method == QStringLiteral("chat.list")) {
      QJsonArray chats;
      if (m_openCount > 0)
        chats.append(chat());
      send(socket, envelope(id, {{"chats", chats}}));
    } else if (method == QStringLiteral("contact.list")) {
      ++m_contactLookupCount;
      m_contactQueryProvided = params.contains(QStringLiteral("query"));
      m_contactQuery = params.value(QStringLiteral("query")).toString();
      m_contactCursor = params.value(QStringLiteral("cursor")).toString();
      if (m_contactCursor.isEmpty()) {
        send(socket, envelope(id, {{"contacts", QJsonArray{contact()}},
                                   {"has_more", true},
                                   {"next_cursor", contact().value("jid")}}));
      } else {
        send(socket, envelope(id, {{"contacts", QJsonArray{secondContact()}},
                                   {"has_more", false},
                                   {"next_cursor", ""}}));
      }
    } else if (method == QStringLiteral("chat.open")) {
      ++m_openCount;
      m_openedRecipient = params.value("recipient").toString();
      if (m_openedRecipient != VerifiedPhone) {
        send(socket, failure(id, QStringLiteral("not_registered"),
                             QStringLiteral("Synthetic registration verification rejected recipient")));
        return;
      }
      send(socket, envelope(id, {{"chat", chat()}, {"contact", contact()},
                                 {"verified", true}}));
    } else if (method == QStringLiteral("draft.get")) {
      send(socket, envelope(id, {{"text", ""}}));
    } else if (method == QStringLiteral("draft.set")) {
      send(socket, envelope(id, {{"saved", true}}));
    } else if (method == QStringLiteral("message.list")) {
      send(socket, envelope(id, {{"messages", QJsonArray{}},
                                 {"has_more", false}}));
    } else {
      send(socket, failure(id, QStringLiteral("unsupported"),
                           QStringLiteral("Synthetic roster fixture method unsupported")));
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_error, m_openedRecipient, m_contactQuery, m_contactCursor;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  int m_openCount = 0, m_contactLookupCount = 0;
  bool m_contactQueryProvided = false;
};
} // namespace

QObject *startChatRosterFixture(QGuiApplication &, Controller &controller,
                                const QString &dataDir) {
  auto *fixture = new SyntheticRosterRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical() << "Cannot start private chat roster fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runChatRosterTest(QGuiApplication &app, QQuickWindow *window,
                       Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticRosterRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native chat roster UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State {
    int stage = 0;
    int ticks = 0;
  };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [&, state, timer, window, fixture] {
    auto fail = [&](const QString &message) {
      qCritical().noquote() << "Native chat roster UI FAIL:" << message;
      timer->stop();
      app.exit(1);
    };
    if (++state->ticks > 300) {
      fail(QStringLiteral("timed out at stage %1").arg(state->stage));
      return;
    }
    if (!controller.demo()) {
      fail(QStringLiteral("refusing interactions outside demo mode"));
      return;
    }
    if (state->stage == 0) {
      if (!window->isVisible() || !controller.connected() ||
          !controller.capabilitiesReady())
        return;
      if (!controller.hasCapability(QStringLiteral("chat.open")) ||
          !controller.hasCapability(QStringLiteral("contact.list"))) {
        fail(QStringLiteral("private fixture did not advertise roster capabilities"));
        return;
      }
      auto *dialog = named(window, QStringLiteral("newChatDialog"));
      auto *recipient = named(window, QStringLiteral("newChatRecipient"));
      if (!dialog || !recipient || !QMetaObject::invokeMethod(dialog, "open")) {
        fail(QStringLiteral("cannot open the actual New chat dialog"));
        return;
      }
      state->stage = 1;
      return;
    }
    if (state->stage == 1) {
      if (fixture->contactLookupCount() == 0) {
        if (state->ticks > 40)
          fail(QStringLiteral("opening New chat did not request the full roster"));
        return;
      }
      if (fixture->contactLookupCount() != 1 || !fixture->contactQueryProvided() ||
          !fixture->contactQuery().isEmpty() || !fixture->contactCursor().isEmpty()) {
        fail(QStringLiteral("opening New chat must request the first page with an empty query and cursor"));
        return;
      }
      if (controller.contactsLoading() || controller.contacts()->rowCount() == 0)
        return;
      if (controller.contacts()->rowCount() != 1) {
        fail(QStringLiteral("expected exactly one first-page synthetic roster contact"));
        return;
      }
      const auto record = controller.contacts()->row(0);
      if (record.size() != 4 || record.contains(QStringLiteral("recipient")) ||
          record.value(QStringLiteral("jid")).toString() !=
              QStringLiteral("15551234567@s.whatsapp.net") ||
          record.value(QStringLiteral("name")).toString() != ChatTitle ||
          record.value(QStringLiteral("phone")).toString() !=
              QStringLiteral("15551234567") ||
          !record.value(QStringLiteral("verified")).toBool()) {
        fail(QStringLiteral("fixture did not preserve the real contact JSON shape"));
        return;
      }
      auto *more = namedVisual(window, QStringLiteral("loadMoreContacts"));
      if (!more || !more->property("visible").toBool()) {
        fail(QStringLiteral("additional roster pages are not exposed to the user"));
        return;
      }
      if (!keyboardActivate(more)) {
        fail(QStringLiteral("load-more roster control is not keyboard-activatable"));
        return;
      }
      state->stage = 2;
      return;
    }
    if (state->stage == 2) {
      if (fixture->contactLookupCount() < 2) {
        if (state->ticks > 100)
          fail(QStringLiteral("load-more control did not request the next roster page"));
        return;
      }
      if (fixture->contactLookupCount() != 2 || !fixture->contactQueryProvided() ||
          !fixture->contactQuery().isEmpty() ||
          fixture->contactCursor() != QStringLiteral("15551234567@s.whatsapp.net")) {
        fail(QStringLiteral("next contact page did not preserve the empty query and exact cursor"));
        return;
      }
      if (controller.contactsLoading() || controller.contacts()->rowCount() < 2)
        return;
      if (controller.contacts()->rowCount() != 2 ||
          controller.contacts()->row(0).value(QStringLiteral("jid")).toString() !=
              QStringLiteral("15551234567@s.whatsapp.net") ||
          controller.contacts()->row(1).value(QStringLiteral("jid")).toString() !=
              QStringLiteral("15551234568@s.whatsapp.net")) {
        fail(QStringLiteral("the next roster page did not append without losing the first page"));
        return;
      }
      auto *row = namedVisual(window, QStringLiteral("contactLookupRow"));
      if (!row || !row->property("visible").toBool() ||
          !row->property("enabled").toBool() || !keyboardActivate(row)) {
        fail(QStringLiteral("first verified roster contact is not keyboard-activatable after paging"));
        return;
      }
      state->stage = 3;
      return;
    }
    if (state->stage == 3) {
      if (fixture->openCount() == 0)
        return;
      if (fixture->openCount() != 1 ||
          fixture->openedRecipient() != VerifiedPhone) {
        fail(QStringLiteral("keyboard selection must issue exactly one chat.open with +155****4567"));
        return;
      }
      state->stage = 4;
      return;
    }
    if (fixture->openCount() != 1 ||
        fixture->openedRecipient() != VerifiedPhone) {
      fail(QStringLiteral("chat.open count or verified E.164 recipient changed"));
      return;
    }
    if (controller.selectedId() != ChatID ||
        controller.selectedTitle() != ChatTitle)
      return;
    if (controller.chats()->indexOf(ChatID) < 0) {
      fail(QStringLiteral("verified returned chat is not in the selected roster"));
      return;
    }
    qInfo() << "Native chat roster UI PASS: empty-query first page, bounded cursor pagination, keyboard activation, one verified E.164 chat.open, returned chat selected";
    timer->stop();
    app.quit();
  });
  timer->start();
}
