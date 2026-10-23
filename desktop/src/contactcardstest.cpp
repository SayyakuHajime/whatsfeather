// Private same-UID RPC and native UI fixture for contact-card review/share.
#include "controller.h"
#include <QDir>
#include <QFile>
#include <QFileDevice>
#include <QFileInfo>
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
const QString ChatID = QStringLiteral("fixture:contact-chat");
const QString ContactID = QStringLiteral("fixture:incoming-contact");
const QString UntrustedName = QStringLiteral("<b>Ada</b> https://example.invalid");
const QString UntrustedPhone = QStringLiteral("<a href=\"https://example.invalid\">555</a>");
const QString UntrustedEmail = QStringLiteral("<img src=\"file:///etc/passwd\">");
const QString UntrustedOrganization = QStringLiteral("<i>Analytical</i>");

QObject *named(QObject *root, const QString &name) {
  if (!root)
    return nullptr;
  if (auto *window = qobject_cast<QQuickWindow *>(root)) {
    if (auto *visual = named(window->contentItem(), name))
      return visual;
  }
  if (root->objectName() == name)
    return root;
  if (auto *item = qobject_cast<QQuickItem *>(root)) {
    for (auto *child : item->childItems())
      if (auto *found = named(child, name))
        return found;
  }
  for (auto *child : root->children())
    if (auto *found = named(child, name))
      return found;
  return nullptr;
}

bool invoke(QQuickWindow *window, const QString &name, const char *method) {
  auto *object = named(window, name);
  return object && QMetaObject::invokeMethod(object, method);
}

class SyntheticContactRPC final : public QObject {
public:
  explicit SyntheticContactRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticContactRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wf-contact-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private contact fixture directory");
      return false;
    }
    const auto privateMode = QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner;
    if (!QFile::setPermissions(m_temp->path(), privateMode)) {
      m_error = QStringLiteral("cannot secure contact fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    m_vcardPath = m_temp->path() + QStringLiteral("/chosen.vcf");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private fixture socket path exceeds the AF_UNIX budget");
      return false;
    }
    QFile card(m_vcardPath);
    const QByteArray syntheticCard =
        "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Selected Contact\r\nEND:VCARD\r\n";
    if (!card.open(QIODevice::WriteOnly) ||
        card.write(syntheticCard) != syntheticCard.size() || !card.flush()) {
      m_error = QStringLiteral("cannot create synthetic selected vCard");
      return false;
    }
    card.close();
    if (!QFile::setPermissions(m_vcardPath,
                               QFileDevice::ReadOwner | QFileDevice::WriteOwner)) {
      m_error = QStringLiteral("cannot restrict synthetic vCard permissions");
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
            const auto line = buffer.left(newline);
            buffer.remove(0, newline + 1);
            const auto request = QJsonDocument::fromJson(line).object();
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
  QString selectedFile() const { return m_vcardPath; }
  QString error() const { return m_error; }
  int prepareCount() const { return m_prepareCount; }
  int sendCount() const { return m_sendCount; }
  QJsonObject sendParams() const { return m_sendParams; }

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
    const auto params = request.value(QStringLiteral("params")).toObject();
    if (method == QStringLiteral("app.status")) {
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic contact fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      reply(socket, id, {{"version", 1},
                         {"methods", QJsonArray{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set", "contact.prepare", "message.contact.send"}},
                         {"features", QJsonObject{{"contact-cards", QJsonObject{{"available", true}, {"reason", "Synthetic contact-card fixture"}}}}}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic recipient"}, {"initials", "S"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject contact{{"id", ContactID}, {"chat_id", ChatID}, {"body", ""},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "contact"},
                                {"contacts", QJsonArray{QJsonObject{{"name", UntrustedName},
                                                                     {"phones", QJsonArray{UntrustedPhone}},
                                                                     {"emails", QJsonArray{UntrustedEmail}},
                                                                     {"organization", "<i>Analytical</i>"}}}}};
      reply(socket, id, {{"messages", QJsonArray{contact}}, {"has_more", false}});
    } else if (method == QStringLiteral("draft.get")) {
      reply(socket, id, {{"text", ""}});
    } else if (method == QStringLiteral("draft.set")) {
      reply(socket, id, {{"saved", true}});
    } else if (method == QStringLiteral("contact.prepare")) {
      ++m_prepareCount;
      if (params.value(QStringLiteral("path")).toString() != m_vcardPath) {
        reply(socket, id, {{"error", "unexpected fixture path"}});
        return;
      }
      reply(socket, id, {{"contacts", QJsonArray{QJsonObject{{"name", "Selected <b>Contact</b>"},
                                                            {"phones", QJsonArray{"+1 555 0100"}},
                                                            {"emails", QJsonArray{"selected@example.test"}},
                                                            {"organization", "Northwind & Associates"}}}}});
    } else if (method == QStringLiteral("message.contact.send")) {
      ++m_sendCount;
      m_sendParams = params;
      const auto message = QJsonObject{{"id", params.value("client_id")},
                                       {"chat_id", params.value("chat_id")},
                                       {"body", ""}, {"timestamp", 2000},
                                       {"outgoing", true}, {"status", "demo_sent"},
                                       {"kind", "contact"}, {"contacts", params.value("contacts")}};
      reply(socket, id, {{"message", message}});
    } else {
      reply(socket, id, {{"unsupported", true}});
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_vcardPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  int m_prepareCount = 0, m_sendCount = 0;
  QJsonObject m_sendParams;
};
} // namespace

QObject *startContactCardFixture(QGuiApplication &, Controller &controller,
                                 const QString &dataDir) {
  auto *fixture = new SyntheticContactRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private contact-card fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runContactCardTest(QGuiApplication &app, QQuickWindow *window,
                        Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticContactRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native contact-card UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int stage = 0; int ticks = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [state, timer, window, controllerPtr = &controller,
                    fixture, appPtr = &app] {
    auto fail = [timer, appPtr](const QString &message) {
      qCritical().noquote() << "Native contact-card UI FAIL:" << message;
      timer->stop();
      appPtr->exit(1);
    };
    auto &controller = *controllerPtr;
    if (++state->ticks > 240) {
      fail(QStringLiteral("timed out at stage %1").arg(state->stage));
      return;
    }
    if (state->stage == 0) {
      if (!controller.connected() || !controller.capabilitiesReady() ||
          controller.selectedId() != ChatID || controller.loading() ||
          controller.messageCount() != 1)
        return;
      if (!controller.hasCapability(QStringLiteral("contact.prepare")) ||
          !controller.hasCapability(QStringLiteral("message.contact.send"))) {
        fail(QStringLiteral("fixture did not expose contact-card capabilities"));
        return;
      }
      auto *surface = named(window, QStringLiteral("contactCardSurface"));
      auto *name = named(window, QStringLiteral("contactCardName"));
      auto *phone = named(window, QStringLiteral("contactCardPhone"));
      auto *email = named(window, QStringLiteral("contactCardEmail"));
      auto *organization = named(window, QStringLiteral("contactCardOrganization"));
      if (!surface || !surface->property("visible").toBool() || !name || !phone || !email || !organization) {
        fail(QStringLiteral("incoming native contact card or its fields were not rendered"));
        return;
      }
      if (name->property("text").toString() != UntrustedName ||
          phone->property("text").toString() != QStringLiteral("Phone · ") + UntrustedPhone ||
          email->property("text").toString() != QStringLiteral("Email · ") + UntrustedEmail ||
          organization->property("text").toString() != QStringLiteral("Organization · ") + UntrustedOrganization) {
        fail(QStringLiteral("untrusted contact values changed during rendering"));
        return;
      }
      for (auto *field : {name, phone, email, organization}) {
        if (field->property("textFormat").toInt() != 0) {
          fail(QStringLiteral("contact fields must be rendered as Text.PlainText"));
          return;
        }
      }
      if (!invoke(window, QStringLiteral("attachmentButton"), "clicked")) {
        fail(QStringLiteral("attachment menu did not open"));
        return;
      }
      state->stage = 1;
      return;
    }
    if (state->stage == 1) {
      auto *action = named(window, QStringLiteral("contactCardAttachmentAction"));
      if (!action || !action->property("enabled").toBool()) {
        fail(QStringLiteral("explicit contact-card file action is missing or disabled"));
        return;
      }
      if (!QMetaObject::invokeMethod(action, "triggered")) {
        fail(QStringLiteral("contact-card file action did not open the picker"));
        return;
      }
      state->stage = 2;
      return;
    }
    if (state->stage == 2) {
      auto *picker = named(window, QStringLiteral("contactCardFilePicker"));
      if (!picker || !picker->property("visible").toBool())
        return;
      const auto filters = picker->property("nameFilters").toStringList().join(' ');
      if (!picker->property("title").toString().contains(QStringLiteral("vCard"), Qt::CaseInsensitive) ||
          picker->property("fileMode").toInt() != 0 ||
          !filters.contains(QStringLiteral(".vcf"), Qt::CaseInsensitive) ||
          fixture->prepareCount() != 0 || fixture->sendCount() != 0) {
        fail(QStringLiteral("picker was not an explicit .vcf chooser or accessed data early"));
        return;
      }
      QMetaObject::invokeMethod(picker, "close");
      controller.stageContactFile(QUrl::fromLocalFile(fixture->selectedFile()));
      state->stage = 3;
      return;
    }
    if (state->stage == 3) {
      if (fixture->prepareCount() != 1 || controller.contactPreparing())
        return;
      auto *dialog = named(window, QStringLiteral("contactShareDialog"));
      auto *name = named(window, QStringLiteral("contactReviewName"));
      auto *phone = named(window, QStringLiteral("contactReviewPhone"));
      auto *email = named(window, QStringLiteral("contactReviewEmail"));
      auto *organization = named(window, QStringLiteral("contactReviewOrganization"));
      auto *send = named(window, QStringLiteral("contactShareSend"));
      if (!dialog || !dialog->property("opened").toBool() || !name || !phone || !email || !organization || !send) {
        fail(QStringLiteral("parsed contact review dialog did not open"));
        return;
      }
      if (name->property("text").toString() != QStringLiteral("Selected <b>Contact</b>") ||
          phone->property("text").toString() != QStringLiteral("Phone · +1 555 0100") ||
          email->property("text").toString() != QStringLiteral("Email · selected@example.test") ||
          organization->property("text").toString() != QStringLiteral("Organization · Northwind & Associates")) {
        fail(QStringLiteral("review did not show the parsed local file projection"));
        return;
      }
      for (auto *field : {name, phone, email, organization}) {
        if (field->property("textFormat").toInt() != 0) {
          fail(QStringLiteral("review fields must use Text.PlainText"));
          return;
        }
      }
      if (fixture->sendCount() != 0 || !send->property("enabled").toBool()) {
        fail(QStringLiteral("review auto-sent or did not enable explicit Send"));
        return;
      }
      if (!invoke(window, QStringLiteral("contactShareSend"), "clicked")) {
        fail(QStringLiteral("explicit contact Send button did not activate"));
        return;
      }
      const auto originalChat = controller.selectedId();
      controller.selectChat(QStringLiteral("fixture:other-chat"));
      if (controller.selectedId() != originalChat) {
        fail(QStringLiteral("conversation changed while the typed contact send was in flight"));
        return;
      }
      state->stage = 4;
      return;
    }
    if (state->stage == 4) {
      if (fixture->sendCount() == 0 || controller.contactSending())
        return;
      const auto params = fixture->sendParams();
      const auto contacts = params.value(QStringLiteral("contacts")).toArray();
      if (fixture->sendCount() != 1 || params.value(QStringLiteral("chat_id")).toString() != controller.selectedId() ||
          params.value(QStringLiteral("client_id")).toString().isEmpty() || contacts.size() != 1 ||
          contacts[0].toObject().value(QStringLiteral("name")).toString() != QStringLiteral("Selected <b>Contact</b>") ||
          params.contains(QStringLiteral("vcard")) || params.contains(QStringLiteral("path"))) {
        fail(QStringLiteral("outgoing RPC did not contain one typed reviewed projection and a stable client ID"));
        return;
      }
      if (controller.messageCount() != 2 ||
          controller.messages()->row(1).value(QStringLiteral("kind")).toString() != QStringLiteral("contact") ||
          !controller.contactCards().isEmpty()) {
        fail(QStringLiteral("accepted local share did not reach the timeline or clear the review"));
        return;
      }
      qInfo() << "Native contact-card PASS: incoming untrusted fields are plain text; explicit local .vcf selection, parsed review, and typed local send are wired";
      timer->stop();
      appPtr->exit(0);
    }
  });
  timer->start();
}
