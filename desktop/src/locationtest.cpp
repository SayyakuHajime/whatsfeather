// Private same-UID RPC and native rendering probe for synthetic location snapshots.
#include "controller.h"
#include <QDir>
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
const QString ChatID = QStringLiteral("fixture:location-chat");
const QString MessageID = QStringLiteral("fixture:incoming-location");
const QString Caption = QStringLiteral("<b>Snapshot caption</b>");
const QString Name = QStringLiteral("Synthetic <script>place</script>");
const QString Address = QStringLiteral("Northwind & West");

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

bool invoke(QQuickWindow *window, const QString &name, const char *method) {
  auto *object = named(window, name);
  return object && QMetaObject::invokeMethod(object, method);
}

class SyntheticLocationRPC final : public QObject {
public:
  explicit SyntheticLocationRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticLocationRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wf-location-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private location fixture directory");
      return false;
    }
    if (!QFile::setPermissions(m_temp->path(), QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner)) {
      m_error = QStringLiteral("cannot secure location fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private location fixture socket exceeds the AF_UNIX budget");
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
      m_error = QStringLiteral("location fixture socket is not private");
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

  void replyError(QLocalSocket *socket, const QString &id, const QJsonObject &error) {
    const auto frame = QJsonDocument(QJsonObject{{"v", 1}, {"id", id}, {"error", error}})
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
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic location fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      reply(socket, id, {{"version", 1},
                         {"methods", QJsonArray{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set", "message.location.send"}},
                         {"features", QJsonObject{{"location-messages", QJsonObject{{"available", true}, {"reason", "Synthetic static location fixture"}}}}}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic location"}, {"initials", "SL"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject projection{{"latitude", 47.6}, {"longitude", -122.3}, {"is_live", true},
                                  {"caption", Caption}, {"name", Name}, {"address", Address}};
      const QJsonObject message{{"id", MessageID}, {"chat_id", ChatID}, {"body", "Location"},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "location"}, {"location", projection}};
      reply(socket, id, {{"messages", QJsonArray{message}}, {"has_more", false}});
    } else if (method == QStringLiteral("draft.get")) {
      reply(socket, id, {{"text", ""}});
    } else if (method == QStringLiteral("draft.set")) {
      reply(socket, id, {{"saved", true}});
    } else if (method == QStringLiteral("message.location.send")) {
      ++m_sendCount;
      m_sendParams = params;
      if (m_sendCount > 1) {
        replyError(socket, id, {{"code", "unknown_outcome"}, {"message", "Synthetic uncertain result"}});
        return;
      }
      const QJsonObject location{{"latitude", params.value("latitude")},
                                 {"longitude", params.value("longitude")},
                                 {"is_live", false}, {"caption", params.value("caption")},
                                 {"name", params.value("name")}, {"address", params.value("address")}};
      const QJsonObject message{{"id", params.value("client_id")}, {"chat_id", params.value("chat_id")},
                                {"body", "Location"}, {"timestamp", 2000}, {"outgoing", true},
                                {"status", "demo_sent"}, {"kind", "location"}, {"location", location}};
      reply(socket, id, {{"message", message}});
    } else {
      reply(socket, id, {{"unsupported", true}});
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  int m_sendCount = 0;
  QJsonObject m_sendParams;
};
} // namespace

QObject *startLocationFixture(QGuiApplication &, Controller &controller,
                              const QString &dataDir) {
  auto *fixture = new SyntheticLocationRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private location fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runLocationTest(QGuiApplication &app, QQuickWindow *window,
                     Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticLocationRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native location UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int ticks = 0; int stage = 0; int unknownWait = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [state, timer, window, controllerPtr = &controller, fixture, appPtr = &app] {
    auto fail = [timer, appPtr](const QString &message) {
      qCritical().noquote() << "Native location UI FAIL:" << message;
      timer->stop();
      appPtr->exit(1);
    };
    auto &controller = *controllerPtr;
    if (++state->ticks > 240) {
      fail(QStringLiteral("timed out waiting for synthetic location card"));
      return;
    }
    if (state->stage == 0 && (!controller.connected() || !controller.capabilitiesReady() ||
        controller.selectedId() != ChatID || controller.loading() || controller.messageCount() != 1))
      return;
    if (state->stage == 0) {
    auto *surface = named(window, QStringLiteral("locationCardSurface"));
    auto *snapshot = named(window, QStringLiteral("locationSnapshotLabel"));
    auto *coordinates = named(window, QStringLiteral("locationCoordinates"));
    auto *caption = named(window, QStringLiteral("locationCaption"));
    auto *name = named(window, QStringLiteral("locationName"));
    auto *address = named(window, QStringLiteral("locationAddress"));
    if (!surface || !surface->property("visible").toBool() || !snapshot || !coordinates || !caption || !name || !address) {
      fail(QStringLiteral("incoming typed location card or a plain-text field was not rendered"));
      return;
    }
    if (snapshot->property("text").toString() != QStringLiteral("Live location · received snapshot") ||
        !coordinates->property("text").toString().contains(QStringLiteral("47.600000")) ||
        !coordinates->property("text").toString().contains(QStringLiteral("-122.300000")) ||
        caption->property("text").toString() != Caption || name->property("text").toString() != Name ||
        address->property("text").toString() != QStringLiteral("Address · ") + Address) {
      fail(QStringLiteral("location snapshot labels or coordinates were altered"));
      return;
    }
    for (auto *field : {snapshot, coordinates, caption, name, address}) {
      if (field->property("textFormat").toInt() != 0) {
        fail(QStringLiteral("location content must use Text.PlainText"));
        return;
      }
    }
    if (named(window, QStringLiteral("openLocationMapButton"))) {
      fail(QStringLiteral("location card must not offer external map navigation"));
      return;
    }
    if (!invoke(window, QStringLiteral("attachmentButton"), "clicked")) {
      fail(QStringLiteral("composer attachment menu did not open"));
      return;
    }
    state->stage = 1;
    return;
    }
    if (state->stage == 1) {
      auto *action = named(window, QStringLiteral("locationAttachmentAction"));
      if (!action || !action->property("enabled").toBool()) {
        fail(QStringLiteral("explicit manual location composer action is missing or disabled"));
        return;
      }
      if (!QMetaObject::invokeMethod(action, "triggered")) {
        fail(QStringLiteral("manual location action did not open its entry dialog"));
        return;
      }
      state->stage = 2;
      return;
    }
    if (state->stage == 2) {
      auto *dialog = named(window, QStringLiteral("locationEntryDialog"));
      auto *latitude = named(window, QStringLiteral("locationLatitudeInput"));
      auto *longitude = named(window, QStringLiteral("locationLongitudeInput"));
      auto *caption = named(window, QStringLiteral("locationCaptionInput"));
      auto *name = named(window, QStringLiteral("locationNameInput"));
      auto *address = named(window, QStringLiteral("locationAddressInput"));
      auto *review = named(window, QStringLiteral("locationReviewButton"));
      if (!dialog || !dialog->property("opened").toBool() || !latitude || !longitude || !caption || !name || !address || !review)
        return;
      latitude->setProperty("text", QStringLiteral("91"));
      longitude->setProperty("text", QStringLiteral("-122.3"));
      caption->setProperty("text", QStringLiteral("Manual <b>caption</b>"));
      name->setProperty("text", QStringLiteral("Synthetic place"));
      address->setProperty("text", QStringLiteral("Northwind & West"));
      if (!invoke(window, QStringLiteral("locationReviewButton"), "clicked")) {
        fail(QStringLiteral("location review action was unavailable"));
        return;
      }
      auto *localError = named(window, QStringLiteral("locationShareError"));
      if (dialog->property("reviewing").toBool() || fixture->sendCount() != 0 || !localError ||
          !localError->property("text").toString().contains(QStringLiteral("finite latitude"))) {
        fail(QStringLiteral("out-of-range manual coordinates were not rejected before review or dispatch"));
        return;
      }
      latitude->setProperty("text", QStringLiteral("47.6"));
      if (!invoke(window, QStringLiteral("locationReviewButton"), "clicked") || !dialog->property("reviewing").toBool() || fixture->sendCount() != 0) {
        fail(QStringLiteral("valid manual location did not require an explicit review step"));
        return;
      }
      state->stage = 3;
      return;
    }
    if (state->stage == 3) {
      auto *reviewCoordinates = named(window, QStringLiteral("locationReviewCoordinates"));
      auto *reviewCaption = named(window, QStringLiteral("locationReviewCaption"));
      auto *reviewName = named(window, QStringLiteral("locationReviewName"));
      auto *reviewAddress = named(window, QStringLiteral("locationReviewAddress"));
      auto *send = named(window, QStringLiteral("locationSendButton"));
      if (!reviewCoordinates || !reviewCaption || !reviewName || !reviewAddress || !send)
        return;
      if (!reviewCoordinates->property("text").toString().contains(QStringLiteral("47.600000")) ||
          !reviewCoordinates->property("text").toString().contains(QStringLiteral("-122.300000")) ||
          reviewCaption->property("text").toString() != QStringLiteral("Manual <b>caption</b>") ||
          reviewName->property("text").toString() != QStringLiteral("Synthetic place") ||
          reviewAddress->property("text").toString() != QStringLiteral("Northwind & West")) {
        fail(QStringLiteral("manual location review did not show the entered coordinates and optional fields"));
        return;
      }
      for (auto *field : {reviewCoordinates, reviewCaption, reviewName, reviewAddress})
        if (field->property("textFormat").toInt() != 0) {
          fail(QStringLiteral("manual location review must use Text.PlainText"));
          return;
        }
      if (fixture->sendCount() != 0 || !send->property("enabled").toBool() ||
          !invoke(window, QStringLiteral("locationSendButton"), "clicked")) {
        fail(QStringLiteral("location review auto-sent or explicit Send was unavailable"));
        return;
      }
      state->stage = 4;
      return;
    }
    if (state->stage == 4) {
      if (fixture->sendCount() == 0 || controller.messageCount() != 2)
        return;
      const auto params = fixture->sendParams();
      const auto clientID = params.value(QStringLiteral("client_id")).toString();
      if (fixture->sendCount() != 1 || params.value(QStringLiteral("chat_id")).toString() != ChatID ||
          params.value(QStringLiteral("latitude")).toDouble() != 47.6 ||
          params.value(QStringLiteral("longitude")).toDouble() != -122.3 ||
          params.value(QStringLiteral("reviewed")).toBool() != true || clientID.isEmpty() ||
          params.value(QStringLiteral("caption")).toString() != QStringLiteral("Manual <b>caption</b>") ||
          params.contains(QStringLiteral("url")) || params.contains(QStringLiteral("thumbnail"))) {
        fail(QStringLiteral("outgoing RPC did not contain one reviewed manual coordinate projection and stable client ID"));
        return;
      }
      const auto message = controller.messages()->row(1);
      const auto location = message.value(QStringLiteral("location")).toMap();
      if (message.value(QStringLiteral("id")).toString() != clientID ||
          message.value(QStringLiteral("kind")).toString() != QStringLiteral("location") ||
          message.value(QStringLiteral("status")).toString() != QStringLiteral("demo_sent") ||
          location.value(QStringLiteral("is_live")).toBool() ||
          location.value(QStringLiteral("latitude")).toDouble() != 47.6) {
        fail(QStringLiteral("accepted manual location did not appear as a static local timeline message"));
        return;
      }
      if (!invoke(window, QStringLiteral("attachmentButton"), "clicked")) {
        fail(QStringLiteral("composer did not reopen after accepted static send"));
        return;
      }
      state->stage = 5;
      return;
    }
    if (state->stage == 5) {
      auto *action = named(window, QStringLiteral("locationAttachmentAction"));
      if (!action || !action->property("enabled").toBool() || !QMetaObject::invokeMethod(action, "triggered"))
        return;
      state->stage = 6;
      return;
    }
    if (state->stage == 6) {
      auto *dialog = named(window, QStringLiteral("locationEntryDialog"));
      auto *latitude = named(window, QStringLiteral("locationLatitudeInput"));
      auto *longitude = named(window, QStringLiteral("locationLongitudeInput"));
      auto *review = named(window, QStringLiteral("locationReviewButton"));
      if (!dialog || !dialog->property("opened").toBool() || !latitude || !longitude || !review)
        return;
      latitude->setProperty("text", QStringLiteral("40.7"));
      longitude->setProperty("text", QStringLiteral("-74.0"));
      if (fixture->sendCount() != 1 || !invoke(window, QStringLiteral("locationReviewButton"), "clicked")) {
        fail(QStringLiteral("second manual share sent before review"));
        return;
      }
      state->stage = 7;
      return;
    }
    if (state->stage == 7) {
      auto *send = named(window, QStringLiteral("locationSendButton"));
      if (!send)
        return;
      if (fixture->sendCount() != 1 || !send->property("enabled").toBool() ||
          !invoke(window, QStringLiteral("locationSendButton"), "clicked")) {
        fail(QStringLiteral("second explicitly reviewed share did not dispatch exactly once"));
        return;
      }
      state->stage = 8;
      return;
    }
    if (state->stage == 8) {
      if (fixture->sendCount() != 2 || !controller.locationOutcomeUnknown() || controller.locationSending())
        return;
      auto *error = named(window, QStringLiteral("locationShareError"));
      auto *send = named(window, QStringLiteral("locationSendButton"));
      if (!error || !error->property("text").toString().contains(QStringLiteral("not be sent again automatically")) ||
          !send || send->property("enabled").toBool()) {
        fail(QStringLiteral("ambiguous native outcome was not preserved with sending disabled"));
        return;
      }
      if (++state->unknownWait < 8)
        return;
      if (fixture->sendCount() != 2 || !invoke(window, QStringLiteral("locationShareCancel"), "clicked")) {
        fail(QStringLiteral("ambiguous outcome was automatically replayed or could not be dismissed"));
        return;
      }
      state->stage = 9;
      return;
    }
    if (state->stage == 9) {
      if (!invoke(window, QStringLiteral("attachmentButton"), "clicked"))
        return;
      auto *action = named(window, QStringLiteral("locationAttachmentAction"));
      if (!action || action->property("enabled").toBool() || fixture->sendCount() != 2) {
        fail(QStringLiteral("unknown outcome did not remain sticky after dismissal or caused a replay"));
        return;
      }
      qInfo() << "Native location PASS: live snapshot is plain text; composer requires manual entry, explicit review and Send; one static typed send reaches the timeline; unknown outcome stays sticky and is never replayed";
      timer->stop();
      appPtr->exit(0);
    }
  });
  timer->start();
}
