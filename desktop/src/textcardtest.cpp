// Private same-UID RPC and native rendering probe for inert synthetic text card content.
#include "controller.h"
#include <QAccessible>
#include <QDir>
#include <QFileDevice>
#include <QFileInfo>
#include <QHash>
#include <QImage>
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
const QString ChatID = QStringLiteral("fixture:textCard-chat");
const QString MessageID = QStringLiteral("fixture:incoming-textCard");
const QString Header = QStringLiteral("<b>Header</b>");
const QString Body = QStringLiteral("<script>Literal body</script>");
const QString Footer = QStringLiteral("<i>Footer</i>");
const QString FirstLabel = QStringLiteral("<img src=x>");
const QString SecondLabel = QStringLiteral("A & B");
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

QObject *namedWithText(QObject *root, const QString &name, const QString &text) {
  if (!root)
    return nullptr;
  if (root->objectName() == name && root->property("text").toString().contains(text))
    return root;
  if (auto *window = qobject_cast<QQuickWindow *>(root))
    if (auto *visual = namedWithText(window->contentItem(), name, text))
      return visual;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = namedWithText(child, name, text))
        return found;
  for (auto *child : root->children())
    if (auto *found = namedWithText(child, name, text))
      return found;
  return nullptr;
}

class SyntheticTextCardRPC final : public QObject {
public:
  explicit SyntheticTextCardRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticTextCardRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wftxt-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private text card fixture directory");
      return false;
    }
    if (!QFile::setPermissions(m_temp->path(), QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner)) {
      m_error = QStringLiteral("cannot secure text card fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private text card fixture socket exceeds the AF_UNIX budget");
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
      m_error = QStringLiteral("text card fixture socket is not private");
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
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic text card fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      reply(socket, id, {{"version", 1}, {"methods", QJsonArray{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set"}}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic textCard"}, {"initials", "SB"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject textCard{{"title", Header}, {"subtitle", "Subtitle"}, {"body", Body}, {"footer", Footer},
                                {"labels", QJsonArray{FirstLabel, SecondLabel}}};
      const QJsonObject message{{"id", MessageID}, {"chat_id", ChatID}, {"body", Body},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "template"}, {"template", textCard}};
      QJsonObject interactive = message; interactive["id"] = "fixture:interactive"; interactive["kind"] = "interactive"; auto fields = textCard; fields.remove("labels"); interactive.remove("template"); fields["title"] = "Interactive <b>title</b>"; interactive["interactive"] = fields;
      const QJsonObject legacy{{"id", "fixture:legacy-textCard"}, {"chat_id", ChatID}, {"body", LegacyBody},
                               {"timestamp", 2000}, {"outgoing", false}, {"status", "received"}, {"kind", "text"}};
      const QJsonObject callLog{{"id", "fixture:call-log"}, {"chat_id", ChatID}, {"body", "Missed video call"},
                                {"timestamp", 3000}, {"outgoing", false}, {"status", "received"}, {"kind", "call"}};
      reply(socket, id, {{"messages", QJsonArray{message, interactive, legacy, callLog}}, {"has_more", false}});
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
};
} // namespace

QObject *startTextCardFixture(QGuiApplication &, Controller &controller,
                             const QString &dataDir) {
  auto *fixture = new SyntheticTextCardRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private text card fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runTextCardTest(QGuiApplication &app, QQuickWindow *window,
                    Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticTextCardRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native text card UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int ticks = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [state, timer, window, controllerPtr = &controller, fixture, appPtr = &app] {
    auto fail = [timer, appPtr](const QString &message) {
      qCritical().noquote() << "Native text card UI FAIL:" << message;
      timer->stop();
      appPtr->exit(1);
    };
    auto &controller = *controllerPtr;
    if (++state->ticks > 240) {
      fail(QStringLiteral("timed out waiting for synthetic text card card"));
      return;
    }
    if (!controller.connected() || !controller.capabilitiesReady() ||
        controller.selectedId() != ChatID || controller.loading() || controller.messageCount() != 4)
      return;

    auto *surface = named(window, QStringLiteral("textCardCardSurface"));
    auto *header = namedWithText(window, QStringLiteral("textCardTitle"), Header);
    auto *subtitle = named(window, QStringLiteral("textCardSubtitle"));
    auto *body = named(window, QStringLiteral("textCardBody"));
    auto *footer = named(window, QStringLiteral("textCardFooter"));
    auto *disclosure = named(window, QStringLiteral("textCardDisclosure"));
    auto *first = named(window, QStringLiteral("textCardLabel1"));
    auto *second = named(window, QStringLiteral("textCardLabel2"));
    auto *interactiveTitle = namedWithText(window, QStringLiteral("textCardTitle"), QStringLiteral("Interactive <b>title</b>"));
    auto *legacy = namedWithText(window, QStringLiteral("messageBody"), QStringLiteral("Legacy &lt;b&gt;plain&lt;/b&gt; text"));
    auto *callLog = namedWithText(window, QStringLiteral("messageBody"), QStringLiteral("Missed video call"));
    if (!surface || !surface->property("visible").toBool() || !header || !body || !footer ||
        !interactiveTitle || !subtitle || !disclosure || !first || !second || !legacy || !callLog) {
      fail(QStringLiteral("typed text card, inert labels, legacy text, or call-log summary was not rendered"));
      return;
    }
    if (subtitle->property("text").toString() != QStringLiteral("Subtitle")) { fail(QStringLiteral("subtitle lost")); return; }
    if (header->property("text").toString() != Header || body->property("text").toString() != Body ||
        footer->property("text").toString() != Footer || disclosure->property("text").toString() != QStringLiteral("Display only") ||
        first->property("text").toString() != FirstLabel || second->property("text").toString() != SecondLabel ||
        legacy->property("visible").toBool() != true || callLog->property("text").toString() != QStringLiteral("Missed video call")) {
      fail(QStringLiteral("text card display text, legacy fallback, or call-log summary changed"));
      return;
    }
    if (callLog->property("textFormat").toInt() != 0) {
      fail(QStringLiteral("call-log summary must be rendered as Text.PlainText"));
      return;
    }
    for (auto *field : {header, interactiveTitle, subtitle, body, footer, disclosure, first, second}) {
      if (field->property("textFormat").toInt() != 0) {
        fail(QStringLiteral("untrusted text card text must use Text.PlainText"));
        return;
      }
    }
    for (auto *field : {header, interactiveTitle, subtitle, body, footer, disclosure}) {
      auto *accessible = QAccessible::queryAccessibleInterface(field);
      if (!accessible || accessible->role() != QAccessible::StaticText || accessible->text(QAccessible::Name) != field->property("text").toString()) {
        fail(QStringLiteral("card field lacks an accessible static-text name")); return;
      }
    }
    for (auto *label : {first, second}) {
      const auto background = label->property("background");
      if (!background.isValid() || !background.isNull()) {
        fail(QStringLiteral("display-only label has a button-like background"));
        return;
      }
      for (const auto *paddingName : {"leftPadding", "rightPadding", "topPadding", "bottomPadding"}) {
        const auto padding = label->property(paddingName);
        if (!padding.isValid() || padding.toDouble() != 0.0) {
          fail(QStringLiteral("display-only label has button-like padding"));
          return;
        }
      }
    }
    auto *accessible = QAccessible::queryAccessibleInterface(first);
    if (!accessible || accessible->role() != QAccessible::StaticText || accessible->text(QAccessible::Name) !=
                           QStringLiteral("Template label (display only): ") + FirstLabel) {
      fail(QStringLiteral("text card label accessible name is missing its inert disclosure"));
      return;
    }
    if (named(window, QStringLiteral("textCardActionButton")) ||
        named(window, QStringLiteral("textCardSelectButton")) || fixture->unexpectedMethodCount() != 0) {
      fail(QStringLiteral("text card display labels exposed or dispatched an action"));
      return;
    }
    const auto capturePath = qEnvironmentVariable("WHATSFEATHER_TEXT_CARD_TEST_CAPTURE");
    if (!capturePath.isEmpty()) {
      const auto capture = window->grabWindow();
      if (capture.isNull() || !capture.save(capturePath)) {
        fail(QStringLiteral("could not capture the synthetic text card card"));
        return;
      }
    }
    qInfo() << "Native text card PASS: header/body/footer and labels use PlainText; labels are inert display-only text; legacy text remains visible";
    timer->stop();
    appPtr->exit(0);
  });
  timer->start();
}
