// Private same-UID RPC and native rendering probe for explicit synthetic ListMessage row selection/reply.
#include "controller.h"
#include <QAccessible>
#include <QDir>
#include <QFileDevice>
#include <QFileInfo>
#include <QGuiApplication>
#include <QHash>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLocalServer>
#include <QLocalSocket>
#include <QQuickItem>
#include <QQuickWindow>
#include <QTemporaryDir>
#include <QTimer>
#include <QDebug>
#include <memory>

namespace {
constexpr int MaxFrame = 256 * 1024;
const QString ChatID = QStringLiteral("fixture:list-chat");
const QString MessageID = QStringLiteral("fixture:incoming-list");
const QString Title = QStringLiteral("<script>Menu</script> & choices");
const QString Description = QStringLiteral("Choose a fixture");
const QString Footer = QStringLiteral("<i>Display only</i>");
const QString ButtonLabel = QStringLiteral("<b>Open list</b>");
const QString SectionTitle = QStringLiteral("<u>Options</u>");
const QString RowTitle = QStringLiteral("<img src=x onerror=alert(1)>");
const QString RowDescription = QStringLiteral("Literal & text");
const QString LegacyBody = QStringLiteral("Legacy <b>plain</b> text");

QObject *named(QObject *root, const QString &name) {
  if (!root)
    return nullptr;
  if (auto *window = qobject_cast<QQuickWindow *>(root)) {
    if (auto *visual = named(window->contentItem(), name))
      return visual;
  }
  if (root->objectName() == name)
    return root;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = named(child, name))
        return found;
  for (auto *child : root->children())
    if (auto *found = named(child, name))
      return found;
  return nullptr;
}

QObject *namedContainingText(QObject *root, const QString &name, const QString &text) {
  if (!root)
    return nullptr;
  if (root->objectName() == name && root->property("text").toString().contains(text))
    return root;
  if (auto *window = qobject_cast<QQuickWindow *>(root))
    if (auto *visual = namedContainingText(window->contentItem(), name, text))
      return visual;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = namedContainingText(child, name, text))
        return found;
  for (auto *child : root->children())
    if (auto *found = namedContainingText(child, name, text))
      return found;
  return nullptr;
}

class SyntheticListRPC final : public QObject {
public:
  explicit SyntheticListRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticListRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wflm-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private list fixture directory");
      return false;
    }
    if (!QFile::setPermissions(m_temp->path(), QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner)) {
      m_error = QStringLiteral("cannot secure list fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private list fixture socket exceeds the AF_UNIX budget");
      return false;
    }
    m_server.setSocketOptions(QLocalServer::UserAccessOption);
    if (!m_server.listen(m_socketPath)) {
      m_error = m_server.errorString();
      return false;
    }
    const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                           QFileDevice::ExeGroup | QFileDevice::ReadOther |
                           QFileDevice::WriteOther | QFileDevice::ExeOther;
    if (QFileInfo(m_socketPath).permissions() & forbidden) {
      m_error = QStringLiteral("list fixture socket is not private");
      return false;
    }
    connect(&m_server, &QLocalServer::newConnection, this, [this] {
      while (m_server.hasPendingConnections()) {
        auto *socket = m_server.nextPendingConnection();
        m_input.insert(socket, {});
        connect(socket, &QLocalSocket::readyRead, this, [this, socket] {
          auto &buffer = m_input[socket];
          buffer += socket->readAll();
          if (buffer.size() > MaxFrame && !buffer.contains('\n')) {
            socket->abort();
            return;
          }
          while (true) {
            const auto newline = buffer.indexOf('\n');
            if (newline < 0)
              break;
            if (newline + 1 > MaxFrame) {
              socket->abort();
              return;
            }
            const auto frame = buffer.left(newline);
            buffer.remove(0, newline + 1);
            const auto request = QJsonDocument::fromJson(frame).object();
            if (!request.isEmpty())
              handle(socket, request);
          }
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
  int unexpectedMethodCount() const { return m_unexpectedMethodCount; }
  int listReplyRequestCount() const { return m_listReplyRequestCount; }
  QJsonObject lastListReplyParams() const { return m_lastListReplyParams; }

private:
  void reply(QLocalSocket *socket, const QString &id, const QJsonObject &result) {
    const auto frame = QJsonDocument(QJsonObject{{"v", 1}, {"id", id}, {"result", result}})
                           .toJson(QJsonDocument::Compact) + '\n';
    if (frame.size() <= MaxFrame) {
      socket->write(frame);
      socket->flush();
    }
  }

  void handle(QLocalSocket *socket, const QJsonObject &request) {
    const auto id = request.value(QStringLiteral("id")).toString();
    const auto method = request.value(QStringLiteral("method")).toString();
    if (method == QStringLiteral("app.status")) {
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic list fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      reply(socket, id, {{"version", 1}, {"methods", QJsonArray{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set", "list.reply"}}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic list"}, {"initials", "SL"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject row{{"title", RowTitle}, {"description", RowDescription}};
      const QJsonObject section{{"title", SectionTitle}, {"rows", QJsonArray{row}}};
      const QJsonObject list{{"title", Title}, {"description", Description}, {"footer", Footer},
                             {"button_label", ButtonLabel}, {"sections", QJsonArray{section}}};
      const QJsonObject message{{"id", MessageID}, {"chat_id", ChatID}, {"body", Title},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "list"}, {"list", list},
                                {"list_reply_available", true}};
      const QJsonObject legacy{{"id", "fixture:legacy-text"}, {"chat_id", ChatID}, {"body", LegacyBody},
                               {"timestamp", 2000}, {"outgoing", false}, {"status", "received"}, {"kind", "text"}};
      reply(socket, id, {{"messages", QJsonArray{message, legacy}}, {"has_more", false}});
    } else if (method == QStringLiteral("list.reply")) {
      ++m_listReplyRequestCount;
      m_lastListReplyParams = request.value(QStringLiteral("params")).toObject();
      const QJsonObject row{{"title", RowTitle}, {"description", RowDescription}};
      const QJsonObject section{{"title", SectionTitle}, {"rows", QJsonArray{row}}};
      const QJsonObject list{{"title", Title}, {"description", Description}, {"footer", Footer},
                             {"button_label", ButtonLabel}, {"sections", QJsonArray{section}}};
      const QJsonObject target{{"id", MessageID}, {"chat_id", ChatID}, {"body", Title},
                               {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                               {"kind", "list"}, {"list", list},
                               {"list_reply_available", false}, {"list_reply_state", "accepted"}};
      const QJsonObject response{{"id", "fixture:selected-list-response"}, {"chat_id", ChatID},
                                 {"body", RowTitle}, {"timestamp", 1500}, {"outgoing", true},
                                 {"status", "accepted"}, {"reply_to_id", MessageID}};
      reply(socket, id, {{"message", response}, {"target", target}, {"status", "accepted"}});
    } else if (method == QStringLiteral("draft.get")) {
      reply(socket, id, {{"text", ""}});
    } else if (method == QStringLiteral("draft.set")) {
      reply(socket, id, {{"saved", true}});
    } else {
      ++m_unexpectedMethodCount;
      reply(socket, id, {{"unsupported", true}});
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  int m_unexpectedMethodCount = 0;
  int m_listReplyRequestCount = 0;
  QJsonObject m_lastListReplyParams;
};
} // namespace

QObject *startListMessageFixture(QGuiApplication &, Controller &controller,
                                 const QString &dataDir) {
  auto *fixture = new SyntheticListRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private list fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runListMessageTest(QGuiApplication &app, QQuickWindow *window,
                        Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticListRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native list UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int ticks = 0; int stage = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [state, timer, window, controllerPtr = &controller, fixture, appPtr = &app] {
    auto fail = [timer, appPtr](const QString &message) {
      qCritical().noquote() << "Native list UI FAIL:" << message;
      timer->stop();
      appPtr->exit(1);
    };
    auto &controller = *controllerPtr;
    if (++state->ticks > 240) {
      fail(QStringLiteral("timed out waiting for synthetic list UI stage=%1 requests=%2 messages=%3 locks=%4")
               .arg(state->stage).arg(fixture->listReplyRequestCount()).arg(controller.messageCount())
               .arg(controller.listReplyBlockedTargets().join(QStringLiteral(","))));
      return;
    }
    const int expectedMessages = state->stage < 2 ? 2 : 3;
    if (!controller.connected() || !controller.capabilitiesReady() ||
        controller.selectedId() != ChatID || controller.loading() ||
        controller.messageCount() < expectedMessages)
      return;

    auto *surface = named(window, QStringLiteral("listCardSurface"));
    auto *title = named(window, QStringLiteral("listTitle"));
    auto *description = named(window, QStringLiteral("listDescription"));
    auto *footer = named(window, QStringLiteral("listFooter"));
    auto *section = named(window, QStringLiteral("listSectionTitle1"));
    auto *rowTitle = named(window, QStringLiteral("listRowTitle1_1"));
    auto *rowDescription = named(window, QStringLiteral("listRowDescription1_1"));
    auto *legacy = namedContainingText(window, QStringLiteral("messageBody"), QStringLiteral("Legacy &lt;b&gt;plain&lt;/b&gt; text"));
    if (!surface || !surface->property("visible").toBool() || !title || !description || !footer || !section || !rowTitle || !rowDescription || !legacy) {
      fail(QStringLiteral("typed list card, inert text, or legacy plain-text message was not rendered"));
      return;
    }
    if (title->property("text").toString() != Title ||
        description->property("text").toString() != Description ||
        footer->property("text").toString() != Footer ||
        section->property("text").toString() != SectionTitle ||
        rowTitle->property("text").toString() != RowTitle ||
        rowDescription->property("text").toString() != RowDescription ||
        legacy->property("visible").toBool() != true) {
      fail(QStringLiteral("list display strings changed or markup was interpreted"));
      return;
    }
    for (const auto *field : {title, description, footer, section, rowTitle, rowDescription}) {
      if (field->property("textFormat").toInt() != 0) {
        fail(QStringLiteral("untrusted list content must use Text.PlainText"));
        return;
      }
    }
    if (named(window, QStringLiteral("listRowAction")) ||
        fixture->unexpectedMethodCount() != 0) {
      fail(QStringLiteral("list rows exposed an unscoped action"));
      return;
    }
    auto *selectButton = named(window, QStringLiteral("listSelectButton"));
    if (state->stage < 2 && (!selectButton || !selectButton->property("visible").toBool() ||
                             !selectButton->property("enabled").toBool() ||
                             !selectButton->property("primary").toBool())) {
      fail(QStringLiteral("complete actionable list lacks a visible primary row-selection control"));
      return;
    }
    if (state->stage == 2 && selectButton && selectButton->property("visible").toBool()) {
      fail(QStringLiteral("list reply control remained available after submission"));
      return;
    }
    if (state->stage == 0) {
      const auto capturePath = qEnvironmentVariable("WHATSFEATHER_LIST_TEST_CAPTURE");
      if (!capturePath.isEmpty()) {
        const auto capture = window->grabWindow();
        if (capture.isNull() || !capture.save(capturePath)) {
          fail(QStringLiteral("could not capture the synthetic list card"));
          return;
        }
      }
      if (!QMetaObject::invokeMethod(selectButton, "clicked")) {
        fail(QStringLiteral("list selection control could not open the chooser"));
        return;
      }
      state->stage = 1;
      return;
    }
    if (state->stage == 1) {
      auto *dialog = named(window, QStringLiteral("listReplyDialog"));
      if (!dialog || !dialog->property("visible").toBool())
        return;
      auto *option = named(window, QStringLiteral("listReplyOption0_0"));
      if (!option || option->property("text").toString() != RowTitle) {
        fail(QStringLiteral("chooser omitted the literal selected-row title"));
        return;
      }
      auto *optionAccessible = QAccessible::queryAccessibleInterface(option);
      if (!optionAccessible || optionAccessible->text(QAccessible::Name) !=
                                   QStringLiteral("Select list option: ") + RowTitle +
                                       QStringLiteral(" · ") + RowDescription) {
        fail(QStringLiteral("chooser row lacks its exact accessible name"));
        return;
      }
      const auto dialogCapture = qEnvironmentVariable("WHATSFEATHER_LIST_DIALOG_CAPTURE");
      if (!dialogCapture.isEmpty()) {
        const auto capture = window->grabWindow();
        if (capture.isNull() || !capture.save(dialogCapture)) {
          fail(QStringLiteral("could not capture the row chooser"));
          return;
        }
      }
      if (!QMetaObject::invokeMethod(option, "clicked")) {
        fail(QStringLiteral("selected list row could not dispatch its explicit action"));
        return;
      }
      state->stage = 2;
      return;
    }
    if (state->stage == 2) {
      if (fixture->listReplyRequestCount() == 0 || controller.messageCount() < 3)
        return;
      const auto params = fixture->lastListReplyParams();
      if (fixture->listReplyRequestCount() != 1 ||
          params.value(QStringLiteral("target_id")).toString() != MessageID ||
          params.value(QStringLiteral("section_index")).toInt(-1) != 0 ||
          params.value(QStringLiteral("row_index")).toInt(-1) != 0 ||
          params.contains(QStringLiteral("row_id")) ||
          params.value(QStringLiteral("client_id")).toString().isEmpty()) {
        fail(QStringLiteral("native selection did not send only stable row indexes and target context"));
        return;
      }
      const int targetIndex = controller.messages()->indexOf(MessageID);
      const int responseIndex = controller.messages()->indexOf(QStringLiteral("fixture:selected-list-response"));
      auto *statusLabel = named(window, QStringLiteral("listButtonLabel"));
      if (targetIndex < 0 || responseIndex < 0 || !statusLabel ||
          !statusLabel->property("visible").toBool() ||
          statusLabel->property("text").toString() != ButtonLabel + QStringLiteral(" · Response accepted") ||
          controller.messages()->row(targetIndex).value("list_reply_state").toString() != QStringLiteral("accepted") ||
          controller.messages()->row(targetIndex).value("list_reply_available").toBool() ||
          controller.messages()->row(responseIndex).value("body").toString() != RowTitle ||
          controller.messages()->row(responseIndex).value("status").toString() != QStringLiteral("accepted") ||
          controller.performMessageAction(QStringLiteral("list.reply"), controller.messages()->row(targetIndex),
                                          {{"section_index", 0}, {"row_index", 0}})) {
        fail(QStringLiteral("accepted response did not reconcile the target, visible status, and duplicate-selection lock"));
        return;
      }
      auto *statusAccessible = QAccessible::queryAccessibleInterface(statusLabel);
      if (!statusAccessible || statusAccessible->text(QAccessible::Name) !=
                                   QStringLiteral("List response status: Response accepted: ") + ButtonLabel) {
        fail(QStringLiteral("accepted status lacks an exact accessible announcement"));
        return;
      }
      qInfo() << "Native list PASS: explicit row selection, stable index RPC, accepted target reconciliation, duplicate blocked, untrusted fields remain PlainText";
      timer->stop();
      appPtr->exit(0);
      return;
    }
  });
  timer->start();
}
