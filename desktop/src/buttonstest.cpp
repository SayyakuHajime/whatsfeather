// Private same-UID RPC and native rendering probe for inert synthetic ButtonsMessage content.
#include "controller.h"
#include <QAccessible>
#include <QDir>
#include <QFileDevice>
#include <QFileInfo>
#include <QGuiApplication>
#include <QHash>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLocalServer>
#include <QLocalSocket>
#include <QMetaObject>
#include <QQuickItem>
#include <QQuickWindow>
#include <QTemporaryDir>
#include <QTimer>
#include <QDebug>
#include <memory>

namespace {
constexpr int MaxFrame = 256 * 1024;
const QString ChatID = QStringLiteral("fixture:buttons-chat");
const QString MessageID = QStringLiteral("fixture:incoming-buttons");
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

class SyntheticButtonsRPC final : public QObject {
public:
  explicit SyntheticButtonsRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticButtonsRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wfbtn-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private ButtonsMessage fixture directory");
      return false;
    }
    if (!QFile::setPermissions(m_temp->path(), QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner)) {
      m_error = QStringLiteral("cannot secure ButtonsMessage fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private ButtonsMessage fixture socket exceeds the AF_UNIX budget");
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
      m_error = QStringLiteral("ButtonsMessage fixture socket is not private");
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
  int buttonReplyCount() const { return m_buttonReplyCount; }
  QJsonObject buttonReplyParams() const { return m_buttonReplyParams; }

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
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic ButtonsMessage fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      reply(socket, id, {{"version", 1}, {"methods", QJsonArray{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set", "button.reply"}}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic buttons"}, {"initials", "SB"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject buttons{{"header", Header}, {"body", Body}, {"footer", Footer},
                                {"buttons", QJsonArray{FirstLabel, SecondLabel}}};
      const QJsonObject message{{"id", MessageID}, {"chat_id", ChatID}, {"body", Body},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "buttons"}, {"buttons", buttons},
                                {"button_reply_available", true}};
      const QJsonObject legacy{{"id", "fixture:legacy-buttons"}, {"chat_id", ChatID}, {"body", LegacyBody},
                               {"timestamp", 2000}, {"outgoing", false}, {"status", "received"}, {"kind", "text"}};
      reply(socket, id, {{"messages", QJsonArray{message, legacy}}, {"has_more", false}});
    } else if (method == QStringLiteral("draft.get")) {
      reply(socket, id, {{"text", ""}});
    } else if (method == QStringLiteral("draft.set")) {
      reply(socket, id, {{"saved", true}});
    } else if (method == QStringLiteral("button.reply")) {
      ++m_buttonReplyCount;
      m_buttonReplyParams = request.value(QStringLiteral("params")).toObject();
      const QJsonObject buttons{{"header", Header}, {"body", Body}, {"footer", Footer},
                                {"buttons", QJsonArray{FirstLabel, SecondLabel}}};
      const QJsonObject target{{"id", MessageID}, {"chat_id", ChatID}, {"body", Body},
                               {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                               {"kind", "buttons"}, {"buttons", buttons},
                               {"button_reply_available", false}, {"button_reply_state", "local"}};
      const QJsonObject response{{"id", "fixture:button-response"}, {"chat_id", ChatID},
                                 {"body", SecondLabel}, {"timestamp", 3000},
                                 {"outgoing", true}, {"status", "demo_sent"},
                                 {"kind", "text"}, {"reply_to_id", MessageID}};
      reply(socket, id, {{"target", target}, {"message", response}, {"status", "demo_sent"}});
    } else {
      ++m_unexpectedMethodCount;
      reply(socket, id, {{"unsupported", true}});
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  QJsonObject m_buttonReplyParams;
  int m_unexpectedMethodCount = 0;
  int m_buttonReplyCount = 0;
};
} // namespace

QObject *startButtonsFixture(QGuiApplication &, Controller &controller,
                             const QString &dataDir) {
  auto *fixture = new SyntheticButtonsRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private ButtonsMessage fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runButtonsTest(QGuiApplication &app, QQuickWindow *window,
                    Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticButtonsRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native ButtonsMessage UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int ticks = 0; int stage = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  const auto saveCapture = [window](const char *variable) {
    const auto path = qEnvironmentVariable(variable);
    if (path.isEmpty())
      return true;
    const auto image = window->grabWindow();
    return !image.isNull() && image.save(path);
  };
  QObject::connect(timer, &QTimer::timeout, &app,
                   [state, timer, window, controllerPtr = &controller, fixture, appPtr = &app,
                    saveCapture] {
    auto fail = [timer, appPtr](const QString &message) {
      qCritical().noquote() << "Native ButtonsMessage UI FAIL:" << message;
      timer->stop();
      appPtr->exit(1);
    };
    auto &controller = *controllerPtr;
    if (++state->ticks > 240) {
      fail(QStringLiteral("timed out waiting for synthetic ButtonsMessage action flow"));
      return;
    }
    if (!controller.connected() || !controller.capabilitiesReady() ||
        controller.selectedId() != ChatID || controller.loading())
      return;

    auto *surface = named(window, QStringLiteral("buttonsCardSurface"));
    auto *header = named(window, QStringLiteral("buttonsHeader"));
    auto *body = named(window, QStringLiteral("buttonsBody"));
    auto *footer = named(window, QStringLiteral("buttonsFooter"));
    auto *disclosure = named(window, QStringLiteral("buttonsDisclosure"));
    auto *first = named(window, QStringLiteral("buttonsLabel1"));
    auto *second = named(window, QStringLiteral("buttonsLabel2"));
    auto *legacy = namedWithText(window, QStringLiteral("messageBody"), QStringLiteral("Legacy &lt;b&gt;plain&lt;/b&gt; text"));
    auto *select = named(window, QStringLiteral("buttonsSelectButton"));
    auto *dialog = named(window, QStringLiteral("buttonsReplyDialog"));
    if (!surface || !surface->property("visible").toBool() || !header || !body || !footer ||
        !disclosure || !first || !second || !legacy || !select || !dialog) {
      if (controller.messageCount() < 2)
        return;
      fail(QStringLiteral("typed ButtonsMessage card, demo chooser, or legacy text was not rendered"));
      return;
    }

    if (state->stage == 0) {
      if (controller.messageCount() != 2 || header->property("text").toString() != Header ||
          body->property("text").toString() != Body || footer->property("text").toString() != Footer ||
          disclosure->property("text").toString() != QStringLiteral("Local demo only · choosing a response records it here; nothing is sent to WhatsApp.") ||
          first->property("text").toString() != FirstLabel || second->property("text").toString() != SecondLabel ||
          !select->property("visible").toBool() || dialog->property("opened").toBool() ||
          legacy->property("visible").toBool() != true) {
        fail(QStringLiteral("ButtonsMessage card or local-only chooser state is incorrect"));
        return;
      }
      for (auto *field : {header, body, footer, disclosure, first, second}) {
        if (field->property("textFormat").toInt() != 0) {
          fail(QStringLiteral("untrusted ButtonsMessage text must use Text.PlainText"));
          return;
        }
      }
      for (auto *label : {first, second}) {
        const auto background = label->property("background");
        if (!background.isValid() || !background.isNull()) {
          fail(QStringLiteral("display labels must remain inert, unstyled text"));
          return;
        }
        for (const auto *paddingName : {"leftPadding", "rightPadding", "topPadding", "bottomPadding"}) {
          const auto padding = label->property(paddingName);
          if (!padding.isValid() || padding.toDouble() != 0.0) {
            fail(QStringLiteral("display label has button-like padding"));
            return;
          }
        }
      }
      auto *accessible = QAccessible::queryAccessibleInterface(first);
      if (!accessible || accessible->text(QAccessible::Name) !=
                             QStringLiteral("Button label: ") + FirstLabel) {
        fail(QStringLiteral("ButtonsMessage label accessible name is missing"));
        return;
      }
      if (fixture->buttonReplyCount() != 0 || fixture->unexpectedMethodCount() != 0 ||
          named(window, QStringLiteral("buttonsActionButton"))) {
        fail(QStringLiteral("opening the card sent an action or exposed a legacy action"));
        return;
      }
      if (!saveCapture("WHATSFEATHER_BUTTONS_TEST_CAPTURE")) {
        fail(QStringLiteral("could not capture the synthetic ButtonsMessage card"));
        return;
      }
      if (!QMetaObject::invokeMethod(select, "clicked")) {
        fail(QStringLiteral("could not open the ButtonsMessage response chooser"));
        return;
      }
      state->stage = 1;
      return;
    }

    if (state->stage == 1) {
      if (!dialog->property("opened").toBool())
        return;
      auto *optionOne = named(window, QStringLiteral("buttonsReplyOption1"));
      auto *optionTwo = named(window, QStringLiteral("buttonsReplyOption2"));
      auto *cancel = named(window, QStringLiteral("cancelButtonsReply"));
      if (!optionOne || !optionTwo || !cancel ||
          optionOne->property("text").toString() != FirstLabel ||
          optionTwo->property("text").toString() != SecondLabel ||
          fixture->buttonReplyCount() != 0) {
        fail(QStringLiteral("chooser options are incorrect or cancellation already dispatched"));
        return;
      }
      if (!saveCapture("WHATSFEATHER_BUTTONS_DIALOG_CAPTURE")) {
        fail(QStringLiteral("could not capture the ButtonsMessage response chooser"));
        return;
      }
      if (!QMetaObject::invokeMethod(cancel, "clicked")) {
        fail(QStringLiteral("could not cancel the ButtonsMessage response chooser"));
        return;
      }
      state->stage = 2;
      return;
    }

    if (state->stage == 2) {
      if (dialog->property("opened").toBool())
        return;
      if (fixture->buttonReplyCount() != 0 || !select->property("visible").toBool()) {
        fail(QStringLiteral("cancel dispatched a reply or removed the available local action"));
        return;
      }
      if (!QMetaObject::invokeMethod(select, "clicked")) {
        fail(QStringLiteral("could not reopen the ButtonsMessage response chooser"));
        return;
      }
      state->stage = 3;
      return;
    }

    if (state->stage == 3) {
      if (!dialog->property("opened").toBool())
        return;
      auto *optionTwo = named(window, QStringLiteral("buttonsReplyOption2"));
      if (!optionTwo || !QMetaObject::invokeMethod(optionTwo, "clicked")) {
        fail(QStringLiteral("could not choose the second ButtonsMessage response"));
        return;
      }
      state->stage = 4;
      return;
    }

    if (state->stage == 4) {
      if (fixture->buttonReplyCount() != 1 || controller.messageCount() != 3)
        return;
      const auto params = fixture->buttonReplyParams();
      if (params.value("chat_id").toString() != ChatID ||
          params.value("target_id").toString() != MessageID ||
          !params.value("button_index").isDouble() || params.value("button_index").toInt() != 1 ||
          params.value("client_id").toString().isEmpty() ||
          params.contains("button_id") || params.contains("id")) {
        fail(QStringLiteral("RPC did not use a bounded display index or exposed a private button ID"));
        return;
      }
      const int targetIndex = controller.messages()->indexOf(MessageID);
      const int responseIndex = controller.messages()->indexOf(QStringLiteral("fixture:button-response"));
      if (targetIndex < 0 || responseIndex < 0) {
        fail(QStringLiteral("authoritative response did not reconcile both message rows"));
        return;
      }
      const auto target = controller.messages()->row(targetIndex);
      const auto response = controller.messages()->row(responseIndex);
      auto *status = named(window, QStringLiteral("buttonReplyStatus"));
      if (fixture->unexpectedMethodCount() != 0 || target.value("button_reply_state").toString() != "local" ||
          target.value("button_reply_available").toBool() || response.value("body").toString() != SecondLabel ||
          response.value("reply_to_id").toString() != MessageID || response.value("status").toString() != "demo_sent" ||
          disclosure->property("text").toString() != QStringLiteral("Local demo · response recorded here only.") ||
          select->property("visible").toBool() || !status || !status->property("visible").toBool() ||
          status->property("text").toString() != QStringLiteral("Recorded locally · not sent to WhatsApp")) {
        fail(QStringLiteral("local action result did not reconcile to a non-repeatable timeline state"));
        return;
      }
      if (controller.performMessageAction(QStringLiteral("button.reply"), target,
                                          {{"button_index", 0}}) || fixture->buttonReplyCount() != 1) {
        fail(QStringLiteral("a recorded response was not protected from duplicate selection"));
        return;
      }
      if (!saveCapture("WHATSFEATHER_BUTTONS_TIMELINE_CAPTURE")) {
        fail(QStringLiteral("could not capture the reconciled ButtonsMessage timeline"));
        return;
      }
      qInfo() << "Native ButtonsMessage PASS: explicit local selection, cancel-without-dispatch, bounded index RPC, no private button ID, authoritative reconciliation, PlainText and accessible labels; no WhatsApp send";
      timer->stop();
      appPtr->exit(0);
    }
  });
  timer->start();
}
