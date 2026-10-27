// Private in-process RPC fixture and keyboard interaction probe for incoming media.
#include "controller.h"
#include <QCoreApplication>
#include <QDesktopServices>
#include <QDebug>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QGuiApplication>
#include <QImageReader>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QKeyEvent>
#include <QLocalServer>
#include <QLocalSocket>
#include <QPointer>
#include <QQuickItem>
#include <QQuickWindow>
#include <QSaveFile>
#include <QTemporaryDir>
#include <QTimer>
#include <QUrl>
#include <functional>
#include <memory>
#include <unistd.h>

namespace {
constexpr qint64 MaxFrame = 256 * 1024;
const QString ChatID = QStringLiteral("fixture:incoming-chat");
const QString MessageID = QStringLiteral("fixture:incoming-document");
const QString StickerID = QStringLiteral("fixture:incoming-sticker");
const QString ImageID = QStringLiteral("fixture:incoming-image");

QObject *named(QQuickWindow *window, const QString &name, bool visibleOnly = false) {
  std::function<QQuickItem *(QQuickItem *)> visit =
      [&](QQuickItem *item) -> QQuickItem * {
    if (item->objectName() == name && (!visibleOnly || item->isVisible()))
      return item;
    for (auto *child : item->childItems())
      if (auto *found = visit(child))
        return found;
    return nullptr;
  };
  if (auto *item = visit(window->contentItem()))
    return item;
  auto *object = window->findChild<QObject *>(name);
  if (!object || !visibleOnly)
    return object;
  const auto visible = object->property("visible");
  return visible.isValid() && visible.toBool() ? object : nullptr;
}

QString readText(const QString &path) {
  QFile file(path);
  if (!file.open(QIODevice::ReadOnly | QIODevice::Text))
    return {};
  return QString::fromUtf8(file.readAll()).trimmed();
}

bool writeText(const QString &path, const QString &value) {
  QSaveFile file(path);
  if (!file.open(QIODevice::WriteOnly | QIODevice::Text))
    return false;
  if (file.write(value.toUtf8()) < 0)
    return false;
  return file.commit();
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

class SyntheticFileOpener final : public QObject {
  Q_OBJECT
public:
  explicit SyntheticFileOpener(QObject *parent = nullptr) : QObject(parent) {
    QDesktopServices::setUrlHandler(QStringLiteral("file"), this, "open");
  }
  ~SyntheticFileOpener() override {
    QDesktopServices::unsetUrlHandler(QStringLiteral("file"));
  }
  int openedCount() const { return m_openedCount; }
  QUrl lastUrl() const { return m_lastUrl; }

public slots:
  void open(const QUrl &url) {
    ++m_openedCount;
    m_lastUrl = url;
  }

private:
  int m_openedCount = 0;
  QUrl m_lastUrl;
};

class SyntheticMediaRPC final : public QObject {
public:
  explicit SyntheticMediaRPC(QObject *parent = nullptr)
      : QObject(parent),
        m_kind(qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_KIND",
                                   QStringLiteral("document")).trimmed().toLower()) {}

  ~SyntheticMediaRPC() override {
    QObject::disconnect(&m_server, nullptr, this, nullptr);
    m_server.close();
    const auto sockets = m_server.findChildren<QLocalSocket *>();
    for (auto *socket : sockets) {
      QObject::disconnect(socket, nullptr, this, nullptr);
      socket->abort();
    }
  }

  bool start() {
    if (m_kind != QStringLiteral("document") && m_kind != QStringLiteral("video") &&
        m_kind != QStringLiteral("sticker") && m_kind != QStringLiteral("image")) {
      m_error = QStringLiteral("unsupported synthetic media kind");
      return false;
    }
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + "/wf-md-XXXXXX");
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private fixture directory");
      return false;
    }
    const auto privateDirectoryPermissions = QFileDevice::ReadOwner |
                                             QFileDevice::WriteOwner |
                                             QFileDevice::ExeOwner;
    if (!QFile::setPermissions(m_temp->path(), privateDirectoryPermissions)) {
      m_error = QStringLiteral("cannot restrict private fixture directory");
      return false;
    }
    m_profilePath = QDir(m_temp->path()).filePath(QStringLiteral("profile"));
    const QString mediaDirectory =
        QDir(m_profilePath).filePath(QStringLiteral("media"));
    if (!QDir().mkpath(mediaDirectory) ||
        !QFile::setPermissions(m_profilePath, privateDirectoryPermissions) ||
        !QFile::setPermissions(mediaDirectory, privateDirectoryPermissions)) {
      m_error = QStringLiteral("cannot create private profile media cache");
      return false;
    }
    const QFileInfo tempInfo(m_temp->path());
    const QFileInfo profileInfo(m_profilePath);
    const QFileInfo mediaDirectoryInfo(mediaDirectory);
    const auto sharedDirectoryPermissions =
        QFileDevice::ReadGroup | QFileDevice::WriteGroup |
        QFileDevice::ExeGroup | QFileDevice::ReadOther |
        QFileDevice::WriteOther | QFileDevice::ExeOther;
    if (tempInfo.isSymLink() || !tempInfo.isDir() ||
        profileInfo.isSymLink() || !profileInfo.isDir() ||
        mediaDirectoryInfo.isSymLink() || !mediaDirectoryInfo.isDir() ||
        tempInfo.ownerId() != static_cast<uint>(::geteuid()) ||
        profileInfo.ownerId() != tempInfo.ownerId() ||
        mediaDirectoryInfo.ownerId() != tempInfo.ownerId() ||
        (tempInfo.permissions() & sharedDirectoryPermissions) ||
        (profileInfo.permissions() & sharedDirectoryPermissions) ||
        (mediaDirectoryInfo.permissions() & sharedDirectoryPermissions)) {
      m_error = QStringLiteral("private profile media cache permissions are unsafe");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    m_mediaPath = QDir(mediaDirectory).filePath(
        isVideo() ? QStringLiteral("synthetic-video.mp4")
        : isSticker() ? QStringLiteral("synthetic-sticker.webp")
        : isImage() ? QStringLiteral("synthetic-image.png")
                    : QStringLiteral("synthetic-document.pdf"));
    QFile media(m_mediaPath);
    if (isVideo()) {
      const QString fixturePath = qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_FIXTURE");
      const QFileInfo sourceInfo(fixturePath);
      const QString runtimePath = qEnvironmentVariable("XDG_RUNTIME_DIR");
      const QFileInfo runtimeInfo(runtimePath);
      const auto runtimeForbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                                    QFileDevice::ExeGroup | QFileDevice::ReadOther |
                                    QFileDevice::WriteOther | QFileDevice::ExeOther;
      constexpr qint64 MaxFixtureBytes = 5 * 1024 * 1024;
      if (fixturePath.isEmpty() || !sourceInfo.isFile() || sourceInfo.isSymLink() ||
          sourceInfo.suffix().compare(QStringLiteral("mp4"), Qt::CaseInsensitive) != 0 ||
          runtimePath.isEmpty() || !runtimeInfo.isDir() || runtimeInfo.isSymLink() ||
          (runtimeInfo.permissions() & runtimeForbidden) ||
          sourceInfo.canonicalPath() != runtimeInfo.canonicalFilePath() ||
          sourceInfo.size() <= 0 || sourceInfo.size() > MaxFixtureBytes) {
        m_error = QStringLiteral("video fixture must be a bounded MP4 inside a private XDG runtime directory");
        return false;
      }
      QFile source(fixturePath);
      if (!source.open(QIODevice::ReadOnly) ||
          !media.open(QIODevice::WriteOnly | QIODevice::NewOnly)) {
        m_error = QStringLiteral("cannot open synthetic video fixture");
        return false;
      }
      const QByteArray bytes = source.readAll();
      if (source.error() != QFileDevice::NoError || bytes.size() != sourceInfo.size() ||
          media.write(bytes) != bytes.size() || !media.flush()) {
        m_error = QStringLiteral("cannot copy complete synthetic video fixture");
        return false;
      }
    } else if (isImage()) {
      const QString fixturePath = qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_FIXTURE");
      const QFileInfo sourceInfo(fixturePath);
      const QString runtimePath = qEnvironmentVariable("XDG_RUNTIME_DIR");
      const QFileInfo runtimeInfo(runtimePath);
      const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                             QFileDevice::ExeGroup | QFileDevice::ReadOther |
                             QFileDevice::WriteOther | QFileDevice::ExeOther;
      constexpr qint64 MaxFixtureBytes = 5 * 1024 * 1024;
      if (fixturePath.isEmpty() || !sourceInfo.isFile() || sourceInfo.isSymLink() ||
          sourceInfo.suffix().compare(QStringLiteral("png"), Qt::CaseInsensitive) != 0 ||
          runtimePath.isEmpty() || !runtimeInfo.isDir() || runtimeInfo.isSymLink() ||
          (runtimeInfo.permissions() & forbidden) ||
          (sourceInfo.permissions() & forbidden) ||
          !(sourceInfo.permissions() & QFileDevice::ReadOwner) ||
          sourceInfo.canonicalPath() != runtimeInfo.canonicalFilePath() ||
          sourceInfo.size() <= 0 || sourceInfo.size() > MaxFixtureBytes) {
        m_error = QStringLiteral("image fixture must be a bounded PNG inside a private XDG runtime directory");
        return false;
      }
      QFile source(fixturePath);
      if (!source.open(QIODevice::ReadOnly) ||
          !media.open(QIODevice::WriteOnly | QIODevice::NewOnly)) {
        m_error = QStringLiteral("cannot open synthetic image fixture");
        return false;
      }
      QImageReader reader(&source);
      reader.setDecideFormatFromContent(true);
      if (reader.format().toLower() != QByteArrayLiteral("png") ||
          reader.size() != QSize(64, 48) || !source.seek(0)) {
        m_error = QStringLiteral("synthetic image fixture is not a 64 by 48 PNG");
        return false;
      }
      const QByteArray bytes = source.readAll();
      if (source.error() != QFileDevice::NoError || bytes.size() != sourceInfo.size() ||
          media.write(bytes) != bytes.size() || !media.flush()) {
        m_error = QStringLiteral("cannot copy complete synthetic image fixture");
        return false;
      }
    } else if (isSticker()) {
      const QString fixturePath = qEnvironmentVariable("WHATSFEATHER_MEDIA_TEST_FIXTURE");
      const QFileInfo sourceInfo(fixturePath);
      const QString runtimePath = qEnvironmentVariable("XDG_RUNTIME_DIR");
      const QFileInfo runtimeInfo(runtimePath);
      const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                             QFileDevice::ExeGroup | QFileDevice::ReadOther |
                             QFileDevice::WriteOther | QFileDevice::ExeOther;
      constexpr qint64 MaxFixtureBytes = 100000;
      if (fixturePath.isEmpty() || !sourceInfo.isFile() || sourceInfo.isSymLink() ||
          sourceInfo.suffix().compare(QStringLiteral("webp"), Qt::CaseInsensitive) != 0 ||
          runtimePath.isEmpty() || !runtimeInfo.isDir() || runtimeInfo.isSymLink() ||
          (runtimeInfo.permissions() & forbidden) ||
          (sourceInfo.permissions() & forbidden) ||
          !(sourceInfo.permissions() & QFileDevice::ReadOwner) ||
          sourceInfo.canonicalPath() != runtimeInfo.canonicalFilePath() ||
          sourceInfo.size() <= 0 || sourceInfo.size() > MaxFixtureBytes) {
        m_error = QStringLiteral("sticker fixture must be a bounded WebP inside a private XDG runtime directory");
        return false;
      }
      QFile source(fixturePath);
      if (!source.open(QIODevice::ReadOnly) ||
          !media.open(QIODevice::WriteOnly | QIODevice::NewOnly)) {
        m_error = QStringLiteral("cannot open synthetic sticker fixture");
        return false;
      }
      QImageReader reader(&source);
      reader.setDecideFormatFromContent(true);
      if (reader.format().toLower() != QByteArrayLiteral("webp") ||
          reader.size() != QSize(512, 512) || !source.seek(0)) {
        m_error = QStringLiteral("synthetic sticker fixture is not a static 512 by 512 WebP");
        return false;
      }
      const QByteArray bytes = source.readAll();
      if (source.error() != QFileDevice::NoError || bytes.size() != sourceInfo.size() ||
          media.write(bytes) != bytes.size() || !media.flush()) {
        m_error = QStringLiteral("cannot copy complete synthetic sticker fixture");
        return false;
      }
    } else if (!media.open(QIODevice::WriteOnly | QIODevice::NewOnly) ||
               media.write("%PDF-1.4 synthetic fixture only\n%%EOF\n") < 0) {
      m_error = QStringLiteral("cannot create synthetic fixture media");
      return false;
    }
    media.close();
    const auto filePermissions = QFileDevice::ReadOwner | QFileDevice::WriteOwner;
    if (!QFile::setPermissions(m_mediaPath, filePermissions)) {
      m_error = QStringLiteral("cannot restrict synthetic media permissions");
      return false;
    }
    const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                           QFileDevice::ExeGroup | QFileDevice::ReadOther |
                           QFileDevice::WriteOther | QFileDevice::ExeOther;
    if (QFileInfo(m_mediaPath).permissions() & forbidden) {
      m_error = QStringLiteral("synthetic media permissions are not private");
      return false;
    }
    m_server.setSocketOptions(QLocalServer::UserAccessOption);
    if (!m_server.listen(m_socketPath)) {
      m_error = m_server.errorString();
      return false;
    }
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
  QString profilePath() const { return m_profilePath; }
  QString mediaPath() const { return m_mediaPath; }
  QString error() const { return m_error; }
  bool isVideo() const { return m_kind == QStringLiteral("video"); }
  bool isSticker() const { return m_kind == QStringLiteral("sticker"); }
  bool isImage() const { return m_kind == QStringLiteral("image"); }
  QString kind() const { return m_kind; }
  QString messageId() const {
    return isVideo() ? QStringLiteral("fixture:incoming-video")
                     : isSticker() ? StickerID
                                   : isImage() ? ImageID : MessageID;
  }
  int downloadCount() const { return m_downloadParams.size(); }
  bool exactDownloads(int expected) const {
    if (m_downloadParams.size() != expected)
      return false;
    const QJsonObject params{{"chat_id", ChatID}, {"id", messageId()}};
    for (const auto &actual : m_downloadParams)
      if (actual != params)
        return false;
    return true;
  }

private:
  QJsonObject message(bool ready) const {
    const QString mediaName = isVideo() ? QStringLiteral("synthetic-video.mp4")
                              : isSticker() ? QStringLiteral("synthetic-sticker.webp")
                              : isImage() ? QStringLiteral("synthetic-image.png")
                                          : QStringLiteral("synthetic-document.pdf");
    const QString mediaMime = isVideo() ? QStringLiteral("video/mp4")
                              : isSticker() ? QStringLiteral("image/webp")
                              : isImage() ? QStringLiteral("image/png")
                                          : QStringLiteral("application/pdf");
    QJsonObject attachment{{"name", mediaName},
                           {"size", QFileInfo(m_mediaPath).size()},
                           {"mime", mediaMime},
                           {"kind", m_kind},
                           {"state", ready ? QStringLiteral("ready")
                                            : QStringLiteral("remote")}};
    if (isVideo()) {
      attachment.insert("duration", 2);
      attachment.insert("width", 96);
      attachment.insert("height", 64);
    } else if (isSticker()) {
      attachment.insert("width", 512);
      attachment.insert("height", 512);
    } else if (isImage()) {
      attachment.insert("width", 64);
      attachment.insert("height", 48);
    }
    if (ready)
      attachment.insert("url", QUrl::fromLocalFile(m_mediaPath).toString());
    return {{"id", messageId()},
            {"chat_id", ChatID},
            {"body", isVideo() ? QStringLiteral("Synthetic incoming video")
                     : isSticker() ? QStringLiteral("Synthetic incoming sticker")
                     : isImage() ? QStringLiteral("Synthetic incoming image")
                                 : QStringLiteral("Synthetic attachment")},
            {"outgoing", false},
            {"status", QStringLiteral("received")},
            {"timestamp", 1700000000000LL},
            {"kind", m_kind},
            {"attachment", attachment}};
  }

  void send(QLocalSocket *socket, const QJsonObject &response) {
    if (!socket || socket->state() != QLocalSocket::ConnectedState)
      return;
    socket->write(QJsonDocument(response).toJson(QJsonDocument::Compact) + '\n');
    socket->flush();
  }

  QJsonObject envelope(const QString &id, const QJsonObject &result) const {
    return {{"v", 1}, {"id", id}, {"result", result}};
  }

  QJsonObject failure(const QString &id, const QString &code,
                      const QString &text) const {
    return {{"v", 1}, {"id", id},
            {"error", QJsonObject{{"code", code}, {"message", text}}}};
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
                                 {"revision", 1}, {"message", "Synthetic fixture"}}));
    } else if (method == QStringLiteral("app.capabilities")) {
      send(socket, envelope(id, {{"version", 1},
                                 {"methods", QJsonArray{QStringLiteral("message.download")}},
                                 {"features", QJsonObject{}}}));
    } else if (method == QStringLiteral("account.get")) {
      send(socket, envelope(id, {{"available", false}, {"state", "demo"}}));
    } else if (method == QStringLiteral("chat.list")) {
      send(socket, envelope(id, {{"chats", QJsonArray{QJsonObject{
          {"id", ChatID}, {"title", "Synthetic incoming media"},
          {"initials", "S"}, {"timestamp", 1700000000000LL}}}}}));
    } else if (method == QStringLiteral("draft.get")) {
      send(socket, envelope(id, {{"text", ""}}));
    } else if (method == QStringLiteral("draft.set")) {
      send(socket, envelope(id, {{"saved", true}}));
    } else if (method == QStringLiteral("message.list")) {
      send(socket, envelope(id, {{"messages", QJsonArray{message(false)}},
                                 {"has_more", false}}));
    } else if (method == QStringLiteral("message.download")) {
      m_downloadParams.append(params);
      const QJsonObject expected{{"chat_id", ChatID}, {"id", messageId()}};
      if (params != expected) {
        send(socket, failure(id, QStringLiteral("wrong_scope"),
                             QStringLiteral("Synthetic fixture rejected wrong scope")));
      } else if (m_downloadParams.size() == 1) {
        const auto response = failure(id, QStringLiteral("synthetic_failure"),
                                      QStringLiteral("Synthetic first-attempt failure"));
        QPointer<QLocalSocket> guarded(socket);
        QTimer::singleShot(250, this, [this, guarded, response] {
          send(guarded, response);
        });
      } else if (m_downloadParams.size() == 2) {
        send(socket, envelope(id, {{"message", message(true)}}));
      } else {
        send(socket, failure(id, QStringLiteral("too_many"),
                             QStringLiteral("Unexpected repeated synthetic request")));
      }
    } else {
      send(socket, failure(id, QStringLiteral("unsupported"),
                           QStringLiteral("Synthetic fixture method unsupported")));
    }
  }

  QString m_kind;
  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_profilePath, m_mediaPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  QJsonArray m_downloadParams;
};
} // namespace

QObject *startMediaDownloadFixture(QGuiApplication &, Controller &controller) {
  auto *fixture = new SyntheticMediaRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical() << "Cannot start private media test fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), fixture->profilePath(), false);
  return fixture;
}

void runMediaDownloadTest(QGuiApplication &app, QQuickWindow *window,
                          Controller &controller, QObject *fixtureObject,
                          const QString &statePath,
                          const QString &commandPath) {
  auto *fixture = static_cast<SyntheticMediaRPC *>(fixtureObject);
  if (!fixture) {
    qCritical() << "Missing private media fixture";
    app.exit(1);
    return;
  }
  struct State { int stage = 0; int ticks = 0; };
  auto state = std::make_shared<State>();
  auto *fileOpener = fixture->kind() == QStringLiteral("document")
                         ? new SyntheticFileOpener(&app)
                         : nullptr;
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(timer, &QTimer::timeout, &app,
                   [&, state, timer, window, fixture, statePath, commandPath,
                    fileOpener] {
    auto fail = [&](const QString &message) {
      qCritical() << "Native incoming media UI FAIL at stage" << state->stage
                  << message;
      timer->stop();
      app.exit(1);
    };
    if (++state->ticks > 600) {
      fail("timed out");
      return;
    }
    if (!controller.demo()) {
      fail("refusing interactions outside demo mode");
      return;
    }
    if (state->stage == 0) {
      if (controller.selectedId().isEmpty() || controller.messageCount() == 0)
        return;
      if (controller.messageCount() != 1 || controller.selectedId() != ChatID) {
        fail(QStringLiteral("expected exactly one synthetic chat and remote %1")
                 .arg(fixture->kind()));
        return;
      }
      const auto record = controller.messages()->row(0);
      const auto attachment = record.value("attachment").toMap();
      if (record.value("id").toString() != fixture->messageId() ||
          record.value("chat_id").toString() != ChatID ||
          attachment.value("kind").toString() != fixture->kind() ||
          attachment.value("state").toString() != QStringLiteral("remote")) {
        fail("unexpected synthetic attachment metadata");
        return;
      }
      auto *download = named(window, QStringLiteral("downloadMediaButton"));
      if (!download || !download->property("visible").toBool() ||
          download->property("text").toString() != QStringLiteral("Download"))
        return;
      if (fixture->downloadCount() != 0 ||
          !writeText(statePath, QStringLiteral("await-download\n0"))) {
        fail("initial state must have no download request");
        return;
      }
      state->stage = 1;
      return;
    }
    if (state->stage == 1) {
      if (readText(commandPath) != QStringLiteral("activate-download"))
        return;
      if (fixture->downloadCount() != 0) {
        fail("download started before explicit keyboard activation");
        return;
      }
      auto *download = named(window, QStringLiteral("downloadMediaButton"));
      if (!download || download->property("text").toString() != QStringLiteral("Download") ||
          !keyboardActivate(download)) {
        fail("rendered Download control was not keyboard actionable");
        return;
      }
      qInfo() << "Activated rendered Download action with Space";
      state->stage = 2;
      return;
    }
    if (state->stage == 2) {
      const auto record = controller.messages()->row(0);
      const auto attachment = record.value("attachment").toMap();
      if (attachment.value("state").toString() == QStringLiteral("downloading")) {
        if (!controller.messageBusy(record.value("id").toString())) {
          fail("download state did not mark the request busy");
          return;
        }
        state->stage = 3;
      }
      return;
    }
    if (state->stage == 3) {
      const auto record = controller.messages()->row(0);
      const auto attachment = record.value("attachment").toMap();
      if (attachment.value("state").toString() != QStringLiteral("error"))
        return;
      auto *retry = named(window, QStringLiteral("downloadMediaButton"));
      if (!fixture->exactDownloads(1) || !retry ||
          !retry->property("visible").toBool() ||
          retry->property("text").toString() != QStringLiteral("Retry") ||
          controller.messageBusy(record.value("id").toString())) {
        fail("failed state must expose idle Retry after exactly one scoped request");
        return;
      }
      if (!writeText(statePath, QStringLiteral("await-retry\n1\n%1\n%2")
                                     .arg(ChatID, fixture->messageId()))) {
        fail("cannot publish failure state");
        return;
      }
      state->stage = 4;
      return;
    }
    if (state->stage == 4) {
      if (fixture->downloadCount() != 1 || !fixture->exactDownloads(1)) {
        fail("failed download was automatically retried");
        return;
      }
      if (readText(commandPath) != QStringLiteral("activate-retry"))
        return;
      auto *retry = named(window, QStringLiteral("downloadMediaButton"));
      if (!retry || retry->property("text").toString() != QStringLiteral("Retry") ||
          !keyboardActivate(retry)) {
        fail("rendered Retry control was not keyboard actionable");
        return;
      }
      qInfo() << "Activated rendered Retry action with Space";
      state->stage = 5;
      return;
    }
    if (state->stage == 5) {
      const auto record = controller.messages()->row(0);
      const auto attachment = record.value("attachment").toMap();
      if (attachment.value("state").toString() != QStringLiteral("ready"))
        return;
      if (fixture->downloadCount() != 2 || !fixture->exactDownloads(2) ||
          record.value("id").toString() != fixture->messageId() ||
          record.value("chat_id").toString() != controller.selectedId() ||
          !QUrl(attachment.value("url").toString()).isLocalFile() ||
          QUrl(attachment.value("url").toString()).toLocalFile() != fixture->mediaPath()) {
        fail("ready response is not correctly scoped to the private synthetic file");
        return;
      }
      auto *open = named(window, QStringLiteral("openMediaButton"), true);
      const QString expectedAction = fixture->isVideo() ? QStringLiteral("Play")
                                                        : QStringLiteral("Open");
      if (!open || !open->property("enabled").toBool() ||
          (!fixture->isSticker() && !fixture->isImage() &&
           open->property("text").toString() != expectedAction)) {
        fail(QStringLiteral("ready %1 did not expose its native %2 action")
                 .arg(fixture->kind(), expectedAction));
        return;
      }
      if (!writeText(statePath,
                     QStringLiteral("verify-media\n%1\n2\n%2\n%3")
                         .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
        fail("cannot publish ready state");
        return;
      }
      state->stage = 6;
      return;
    }
    if (state->stage == 6) {
      if (readText(commandPath) != QStringLiteral("permissions-verified"))
        return;
      if (fixture->isSticker() || fixture->isImage()) {
        auto *preview = named(window, QStringLiteral("messageMediaPreview"), true);
        if (!preview)
          return;
        const int status = preview->property("status").toInt();
        if (status == 3) {
          fail(QStringLiteral("Qt could not decode the synthetic incoming %1")
                   .arg(fixture->kind()));
          return;
        }
        if (status != 1)
          return;
        const QUrl previewSource(preview->property("source").toString());
        auto *open = named(window, QStringLiteral("openMediaButton"), true);
        if (!previewSource.isLocalFile() ||
            previewSource.toLocalFile() != fixture->mediaPath() || !open ||
            !open->property("enabled").toBool()) {
          fail(QStringLiteral("ready %1 preview or native open control is not bound to the downloaded file")
                   .arg(fixture->kind()));
          return;
        }
        const QString stateText = QStringLiteral("await-viewer") + QChar(10) +
                                  fixture->mediaPath() + QChar(10) +
                                  QStringLiteral("2") + QChar(10) + ChatID +
                                  QChar(10) + fixture->messageId();
        if (!writeText(statePath, stateText)) {
          fail(QStringLiteral("cannot publish incoming %1 viewer state")
                   .arg(fixture->kind()));
          return;
        }
        state->stage = 13;
        return;
      }
      if (fixture->kind() == QStringLiteral("document")) {
        const QString stateText = QStringLiteral("await-document-open") + QChar(10) +
                                  fixture->mediaPath() + QChar(10) +
                                  QStringLiteral("2") + QChar(10) + ChatID +
                                  QChar(10) + fixture->messageId();
        if (!writeText(statePath, stateText)) {
          fail("cannot publish incoming document open state");
          return;
        }
        state->stage = 16;
        return;
      }
      if (!fixture->isVideo()) {
        if (!writeText(statePath,
                       QStringLiteral("complete\n%1\n2\n%2\n%3")
                           .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
          fail("cannot publish completion state");
          return;
        }
        qInfo() << "Native incoming media UI PASS: private synthetic RPC, explicit keyboard Download/Retry, exact scope, ready model and Open action";
        timer->stop();
        app.quit();
        return;
      }
      auto *open = named(window, QStringLiteral("openMediaButton"), true);
      if (!open || open->property("text").toString() != QStringLiteral("Play") ||
          !keyboardActivate(open)) {
        fail("rendered incoming video Play action was not keyboard actionable");
        return;
      }
      if (!writeText(statePath,
                     QStringLiteral("await-viewer\n%1\n2\n%2\n%3")
                         .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
        fail("cannot publish incoming video viewer state");
        return;
      }
      state->stage = 7;
      return;
    }
    if (state->stage == 7) {
      auto *viewer = named(window, QStringLiteral("mediaViewer"));
      if (!viewer || !viewer->property("opened").toBool())
        return;
      auto *player = named(window, QStringLiteral("viewerPlayer"));
      auto *video = qobject_cast<QQuickItem *>(named(window, QStringLiteral("viewerVideoOutput")));
      auto *audioLoader = named(window, QStringLiteral("viewerAudioOutputLoader"));
      auto *progress = named(window, QStringLiteral("viewerProgress"));
      if (!player || !video || !audioLoader || !progress)
        return;
      const auto playerError = player->property("errorString").toString();
      if (!playerError.isEmpty()) {
        fail("Qt Multimedia could not load synthetic incoming MP4: " + playerError);
        return;
      }
      if (player->property("duration").toLongLong() <= 0)
        return;
      const QUrl source(viewer->property("localSource").toString());
      const qreal width = viewer->property("naturalWidth").toReal();
      const qreal height = viewer->property("naturalHeight").toReal();
      if (!viewer->property("isVideo").toBool() || !video->isVisible() ||
          !source.isLocalFile() || source.toLocalFile() != fixture->mediaPath() ||
          qAbs(width / qMax(qreal(1), height) - qreal(96) / 64) > 0.01 ||
          !progress->property("enabled").toBool() ||
          player->property("playbackState").toInt() != 0 ||
          !viewer->property("audioMuted").toBool() ||
          viewer->property("audioOutputArmed").toBool() ||
          audioLoader->property("active").toBool() ||
          named(window, QStringLiteral("viewerAudioOutput"))) {
        fail("incoming video viewer metadata, controls or muted-stopped boundary is incorrect");
        return;
      }
      qInfo() << "Loaded the private synthetic incoming video in the native viewer";
      if (readText(commandPath) != QStringLiteral("activate-play"))
        return;
      auto *play = named(window, QStringLiteral("viewerPlayPause"));
      if (!play || play->property("text").toString() != QStringLiteral("Play") ||
          !keyboardActivate(play)) {
        fail("native incoming video Play control was not keyboard actionable");
        return;
      }
      state->stage = 8;
      return;
    }
    if (state->stage == 8) {
      auto *viewer = named(window, QStringLiteral("mediaViewer"));
      auto *player = named(window, QStringLiteral("viewerPlayer"));
      auto *audioLoader = named(window, QStringLiteral("viewerAudioOutputLoader"));
      if (!viewer || !player || !audioLoader)
        return;
      if (player->property("playbackState").toInt() != 1) {
        if (!player->property("errorString").toString().isEmpty())
          fail("native incoming video playback failed: " + player->property("errorString").toString());
        return;
      }
      if (!viewer->property("audioMuted").toBool() ||
          viewer->property("audioOutputArmed").toBool() ||
          audioLoader->property("active").toBool() ||
          named(window, QStringLiteral("viewerAudioOutput"))) {
        fail("video Play must remain muted and must not create audio output");
        return;
      }
      if (!writeText(statePath,
                     QStringLiteral("await-pause\n%1\n2\n%2\n%3")
                         .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
        fail("cannot publish playing video state");
        return;
      }
      state->stage = 9;
      return;
    }
    if (state->stage == 9) {
      if (readText(commandPath) != QStringLiteral("activate-pause"))
        return;
      auto *pause = named(window, QStringLiteral("viewerPlayPause"));
      if (!pause || pause->property("text").toString() != QStringLiteral("Pause") ||
          !keyboardActivate(pause)) {
        fail("native incoming video Pause control was not keyboard actionable");
        return;
      }
      state->stage = 10;
      return;
    }
    if (state->stage == 10) {
      auto *player = named(window, QStringLiteral("viewerPlayer"));
      if (!player)
        return;
      if (player->property("playbackState").toInt() != 2)
        return;
      if (!writeText(statePath,
                     QStringLiteral("await-close\n%1\n2\n%2\n%3")
                         .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
        fail("cannot publish paused video state");
        return;
      }
      state->stage = 11;
      return;
    }
    if (state->stage == 11) {
      if (readText(commandPath) != QStringLiteral("close-viewer"))
        return;
      auto *close = named(window, QStringLiteral("mediaViewerClose"));
      if (!close || !keyboardActivate(close)) {
        fail("media viewer Close control was not keyboard actionable");
        return;
      }
      state->stage = 12;
      return;
    }
    if (state->stage == 12) {
      auto *viewer = named(window, QStringLiteral("mediaViewer"));
      auto *player = named(window, QStringLiteral("viewerPlayer"));
      if (!viewer || !player || viewer->property("opened").toBool())
        return;
      if (player->property("playbackState").toInt() != 0 ||
          !viewer->property("localSource").toString().isEmpty() ||
          viewer->property("audioOutputArmed").toBool() ||
          named(window, QStringLiteral("viewerAudioOutput"))) {
        fail("closing the viewer must stop playback, clear local source and leave audio disarmed");
        return;
      }
      if (!writeText(statePath,
                     QStringLiteral("complete\n%1\n2\n%2\n%3")
                         .arg(fixture->mediaPath(), ChatID, fixture->messageId()))) {
        fail("cannot publish incoming video completion");
        return;
      }
      qInfo() << "Native incoming video UI PASS: exact private download scope, explicit keyboard Retry/Play/Pause/Close, local decode and muted playback";
      timer->stop();
      app.quit();
    }
    if (state->stage == 13) {
      const QString openCommand = fixture->isImage() ? QStringLiteral("open-image")
                                                     : QStringLiteral("open-sticker");
      if (readText(commandPath) != openCommand)
        return;
      auto *open = named(window, QStringLiteral("openMediaButton"), true);
      if (!open || !keyboardActivate(open)) {
        fail(QStringLiteral("ready %1 preview was not keyboard actionable")
                 .arg(fixture->kind()));
        return;
      }
      state->stage = 14;
      return;
    }
    if (state->stage == 14) {
      auto *viewer = named(window, QStringLiteral("mediaViewer"));
      if (!viewer || !viewer->property("opened").toBool())
        return;
      auto *still = named(window, QStringLiteral("viewerStillImage"));
      const int status = viewer->property("loadStatus").toInt();
      if (status == 3) {
        fail(QStringLiteral("native media viewer could not decode the downloaded %1")
                 .arg(fixture->kind()));
        return;
      }
      if (status != 1 || !still)
        return;
      const QUrl source(viewer->property("localSource").toString());
      const qreal width = viewer->property("naturalWidth").toReal();
      const qreal height = viewer->property("naturalHeight").toReal();
      const auto attachment = viewer->property("attachment").toMap();
      const qreal expectedAspect = attachment.value("width").toReal() /
                                   qMax(qreal(1), attachment.value("height").toReal());
      if (viewer->property("isVideo").toBool() ||
          viewer->property("playable").toBool() ||
          attachment.value("kind").toString() != fixture->kind() ||
          !source.isLocalFile() || source.toLocalFile() != fixture->mediaPath() ||
          !still->property("visible").toBool() ||
          expectedAspect <= 0 ||
          qAbs(width / qMax(qreal(1), height) - expectedAspect) > 0.01) {
        fail(QStringLiteral("native %1 viewer lost its kind, local source or intrinsic aspect ratio")
                 .arg(fixture->kind()));
        return;
      }
      auto *close = named(window, QStringLiteral("mediaViewerClose"));
      if (!close || !keyboardActivate(close)) {
        fail(QStringLiteral("%1 viewer Close control was not keyboard actionable")
                 .arg(fixture->kind()));
        return;
      }
      state->stage = 15;
      return;
    }
    if (state->stage == 15) {
      auto *viewer = named(window, QStringLiteral("mediaViewer"));
      auto *still = named(window, QStringLiteral("viewerStillImage"));
      if (!viewer || !still || viewer->property("opened").toBool())
        return;
      if (!viewer->property("localSource").toString().isEmpty() ||
          !still->property("source").toString().isEmpty()) {
        fail(QStringLiteral("closing %1 viewer must clear its local media source")
                 .arg(fixture->kind()));
        return;
      }
      const QString stateText = QStringLiteral("complete") + QChar(10) +
                                fixture->mediaPath() + QChar(10) +
                                QStringLiteral("2") + QChar(10) + ChatID +
                                QChar(10) + fixture->messageId();
      if (!writeText(statePath, stateText)) {
        fail(QStringLiteral("cannot publish incoming %1 completion")
                 .arg(fixture->kind()));
        return;
      }
      qInfo() << QStringLiteral("Native incoming %1 UI PASS: private download, explicit keyboard Retry, decoded image preview, native Open/Close")
                     .arg(fixture->kind());
      timer->stop();
      app.quit();
    }
    if (state->stage == 16) {
      if (readText(commandPath) != QStringLiteral("open-document"))
        return;
      auto *open = named(window, QStringLiteral("openMediaButton"), true);
      if (!open || open->property("text").toString() != QStringLiteral("Open") ||
          !keyboardActivate(open)) {
        fail("ready document Open action was not keyboard actionable");
        return;
      }
      if (!fileOpener || fileOpener->openedCount() != 1 ||
          !fileOpener->lastUrl().isLocalFile() ||
          fileOpener->lastUrl().toLocalFile() != fixture->mediaPath()) {
        fail("document Open did not dispatch the exact private downloaded file");
        return;
      }
      const QString stateText = QStringLiteral("complete") + QChar(10) +
                                fixture->mediaPath() + QChar(10) +
                                QStringLiteral("2") + QChar(10) + ChatID +
                                QChar(10) + fixture->messageId();
      if (!writeText(statePath, stateText)) {
        fail("cannot publish incoming document completion");
        return;
      }
      qInfo() << "Native incoming document UI PASS: private download, explicit keyboard Retry/Open, exact local file URL";
      timer->stop();
      app.quit();
    }
  });
  timer->start();
}

#include "mediadownloadtest.moc"
