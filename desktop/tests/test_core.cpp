#include "controller.h"
#include "desktopnotifications.h"
#include "jsonmodel.h"
#include "localexpressionstore.h"
#include "voicenoterecorder.h"
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QImage>
#include <QLocalServer>
#include <QLocalSocket>
#include <QPointer>
#include <QtTest>
#include <functional>
#include <memory>

namespace {
constexpr int MaxFrame = 256 * 1024;

class FakeBackend final : public QObject {
public:
  explicit FakeBackend(const QString &path, QObject *parent = nullptr)
      : QObject(parent), m_path(path) {
    QLocalServer::removeServer(m_path);
    m_server.listen(m_path);
    connect(&m_server, &QLocalServer::newConnection, this, [this] {
      while (m_server.hasPendingConnections()) {
        auto *socket = m_server.nextPendingConnection();
        ++connectionCount;
        m_sockets.append(socket);
        connect(socket, &QLocalSocket::readyRead, this, [this, socket] {
          auto &buffer = m_input[socket];
          buffer += socket->readAll();
          while (true) {
            const auto newline = buffer.indexOf('\n');
            if (newline < 0)
              break;
            const auto line = buffer.left(newline);
            buffer.remove(0, newline + 1);
            QJsonParseError error;
            const auto document = QJsonDocument::fromJson(line, &error);
            if (error.error == QJsonParseError::NoError && document.isObject()) {
              const auto request = document.object();
              m_requestMethods.insert(request.value("id").toString(),
                                      request.value("method").toString());
              if (onRequest)
                onRequest(socket, request);
            }
          }
        });
        connect(socket, &QLocalSocket::disconnected, this, [this, socket] {
          m_input.remove(socket);
          m_sockets.removeOne(socket);
          socket->deleteLater();
        });
      }
    });
  }

  ~FakeBackend() override {
    const auto sockets = m_sockets;
    for (auto *socket : sockets)
      socket->close();
    m_server.close();
    QLocalServer::removeServer(m_path);
  }

  void stopListening() { m_server.close(); }
  void disconnectClients() {
    const auto sockets = m_sockets;
    for (auto *socket : sockets)
      if (socket)
        socket->abort();
  }

  void send(QLocalSocket *socket, const QByteArray &bytes) {
    if (!socket || socket->state() != QLocalSocket::ConnectedState)
      return;
    auto outgoing = bytes;
    if (!storageView.isEmpty()) {
      QJsonParseError parseError;
      const auto document = QJsonDocument::fromJson(outgoing, &parseError);
      if (parseError.error == QJsonParseError::NoError && document.isObject()) {
        auto frame = document.object();
        const auto id = frame.value("id").toString();
        if (m_requestMethods.value(id) == QStringLiteral("app.status")) {
          auto result = frame.value("result").toObject();
          result.insert(QStringLiteral("mode"), QStringLiteral("live"));
          result.insert(QStringLiteral("storage_view"), storageView);
          frame.insert(QStringLiteral("result"), result);
          outgoing = QJsonDocument(frame).toJson(QJsonDocument::Compact) + '\n';
        }
        m_requestMethods.remove(id);
      }
    }
    socket->write(outgoing);
    socket->flush();
  }

  void sendFragmented(QLocalSocket *socket, const QByteArray &bytes) {
    const int split = bytes.size() / 2;
    send(socket, bytes.left(split));
    QPointer<QLocalSocket> guarded(socket);
    const auto tail = bytes.mid(split);
    QTimer::singleShot(10, this, [this, guarded, tail] {
      if (guarded)
        send(guarded, tail);
    });
  }

  void sendCoalesced(QLocalSocket *socket, const QList<QByteArray> &frames) {
    QByteArray bytes;
    for (const auto &frame : frames)
      bytes += frame;
    send(socket, bytes);
  }

  std::function<void(QLocalSocket *, const QJsonObject &)> onRequest;
  int connectionCount = 0;
  QJsonObject storageView;

  void setEncryptedStorageView(const QJsonObject &view) { storageView = view; }

private:
  QString m_path;
  QLocalServer m_server;
  QList<QLocalSocket *> m_sockets;
  QHash<QLocalSocket *, QByteArray> m_input;
  QHash<QString, QString> m_requestMethods;
};

struct SyntheticEncryptedProfile {
  explicit SyntheticEncryptedProfile(const QString &basePath) {
    runtimeDir = QDir(basePath).filePath(QStringLiteral("runtime"));
    ciphertextRoot = QDir(basePath).filePath(QStringLiteral("ciphertext"));
    profileRoot = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic"));
    for (const auto &path : {runtimeDir, ciphertextRoot, profileRoot}) {
      if (!QDir().mkpath(path) ||
          !QFile::setPermissions(path, QFileDevice::ReadOwner |
                                           QFileDevice::WriteOwner |
                                           QFileDevice::ExeOwner))
        return;
    }
    valid = true;
  }

  QString socketPath(const QString &name) const {
    return QDir(runtimeDir).filePath(name);
  }

  QString createPreparedFile(const QString &name,
                             const QByteArray &contents = QByteArrayLiteral("synthetic prepared media")) const {
    const auto mediaDir = QDir(profileRoot).filePath(QStringLiteral("media"));
    const auto preparedDir = QDir(mediaDir).filePath(QStringLiteral("prepared"));
    for (const auto &directory : {mediaDir, preparedDir})
      if (!QDir().mkpath(directory) ||
          !QFile::setPermissions(directory, QFileDevice::ReadOwner |
                                               QFileDevice::WriteOwner |
                                               QFileDevice::ExeOwner))
        return {};
    const auto path = QDir(preparedDir).filePath(name);
    QFile file(path);
    if (!file.open(QIODevice::WriteOnly | QIODevice::Truncate) ||
        file.write(contents) != contents.size())
      return {};
    file.close();
    if (!QFile::setPermissions(path, QFileDevice::ReadOwner |
                                         QFileDevice::WriteOwner))
      return {};
    return path;
  }

  QJsonObject statusView() const {
    return {{QStringLiteral("version"), 1},
            {QStringLiteral("mode"), QStringLiteral("encrypted")},
            {QStringLiteral("ciphertext_root"), ciphertextRoot},
            {QStringLiteral("runtime_dir"), runtimeDir},
            {QStringLiteral("profile_root"), profileRoot},
            {QStringLiteral("view_id"), QStringLiteral("0123456789abcdef0123456789abcdef")},
            {QStringLiteral("mount_id"), QStringLiteral("42")}};
  }

  Controller::ProfileMountVerifier mountVerifier() const {
    return [](const QString &, const QString &runtime, const QString &profile,
              const QString &mountId, QString *error) {
      if (mountId == QStringLiteral("42") &&
          QFileInfo(profile).absolutePath() == runtime)
        return true;
      if (error)
        *error = QStringLiteral("Synthetic fixture mount identity mismatch.");
      return false;
    };
  }

  bool valid = false;
  QString runtimeDir;
  QString ciphertextRoot;
  QString profileRoot;
};

QByteArray responseFrame(const QString &id, QJsonObject result,
                         int targetSize = 0) {
  if (targetSize > 0) {
    result.insert("__padding", QString());
    const auto encode = [&result, &id] {
      return QJsonDocument(QJsonObject{{"id", id}, {"result", result}})
                 .toJson(QJsonDocument::Compact) +
             '\n';
    };
    const int padding = targetSize - encode().size();
    if (padding < 0)
      qFatal("response fixture cannot reach requested frame size");
    result.insert("__padding", QString(padding, QChar('x')));
  }
  const auto frame = QJsonDocument(QJsonObject{{"id", id}, {"result", result}})
                         .toJson(QJsonDocument::Compact) +
                     '\n';
  if (targetSize > 0 && frame.size() != targetSize)
    qFatal("response fixture produced the wrong frame size");
  return frame;
}

QByteArray statusFrame(const QString &id, const QString &message) {
  return responseFrame(id, {{"mode", "demo"},
                            {"message", message},
                            {"revision", 1.0},
                            {"qr_png", ""}});
}

struct SyntheticVoiceCaptureState {
  int factoryCalls = 0;
  bool started = false;
  bool stopped = false;
};

class SyntheticVoiceCaptureSource final : public VoiceCaptureSource {
public:
  explicit SyntheticVoiceCaptureSource(
      std::shared_ptr<SyntheticVoiceCaptureState> state)
      : m_state(std::move(state)) {}

  bool start(QIODevice *destination, PcmFormat *format,
             QString *error) override {
    m_state->started = true;
    if (!destination || !destination->isWritable() || !format) {
      if (error)
        *error = QStringLiteral("synthetic capture output unavailable");
      return false;
    }
    format->ffmpegInputFormat = QStringLiteral("s16le");
    format->sampleRate = 48000;
    format->channels = 1;
    format->bytesPerSample = 2;
    const QByteArray pcm(48000 * 2, '\0');
    if (destination->write(pcm) != pcm.size()) {
      if (error)
        *error = QStringLiteral("synthetic PCM write failed");
      return false;
    }
    return true;
  }

  void stop() override { m_state->stopped = true; }

private:
  std::shared_ptr<SyntheticVoiceCaptureState> m_state;
};
} // namespace

class CoreTest : public QObject {
  Q_OBJECT
private slots:
  void outgoingReceiptLabelsAreTruthful() {
    Controller controller;
    const QList<QPair<QString, QString>> cases{
        {QStringLiteral("demo_sent"), QStringLiteral("Saved locally")},
        {QStringLiteral("demo_received"), QStringLiteral("Received (demo)")},
        {QStringLiteral("queued"), QStringLiteral("Queued")},
        {QStringLiteral("pending"), QStringLiteral("Pending")},
        {QStringLiteral("sending"), QStringLiteral("Sending")},
        {QStringLiteral("accepted"), QStringLiteral("Sent")},
        {QStringLiteral("delivered"), QStringLiteral("Delivered")},
        {QStringLiteral("read"), QStringLiteral("Read")},
        {QStringLiteral("unknown"), QStringLiteral("Outcome unknown")},
        {QStringLiteral("failed"), QStringLiteral("Not sent")},
        {QStringLiteral("not_dispatched"), QStringLiteral("Not sent")},
        {QStringLiteral("received"), QStringLiteral("Received")},
        {QStringLiteral("vendor_future_status"), QStringLiteral("Status unavailable")},
        {QString(), QStringLiteral("Status unavailable")},
    };
    for (const auto &test : cases) {
      QString actual;
      QVERIFY2(QMetaObject::invokeMethod(
                   &controller, "messageStatusLabel", Qt::DirectConnection,
                   Q_RETURN_ARG(QString, actual),
                   Q_ARG(QString, test.first)),
               qPrintable(QStringLiteral("messageStatusLabel missing for '%1'").arg(test.first)));
      QCOMPARE(actual, test.second);
    }
  }

  void outgoingReceiptDescriptionsAreTruthful() {
    Controller controller;
    const QList<QPair<QString, QString>> cases{
        {QStringLiteral("demo_sent"), QStringLiteral("Saved locally · not sent to WhatsApp")},
        {QStringLiteral("demo_received"), QStringLiteral("Received in synthetic local demo")},
        {QStringLiteral("queued"), QStringLiteral("Queued locally · not sent to WhatsApp")},
        {QStringLiteral("pending"), QStringLiteral("Pending")},
        {QStringLiteral("sending"), QStringLiteral("Sending to WhatsApp")},
        {QStringLiteral("accepted"), QStringLiteral("Sent · awaiting delivery receipt")},
        {QStringLiteral("delivered"), QStringLiteral("Delivered · awaiting read receipt")},
        {QStringLiteral("read"), QStringLiteral("Read")},
        {QStringLiteral("unknown"), QStringLiteral("Outcome unknown · check conversation before retrying")},
        {QStringLiteral("failed"), QStringLiteral("Not sent")},
        {QStringLiteral("not_dispatched"), QStringLiteral("Not sent")},
        {QStringLiteral("received"), QStringLiteral("Received")},
        {QStringLiteral("vendor_future_status"), QStringLiteral("Status unavailable")},
        {QString(), QStringLiteral("Status unavailable")},
    };
    for (const auto &test : cases) {
      QString actual;
      QVERIFY2(QMetaObject::invokeMethod(
                   &controller, "messageStatusDescription", Qt::DirectConnection,
                   Q_RETURN_ARG(QString, actual),
                   Q_ARG(QString, test.first)),
               qPrintable(QStringLiteral("messageStatusDescription missing for '%1'").arg(test.first)));
      QCOMPARE(actual, test.second);
    }
  }

  void messageListReceiptStatusSurvivesControllerProjection() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto socket = root.path() + QStringLiteral("/receipts.sock");
    FakeBackend backend(socket);
    const QJsonObject readMessage{
        {"id", "receipt-read"},
        {"chat_id", "fixture:receipts"},
        {"body", "Synthetic read receipt"},
        {"timestamp", 1700000000000.0},
        {"outgoing", true},
        {"status", "read"},
    };
    backend.onRequest = [&backend, &readMessage](QLocalSocket *peer,
                                                const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      if (method == "app.status") {
        backend.send(peer, statusFrame(id, "receipt projection test"));
      } else if (method == "chat.list") {
        backend.send(peer, responseFrame(id, {{"chats", QJsonArray{}}}));
      } else if (method == "message.list") {
        backend.send(peer, responseFrame(
                               id, {{"messages", QJsonArray{readMessage}},
                                    {"has_more", false}}));
      } else if (method == "draft.get") {
        backend.send(peer, responseFrame(id, {{"text", ""}}));
      } else {
        backend.send(peer, responseFrame(id, {}));
      }
    };

    Controller controller;
    controller.start(socket, root.path() + QStringLiteral("/profile"), false);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.status(),
                              QStringLiteral("receipt projection test"), 3000);
    controller.selectChat(QStringLiteral("fixture:receipts"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.messageCount(), 1, 3000);
    const auto projected = controller.messages()->row(0);
    QCOMPARE(projected.value("id").toString(), QStringLiteral("receipt-read"));
    QCOMPARE(projected.value("status").toString(), QStringLiteral("read"));
    QCOMPARE(controller.messageStatusLabel(projected.value("status").toString()),
             QStringLiteral("Read"));
    QCOMPARE(controller.messageStatusDescription(projected.value("status").toString()),
             QStringLiteral("Read"));
  }

  void localEmojiPreferencesPersistPerProfileAndStayBounded() {
    QTemporaryDir profile;
    QVERIFY(profile.isValid());
    LocalExpressionStore store(profile.path());
    QString error;
    QVERIFY2(store.load(&error), qPrintable(error));
    const QStringList sample{
        QStringLiteral("😀"), QStringLiteral("😃"), QStringLiteral("😄"),
        QStringLiteral("😁"), QStringLiteral("😆"), QStringLiteral("😅"),
        QStringLiteral("😂"), QStringLiteral("🤣"), QStringLiteral("🙂"),
        QStringLiteral("🙃"), QStringLiteral("😉"), QStringLiteral("😊"),
        QStringLiteral("😇"), QStringLiteral("🥰"), QStringLiteral("😍"),
        QStringLiteral("🤩"), QStringLiteral("😘"), QStringLiteral("😗"),
        QStringLiteral("😚"), QStringLiteral("😙")};
    for (const auto &emoji : sample)
      QVERIFY2(store.recordRecentEmoji(emoji, &error), qPrintable(error));
    QVERIFY2(store.recordRecentEmoji(QStringLiteral("👎"), &error), qPrintable(error));
    QVERIFY2(store.toggleFavoriteEmoji(QStringLiteral("👎"), &error),
             qPrintable(error));
    QCOMPARE(store.recentEmojis().size(), 16);
    QCOMPARE(store.recentEmojis().first(), QStringLiteral("👎"));
    QCOMPARE(store.favoriteEmojis(), QStringList{QStringLiteral("👎")});
    QVERIFY(!store.recordRecentEmoji(QString(17, QChar('x')), &error));

    LocalExpressionStore reopened(profile.path());
    QVERIFY2(reopened.load(&error), qPrintable(error));
    QCOMPARE(reopened.recentEmojis(), store.recentEmojis());
    QCOMPARE(reopened.favoriteEmojis(), store.favoriteEmojis());
    QTemporaryDir otherProfile;
    QVERIFY(otherProfile.isValid());
    LocalExpressionStore isolated(otherProfile.path());
    QVERIFY(isolated.load(&error));
    QVERIFY(isolated.recentEmojis().isEmpty());
    QVERIFY(isolated.favoriteEmojis().isEmpty());
  }

  void localStickerPacksCopyValidatedFilesAndReopen() {
    QTemporaryDir profile;
    QVERIFY(profile.isValid());
    const auto fixture = QFINDTESTDATA(
        "backend/internal/app/testdata/sticker-static.webp");
    QVERIFY2(!fixture.isEmpty(), "synthetic static WebP fixture not found");
    LocalExpressionStore store(profile.path());
    QString error;
    QVERIFY2(store.load(&error), qPrintable(error));
    QVERIFY2(store.addStickerToPack(QStringLiteral("Synthetic set"), fixture,
                                   &error, QStringLiteral("Friendly sticker.webp")),
             qPrintable(error));
    QCOMPARE(store.stickerPacks().size(), 1);
    const auto pack = store.stickerPacks().first().toMap();
    QCOMPARE(pack.value("name").toString(), QStringLiteral("Synthetic set"));
    const auto stickers = pack.value("stickers").toList();
    QCOMPARE(stickers.size(), 1);
    const auto sticker = stickers.first().toMap();
    QCOMPARE(sticker.value("name").toString(),
             QStringLiteral("Friendly sticker.webp"));
    QString displayName;
    const auto copied = store.resolveStickerPath(
        pack.value("id").toString(), sticker.value("id").toString(),
        &displayName);
    QVERIFY(!copied.isEmpty());
    QCOMPARE(displayName, QStringLiteral("Friendly sticker.webp"));
    QVERIFY(copied.startsWith(profile.path() +
                              QStringLiteral("/local-expressions/sticker-packs/")));
    const QFileInfo copiedInfo(copied);
    QVERIFY(copiedInfo.isFile());
    QVERIFY(!copiedInfo.isSymLink());
    const auto permissions = copiedInfo.permissions();
    const auto publicBits = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                           QFileDevice::ExeGroup | QFileDevice::ReadOther |
                           QFileDevice::WriteOther | QFileDevice::ExeOther;
    QVERIFY(!(permissions & publicBits));

    LocalExpressionStore reopened(profile.path());
    QVERIFY2(reopened.load(&error), qPrintable(error));
    QCOMPARE(reopened.stickerPacks().size(), 1);
    QCOMPARE(reopened.stickerPacks().first().toMap().value("name").toString(),
             QStringLiteral("Synthetic set"));
    const auto reopenedStickers =
        reopened.stickerPacks().first().toMap().value("stickers").toList();
    QCOMPARE(reopenedStickers.size(), 1);
    QCOMPARE(reopenedStickers.first().toMap().value("name").toString(),
             QStringLiteral("Friendly sticker.webp"));
    QVERIFY(!reopened.resolveStickerPath(pack.value("id").toString(),
                                         QString(64, QChar('0'))).size());

    const auto invalid = profile.path() + QStringLiteral("/not-a-sticker.webp");
    QFile invalidFile(invalid);
    QVERIFY(invalidFile.open(QIODevice::WriteOnly));
    QCOMPARE(invalidFile.write("not a webp"), qint64(10));
    invalidFile.close();
    QVERIFY(!store.addStickerToPack(QStringLiteral("Bad"), invalid, &error));
    QCOMPARE(store.stickerPacks().size(), 1);
  }

  void localStickerPackLoadingRejectsSymlinkDirectory() {
    QTemporaryDir profile;
    QTemporaryDir outside;
    QVERIFY(profile.isValid());
    QVERIFY(outside.isValid());
    const auto localDir = QDir(profile.path()).filePath("local-expressions");
    const auto packsDir = QDir(localDir).filePath("sticker-packs");
    QVERIFY(QDir().mkpath(packsDir));
    const auto packId = QString(32, QChar('a'));
    const auto packDir = QDir(packsDir).filePath(packId);
    QVERIFY(QFile::link(outside.path(), packDir));

    const QJsonObject pack{{"id", packId},
                           {"name", QStringLiteral("Synthetic")},
                           {"stickers", QJsonArray{}}};
    const QJsonObject manifestObject{
        {"version", 1},
        {"recent_emojis", QJsonArray{}},
        {"favorite_emojis", QJsonArray{}},
        {"sticker_packs", QJsonArray{pack}}};
    QFile manifest(QDir(localDir).filePath("preferences.json"));
    QVERIFY(manifest.open(QIODevice::WriteOnly));
    const auto bytes = QJsonDocument(manifestObject).toJson(QJsonDocument::Compact);
    QCOMPARE(manifest.write(bytes), qint64(bytes.size()));
    manifest.close();

    LocalExpressionStore store(profile.path());
    QString error;
    QVERIFY2(!store.load(&error), "store accepted a symlinked sticker-pack directory");
    QVERIFY2(error.contains(QStringLiteral("directory"), Qt::CaseInsensitive),
             qPrintable(error));
  }

  void localStickerPackAddingRejectsSymlinkDirectory() {
    QTemporaryDir profile;
    QTemporaryDir outside;
    QVERIFY(profile.isValid());
    QVERIFY(outside.isValid());
    const auto fixture = QFINDTESTDATA(
        "backend/internal/app/testdata/sticker-static.webp");
    QVERIFY2(!fixture.isEmpty(), "synthetic static WebP fixture not found");
    const auto localDir = QDir(profile.path()).filePath("local-expressions");
    const auto packsDir = QDir(localDir).filePath("sticker-packs");
    const auto packId = QString(32, QChar('a'));
    const auto packDir = QDir(packsDir).filePath(packId);
    QVERIFY(QDir().mkpath(packDir));

    const QJsonObject pack{{"id", packId},
                           {"name", QStringLiteral("Synthetic")},
                           {"stickers", QJsonArray{}}};
    const QJsonObject manifestObject{
        {"version", 1},
        {"recent_emojis", QJsonArray{}},
        {"favorite_emojis", QJsonArray{}},
        {"sticker_packs", QJsonArray{pack}}};
    QFile manifest(QDir(localDir).filePath("preferences.json"));
    QVERIFY(manifest.open(QIODevice::WriteOnly));
    const auto bytes = QJsonDocument(manifestObject).toJson(QJsonDocument::Compact);
    QCOMPARE(manifest.write(bytes), qint64(bytes.size()));
    manifest.close();

    LocalExpressionStore store(profile.path());
    QString error;
    QVERIFY2(store.load(&error), qPrintable(error));
    QVERIFY(QDir(packDir).removeRecursively());
    QVERIFY(QFile::link(outside.path(), packDir));
    QVERIFY2(!store.addStickerToPack(QStringLiteral("Synthetic"), fixture,
                                    &error),
             "sticker copy followed a symlink outside the profile");
    QVERIFY2(QDir(outside.path()).entryList(QDir::Files | QDir::NoDotAndDotDot).isEmpty(),
             "sticker copy created a file outside the profile");
  }

  void localStickerPackResolutionRejectsSymlinkDirectory() {
    QTemporaryDir profile;
    QTemporaryDir outside;
    QVERIFY(profile.isValid());
    QVERIFY(outside.isValid());
    const auto fixture = QFINDTESTDATA(
        "backend/internal/app/testdata/sticker-static.webp");
    QVERIFY2(!fixture.isEmpty(), "synthetic static WebP fixture not found");
    LocalExpressionStore store(profile.path());
    QString error;
    QVERIFY2(store.load(&error), qPrintable(error));
    QVERIFY2(store.addStickerToPack(QStringLiteral("Synthetic"), fixture,
                                   &error),
             qPrintable(error));
    const auto pack = store.stickerPacks().first().toMap();
    const auto sticker = pack.value("stickers").toList().first().toMap();
    const auto packId = pack.value("id").toString();
    const auto stickerId = sticker.value("id").toString();
    const auto originalPath = store.resolveStickerPath(packId, stickerId);
    QVERIFY(!originalPath.isEmpty());
    const auto packDir = QDir(profile.path()).filePath(
        QStringLiteral("local-expressions/sticker-packs/%1").arg(packId));
    QVERIFY(QFile::copy(originalPath,
                        QDir(outside.path()).filePath(QFileInfo(originalPath).fileName())));
    QVERIFY(QDir(packDir).removeRecursively());
    QVERIFY(QFile::link(outside.path(), packDir));
    QVERIFY2(store.resolveStickerPath(packId, stickerId).isEmpty(),
             "sticker resolution exposed a file outside the profile");
  }

  void modelBoundAndDedup() {
    JsonModel m;
    QVariantList rows;
    for (int i = 0; i < 700; ++i)
      rows.append(QVariantMap{{"id", QString::number(i)}, {"body", "text"}});
    m.replace(rows);
    QCOMPARE(m.rowCount(), 500);
    QCOMPARE(m.row(0).value("id").toString(), QStringLiteral("200"));
    m.mergeTail({QVariantMap{{"id", "699"}, {"body", "edited"}}});
    QCOMPARE(m.rowCount(), 500);
    QCOMPARE(m.row(499).value("body").toString(), QStringLiteral("edited"));
  }
  void olderWindowRemainsBounded() {
    JsonModel m;
    QVariantList recent, older;
    for (int i = 100; i < 600; ++i)
      recent.append(QVariantMap{{"id", QString::number(i)}});
    for (int i = 50; i < 100; ++i)
      older.append(QVariantMap{{"id", QString::number(i)}});
    m.replace(recent);
    m.prepend(older);
    QCOMPARE(m.rowCount(), 500);
    QCOMPARE(m.row(0).value("id").toString(), QStringLiteral("50"));
    QCOMPARE(m.row(499).value("id").toString(), QStringLiteral("549"));
  }
  void liveAttachmentStagingAndUncertainSendPreserveDraft() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("live-media.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    QString sentState = "unknown";
    QStringList clientIds;
    QJsonObject historyRequestParams;
    int historyRequests = 0;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status") result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}, {"message", "Test connection"}};
      else if (method == "chat.list") result = {{"chats", QJsonArray{}}};
      else if (method == "message.list") result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "draft.get") result = {{"text", ""}};
      else if (method == "chat.self") result = {{"chat", QJsonObject{{"id", "fixture:self"}, {"title", "Tester (You)"}, {"initials", "T"}}}};
      else if (method == "history.sync") {
        ++historyRequests;
        historyRequestParams = request.value("params").toObject();
        result = {{"chat_id", "fixture:self"}, {"requested", true}, {"completed", false}, {"count", 50}, {"anchor_id", "anchor"}, {"anchor_timestamp", 10}};
      }
      else if (method == "attachment.inspect") result = {{"name", "fixture.png"}, {"kind", "image"}, {"mime", "image/png"}, {"size", 10}};
      else if (method == "attachment.prepare") {
        const auto prepared = profile.createPreparedFile(QStringLiteral("live-media.webp"));
        result = {{"name", "fixture.png"}, {"kind", "image"},
                  {"mode", "photo"}, {"state", "ready"},
                  {"prepared_path", prepared}};
      }
      else if (method == "message.attach") {
        const auto params = request.value("params").toObject();
        clientIds.append(params.value("client_id").toString());
        result = {{"message", QJsonObject{{"id", "media"}, {"chat_id", params.value("chat_id")}, {"body", params.value("text")}, {"outgoing", true}, {"status", sentState}}}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QVERIFY(QMetaObject::invokeMethod(&c, "openSelfChat"));
    QTRY_COMPARE(c.selectedId(), QStringLiteral("fixture:self"));
    QCOMPARE(c.selectedTitle(), QStringLiteral("Tester (You)"));
    QTRY_VERIFY(!c.loading());
    QCOMPARE(historyRequests, 0);
    QVERIFY(QMetaObject::invokeMethod(&c, "refreshHistory"));
    QTRY_COMPARE(historyRequests, 1);
    QTRY_VERIFY(c.property("notice").toString().contains("up to 50"));
    QTRY_VERIFY(c.property("notice").toString().contains("1970-01-01"));
    QCOMPARE(historyRequestParams.value("chat_id").toString(), QStringLiteral("fixture:self"));
    c.setDraft("Keep this caption until accepted");
    c.stageAttachment(QUrl::fromLocalFile(root.path() + "/fixture.png"));
    QTRY_VERIFY_WITH_TIMEOUT(!c.attachment().isEmpty(), 1500);
    QTRY_COMPARE_WITH_TIMEOUT(c.attachment().value("state").toString(),
                              QStringLiteral("ready"), 1500);
    c.send();
    QTRY_VERIFY(!c.sending());
    QCOMPARE(clientIds.size(), 1);
    QVERIFY(!c.attachment().isEmpty());
    QCOMPARE(c.draft(), QStringLiteral("Keep this caption until accepted"));
    QVERIFY(!c.error().isEmpty());
    sentState = "accepted";
    c.clearError();
    c.send();
    QTRY_VERIFY(c.attachment().isEmpty());
    QCOMPARE(clientIds.size(), 2);
    QCOMPARE(clientIds[0], clientIds[1]);
    QVERIFY(c.draft().isEmpty());
  }
  void contactFileReviewRequiresExplicitSend() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto socket = root.path() + QStringLiteral("/contact-review.sock");
    FakeBackend backend(socket);
    int prepareCount = 0;
    int sendCount = 0;
    QJsonObject sendParams;
    backend.onRequest = [&](QLocalSocket *peer, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      const auto params = request.value("params").toObject();
      QJsonObject result;
      if (method == "app.status") result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}, {"message", "Synthetic fixture"}};
      else if (method == "app.capabilities") result = {{"version", 1}, {"methods", QJsonArray{"contact.prepare", "message.contact.send"}}, {"features", QJsonObject{{"contact-cards", QJsonObject{{"available", true}}}}}};
      else if (method == "chat.list") result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:contact-chat"}, {"title", "Synthetic recipient"}, {"initials", "S"}}}}};
      else if (method == "message.list") result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "draft.get") result = {{"text", ""}};
      else if (method == "contact.prepare") {
        ++prepareCount;
        QCOMPARE(params.value("path").toString(), root.path() + QStringLiteral("/chosen.vcf"));
        result = {{"contacts", QJsonArray{QJsonObject{{"name", "Ada Lovelace"}, {"phones", QJsonArray{"+1 555 0100"}}, {"emails", QJsonArray{"ada@example.test"}}}}}};
      } else if (method == "message.contact.send") {
        ++sendCount;
        sendParams = params;
        result = {{"message", QJsonObject{{"id", params.value("client_id")}, {"chat_id", params.value("chat_id")}, {"kind", "contact"}, {"contacts", params.value("contacts")}, {"outgoing", true}, {"status", "demo_sent"}, {"timestamp", 1234}}}};
      }
      backend.send(peer, responseFrame(id, result));
    };
    const auto vcardPath = root.path() + QStringLiteral("/chosen.vcf");
    QFile file(vcardPath);
    QVERIFY(file.open(QIODevice::WriteOnly));
    QCOMPARE(file.write("synthetic selected vCard"), qint64(24));
    file.close();

    Controller controller;
    controller.start(socket, root.path() + QStringLiteral("/profile"), false);
    QTRY_VERIFY(controller.connected());
    QTRY_VERIFY(controller.capabilitiesReady());
    controller.selectChat(QStringLiteral("fixture:contact-chat"));
    QTRY_COMPARE(controller.selectedId(), QStringLiteral("fixture:contact-chat"));
    QTRY_VERIFY(!controller.loading());

    controller.stageContactFile(QUrl::fromLocalFile(vcardPath));
    QTRY_COMPARE(prepareCount, 1);
    QTRY_COMPARE(controller.contactCards().size(), 1);
    QCOMPARE(controller.contactCards().first().toMap().value("name").toString(), QStringLiteral("Ada Lovelace"));
    QCOMPARE(sendCount, 0);
    QVERIFY(controller.sendContactCards());
    QTRY_COMPARE(sendCount, 1);
    QTRY_VERIFY(!controller.contactSending());
    QCOMPARE(sendParams.value("chat_id").toString(), QStringLiteral("fixture:contact-chat"));
    QCOMPARE(sendParams.value("contacts").toArray().size(), 1);
    QVERIFY(!sendParams.value("client_id").toString().isEmpty());
    QVERIFY(!sendParams.contains("vcard"));
    QTRY_VERIFY(controller.contactCards().isEmpty());
    QCOMPARE(controller.messageCount(), 1);
    QCOMPARE(controller.messages()->row(0).value("kind").toString(), QStringLiteral("contact"));
  }
  void demoAlbumRequiresExplicitReviewAndSendsOneBoundedRequest() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto socket = root.path() + QStringLiteral("/album.sock");
    const auto profile = root.path() + QStringLiteral("/profile");
    FakeBackend backend(socket);
    int prepareCount = 0;
    int sendCount = 0;
    int releaseCount = 0;
    int singleAttachmentSendCount = 0;
    QJsonObject sendParams;
    QJsonArray stagePaths;
    QJsonArray preparedItems;
    QJsonArray sentItems;
    const auto preparedOne = profile + QStringLiteral("/media/prepared/album-one.png");
    const auto preparedTwo = profile + QStringLiteral("/media/prepared/album-two.png");
    const auto sentOne = profile + QStringLiteral("/media/album-send-one.png");
    const auto sentTwo = profile + QStringLiteral("/media/album-send-two.png");
    backend.onRequest = [&](QLocalSocket *peer, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      const auto params = request.value("params").toObject();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"attachment.album.prepare", "attachment.album.release", "message.album.send"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:album-chat"}, {"title", "Synthetic album chat"}}}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "attachment.album.prepare") {
        ++prepareCount;
        stagePaths = params.value("paths").toArray();
        const auto makeItem = [](const QString &name, const QString &path,
                                 const QString &hash) {
          const auto size = QFileInfo(path).size();
          return QJsonObject{{"name", name}, {"kind", "image"},
                             {"mime", "image/png"}, {"mode", "photo"},
                             {"state", "ready"}, {"size", size},
                             {"original_size", size}, {"prepared_size", size},
                             {"sha256", hash},
                             {"url", QUrl::fromLocalFile(path).toString()},
                             {"prepared_path", path}};
        };
        preparedItems = QJsonArray{
            makeItem(QStringLiteral("first.png"), preparedOne, QString(64, QLatin1Char('a'))),
            makeItem(QStringLiteral("second.png"), preparedTwo, QString(64, QLatin1Char('b')))};
        sentItems = QJsonArray{};
        int index = 0;
        for (const auto &value : preparedItems) {
          auto item = value.toObject();
          item.remove("prepared_path");
          const auto finalPath = index++ == 0 ? sentOne : sentTwo;
          item.insert("url", QUrl::fromLocalFile(finalPath).toString());
          sentItems.append(item);
        }
        result = {{"items", preparedItems},
                  {"total_prepared_size", preparedItems.at(0).toObject().value("prepared_size").toDouble() + preparedItems.at(1).toObject().value("prepared_size").toDouble()},
                  {"provenance", "local_demo"}};
      } else if (method == "message.album.send") {
        ++sendCount;
        sendParams = params;
        result = {{"message", QJsonObject{{"id", params.value("client_id")},
                                          {"chat_id", params.value("chat_id")},
                                          {"body", params.value("text")},
                                          {"outgoing", true}, {"status", "demo_sent"},
                                          {"kind", "album"}, {"album", sentItems},
                                          {"timestamp", 1234}}}};
      } else if (method == "attachment.album.release") {
        ++releaseCount;
        result = {{"released", params.value("paths").toArray().size()}};
      } else if (method == "message.attach") {
        ++singleAttachmentSendCount;
      }
      backend.send(peer, responseFrame(id, result));
    };

    const auto makeSource = [&](const QString &name, const QColor &color) -> QString {
      const auto path = root.path() + QLatin1Char('/') + name;
      QImage image(4, 4, QImage::Format_RGB32);
      image.fill(color);
      return image.save(path) ? path : QString{};
    };
    const auto first = makeSource(QStringLiteral("first.png"), Qt::red);
    const auto second = makeSource(QStringLiteral("second.png"), Qt::blue);
    QVERIFY(!first.isEmpty());
    QVERIFY(!second.isEmpty());
    QVERIFY(QDir().mkpath(profile + QStringLiteral("/media/prepared")));
    QImage preparedImageOne(4, 4, QImage::Format_RGB32);
    preparedImageOne.fill(Qt::red);
    QVERIFY(preparedImageOne.save(preparedOne, "PNG"));
    QImage preparedImageTwo(4, 4, QImage::Format_RGB32);
    preparedImageTwo.fill(Qt::blue);
    QVERIFY(preparedImageTwo.save(preparedTwo, "PNG"));
    QVERIFY(QDir().mkpath(profile + QStringLiteral("/media")));
    QVERIFY(preparedImageOne.save(sentOne, "PNG"));
    QVERIFY(preparedImageTwo.save(sentTwo, "PNG"));

    Controller controller;
    controller.start(socket, profile, false);
    QTRY_VERIFY(controller.connected());
    QTRY_VERIFY(controller.capabilitiesReady());
    controller.selectChat(QStringLiteral("fixture:album-chat"));
    QTRY_COMPARE(controller.selectedId(), QStringLiteral("fixture:album-chat"));
    QTRY_VERIFY(!controller.loading());

    QVariantList selection{QUrl::fromLocalFile(first), QUrl::fromLocalFile(second)};
    QVERIFY2(QMetaObject::invokeMethod(&controller, "stageAttachmentAlbum",
                                       Q_ARG(QVariantList, selection)),
             "controller must expose demo album selection/review");
    QTRY_COMPARE(prepareCount, 1);
    QTRY_COMPARE(controller.attachment().value("state").toString(), QStringLiteral("ready"));
    QCOMPARE(controller.attachment().value("kind").toString(), QStringLiteral("album"));
    QCOMPARE(controller.attachment().value("items").toList().size(), 2);
    QCOMPARE(sendCount, 0);
    controller.setDraft(QStringLiteral("Synthetic caption"));
    controller.send();
    QTRY_COMPARE(sendCount, 1);
    QTRY_VERIFY(!controller.sending());
    QCOMPARE(singleAttachmentSendCount, 0);
    QCOMPARE(stagePaths.size(), 2);
    QCOMPARE(stagePaths.at(0).toString(), first);
    QCOMPARE(stagePaths.at(1).toString(), second);
    QCOMPARE(sendParams.value("chat_id").toString(), QStringLiteral("fixture:album-chat"));
    QCOMPARE(sendParams.value("text").toString(), QStringLiteral("Synthetic caption"));
    QCOMPARE(sendParams.value("prepared_paths").toArray().size(), 2);
    QVERIFY(!sendParams.value("client_id").toString().isEmpty());
    QCOMPARE(controller.messages()->row(0).value("kind").toString(), QStringLiteral("album"));
    QCOMPARE(controller.messages()->row(0).value("album").toList().size(), 2);
    QTRY_VERIFY(controller.attachment().isEmpty());
    QTRY_VERIFY(controller.draft().isEmpty());
    QTRY_COMPARE(releaseCount, 1);
  }
  void unknownContactSendReconcilesHistoryWithoutReplay() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    FakeBackend backend(root.path() + QStringLiteral("/contact-unknown.sock"));
    int sendCount = 0;
    int messageListCount = 0;
    bool unknown = false;
    QJsonObject sendParams;
    backend.onRequest = [&](QLocalSocket *peer, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      const auto params = request.value("params").toObject();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1},
                  {"methods", QJsonArray{"contact.prepare", "message.contact.send"}},
                  {"features", QJsonObject{{"contact-cards", QJsonObject{{"available", true}}}}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:contact-chat"}, {"title", "Synthetic recipient"}}}}};
      else if (method == "message.list") {
        ++messageListCount;
        QJsonArray messages;
        if (unknown)
          messages.append(QJsonObject{{"id", sendParams.value("client_id")},
                                      {"chat_id", "fixture:contact-chat"}, {"kind", "contact"},
                                      {"contacts", sendParams.value("contacts")}, {"timestamp", 1234},
                                      {"outgoing", true}, {"status", "unknown"}});
        result = {{"messages", messages}, {"has_more", false}};
      } else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "contact.prepare")
        result = {{"contacts", QJsonArray{QJsonObject{{"name", "Ada Lovelace"}}}}};
      else if (method == "message.contact.send") {
        ++sendCount;
        sendParams = params;
        unknown = true;
        const auto frame = QJsonDocument(QJsonObject{
            {"id", id},
            {"error", QJsonObject{{"code", "unknown_outcome"},
                                  {"message", "Check history before retrying"}}}})
                            .toJson(QJsonDocument::Compact) + '\n';
        backend.send(peer, frame);
        return;
      }
      backend.send(peer, responseFrame(id, result));
    };
    const auto vcardPath = root.path() + QStringLiteral("/chosen.vcf");
    QFile vcard(vcardPath);
    QVERIFY(vcard.open(QIODevice::WriteOnly));
    QCOMPARE(vcard.write("synthetic selected vCard"), qint64(24));
    vcard.close();

    Controller controller;
    controller.start(root.path() + QStringLiteral("/contact-unknown.sock"),
                     root.path() + QStringLiteral("/profile"), false);
    QTRY_VERIFY(controller.connected());
    QTRY_VERIFY(controller.capabilitiesReady());
    controller.selectChat(QStringLiteral("fixture:contact-chat"));
    QTRY_VERIFY(!controller.loading());
    controller.stageContactFile(QUrl::fromLocalFile(vcardPath));
    QTRY_COMPARE(controller.contactCards().size(), 1);
    QCOMPARE(controller.sendContactCards(), true);
    QTRY_VERIFY(controller.contactOutcomeUnknown());
    QTRY_COMPARE(messageListCount, 2);
    QTRY_COMPARE(controller.messageCount(), 1);
    QCOMPARE(controller.messages()->row(0).value("status").toString(), QStringLiteral("unknown"));
    QVERIFY(!controller.sendContactCards());
    QCOMPARE(sendCount, 1);
    QCOMPARE(sendParams.value("client_id").toString(), controller.messages()->row(0).value("id").toString());
  }

  void preparationNeverSendsOldFileAndModesKeepOriginal() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("prepare.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    const auto preparedDocument = profile.createPreparedFile(QStringLiteral("prepared-document.png"));
    const auto preparedPhoto = profile.createPreparedFile(QStringLiteral("prepared-photo.jpg"));
    QVERIFY(!preparedDocument.isEmpty());
    QVERIFY(!preparedPhoto.isEmpty());
    QPointer<QLocalSocket> pending;
    QString pendingId;
    QJsonObject lastPrepare, lastSend;
    bool hold = true;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      const auto p = request.value("params").toObject();
      QJsonObject result;
      if (method == "app.status") result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "chat.list") result = {{"chats", QJsonArray{}}};
      else if (method == "message.list") result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "attachment.inspect") result = {{"name", "old.png"}, {"kind", "image"}};
      else if (method == "attachment.prepare") {
        lastPrepare = p;
        if (hold) { pending = socket; pendingId = id; return; }
        result = {{"name", "new.png"}, {"kind", "document"}, {"mode", "document"}, {"prepared_path", preparedDocument}, {"state", "ready"}};
      } else if (method == "message.attach") {
        lastSend = p;
        result = {{"message", QJsonObject{{"id", p.value("client_id")}, {"chat_id", p.value("chat_id")}, {"status", "accepted"}}}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    c.selectChat("fixture:self"); QTRY_VERIFY(!c.loading());
    c.stageAttachment(QUrl::fromLocalFile(root.path()+"/old.png"));
    QTRY_VERIFY(!c.attachment().isEmpty());
    const auto original = root.path()+"/new.png";
    c.stageAttachmentAs(QUrl::fromLocalFile(original), "photo");
    QTRY_VERIFY(pending);
    c.send(); QTest::qWait(50);
    QVERIFY2(lastSend.isEmpty(), "preparation sent the previously staged file");
    backend.send(pending, responseFrame(pendingId, {{"name", "new.jpg"}, {"kind", "image"}, {"state", "ready"}, {"mode", "photo"}, {"prepared_path", preparedPhoto}}));
    QTRY_COMPARE(c.attachment().value("state").toString(), QStringLiteral("ready"));
    hold = false;
    QVERIFY(QMetaObject::invokeMethod(&c, "changeAttachmentMode", Q_ARG(QString, QStringLiteral("document"))));
    QTRY_COMPARE(lastPrepare.value("mode").toString(), QStringLiteral("document"));
    QCOMPARE(lastPrepare.value("path").toString(), original);
    QTRY_COMPARE(c.attachment().value("mode").toString(), QStringLiteral("document"));
    c.send(); QTRY_VERIFY(!lastSend.isEmpty());
    QCOMPARE(lastSend.value("path").toString(), preparedDocument);
    QCOMPARE(lastSend.value("mode").toString(), QStringLiteral("document"));
  }
  void explicitSyntheticRecordingStagesAndSendsTypedVoiceNote() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("voice-note.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    auto captureState = std::make_shared<SyntheticVoiceCaptureState>();
    int prepareCount = 0;
    int sendCount = 0;
    QJsonObject prepareParams, sendParams;
    bool privateInputAtPrepare = false;
    const auto preparedPath = profile.createPreparedFile(
        QStringLiteral("backend-prepared.ogg"));
    QVERIFY(!preparedPath.isEmpty());
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      const auto params = request.value("params").toObject();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "attachment.prepare") {
        ++prepareCount;
        prepareParams = params;
        const QFileInfo input(params.value("path").toString());
        const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                               QFileDevice::ExeGroup | QFileDevice::ReadOther |
                               QFileDevice::WriteOther | QFileDevice::ExeOther;
        privateInputAtPrepare = input.isFile() &&
            (input.permissions() & forbidden) == 0 &&
            (QFileInfo(input.absolutePath()).permissions() & forbidden) == 0;
        result = {{"name", "voice-note.ogg"},
                  {"kind", "audio"},
                  {"mime", "audio/ogg; codecs=opus"},
                  {"audio_codec", "opus"},
                  {"mode", "voice-note"},
                  {"duration", 1},
                  {"is_voice_note", true},
                  {"prepared_size", 256},
                  {"prepared_path", preparedPath},
                  {"state", "ready"}};
      } else if (method == "message.attach") {
        ++sendCount;
        sendParams = params;
        result = {{"message", QJsonObject{{"id", params.value("client_id")},
                                           {"chat_id", params.value("chat_id")},
                                           {"body", params.value("text")},
                                           {"kind", "audio"},
                                           {"outgoing", true},
                                           {"status", "accepted"}}}};
      }
      backend.send(socket, responseFrame(id, result));
    };

    Controller controller(nullptr, [captureState] {
      ++captureState->factoryCalls;
      return std::make_unique<SyntheticVoiceCaptureSource>(captureState);
    }, profile.mountVerifier());
    controller.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QVERIFY(!controller.startVoiceNoteRecording());
    QCOMPARE(captureState->factoryCalls, 0);
    controller.selectChat(QStringLiteral("fixture:self"));
    QTRY_VERIFY_WITH_TIMEOUT(!controller.loading(), 3000);
    QCOMPARE(captureState->factoryCalls, 0);

    QVERIFY(controller.startVoiceNoteRecording());
    QVERIFY(controller.voiceRecording());
    QCOMPARE(captureState->factoryCalls, 1);
    QVERIFY(captureState->started);
    controller.stopVoiceNoteRecording();
    QTRY_COMPARE_WITH_TIMEOUT(prepareCount, 1, 20000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.attachment().value("state").toString(),
                               QStringLiteral("ready"), 3000);
    QVERIFY(privateInputAtPrepare);
    QCOMPARE(prepareParams.value("mode").toString(), QStringLiteral("voice-note"));
    const auto capturedPath = prepareParams.value("path").toString();
    QVERIFY(QFileInfo(capturedPath).isFile());
    QCOMPARE(controller.attachment().value("kind").toString(), QStringLiteral("audio"));
    QCOMPARE(controller.attachment().value("mode").toString(), QStringLiteral("voice-note"));
    QVERIFY(controller.attachment().value("is_voice_note").toBool());
    QCOMPARE(controller.attachment().value("audio_codec").toString(), QStringLiteral("opus"));

    controller.send();
    QTRY_COMPARE_WITH_TIMEOUT(sendCount, 1, 3000);
    QCOMPARE(sendParams.value("mode").toString(), QStringLiteral("voice-note"));
    QCOMPARE(sendParams.value("prepared_path").toString(), preparedPath);
    QVERIFY(!sendParams.value("as_document").toBool());
    QCOMPARE(sendParams.value("text").toString(), QString{});
    QTRY_VERIFY_WITH_TIMEOUT(controller.attachment().isEmpty(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(!QFileInfo::exists(capturedPath), 3000);
    QVERIFY(captureState->stopped);
  }
  void audioPreparationFailsClosedWhenValidatedStageIsUnavailable() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("audio-prepare.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    QFile source(root.path() + "/voice-note.ogg");
    QVERIFY(source.open(QIODevice::WriteOnly));
    QCOMPARE(source.write(QByteArrayLiteral("synthetic audio fixture")),
             qint64(23));
    source.close();
    int inspectCount = 0;
    int sendCount = 0;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "attachment.prepare") {
        const QJsonObject failure{{"id", id},
                                  {"error", QJsonObject{
                                      {"code", "method_not_found"},
                                      {"message", "Preparation unavailable"}}}};
        backend.send(socket, QJsonDocument(failure).toJson(QJsonDocument::Compact) + '\n');
        return;
      } else if (method == "attachment.inspect") {
        ++inspectCount;
        result = {{"name", "voice-note.ogg"},
                  {"kind", "audio"},
                  {"mime", "audio/ogg; codecs=opus"},
                  {"audio_codec", "opus"},
                  {"mode", "voice-note"},
                  {"is_voice_note", true},
                  {"state", "ready"},
                  {"url", QUrl::fromLocalFile(source.fileName()).toString()}};
      } else if (method == "message.attach") {
        ++sendCount;
        result = {{"message", QJsonObject{{"id", "unexpected"},
                                           {"chat_id", "fixture:self"},
                                           {"status", "accepted"}}}};
      }
      backend.send(socket, responseFrame(id, result));
    };

    Controller controller(nullptr, {}, profile.mountVerifier());
    controller.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    controller.selectChat(QStringLiteral("fixture:self"));
    QTRY_VERIFY_WITH_TIMEOUT(!controller.loading(), 3000);

    controller.stageAttachmentAs(QUrl::fromLocalFile(source.fileName()),
                                 QStringLiteral("voice-note"));
    QTRY_COMPARE_WITH_TIMEOUT(
        controller.attachment().value("state").toString(),
        QStringLiteral("error"), 3000);
    QCOMPARE(inspectCount, 0);
    controller.send();
    QCoreApplication::processEvents();
    QCOMPARE(sendCount, 0);
  }
  void boundedCoalescedReceiveAndCallbacks() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    FakeBackend backend(root.path() + "/framing.sock");
    QVERIFY(backend.connectionCount == 0);
    QList<QByteArray> largeReplies;
    backend.onRequest = [&backend, &largeReplies](QLocalSocket *socket,
                                                  const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      if (method == "app.status") {
        backend.sendFragmented(socket, statusFrame(id, "fragmented status"));
      } else if (method == "chat.list") {
        backend.send(socket, responseFrame(id, {{"chats", QJsonArray{}}}));
      } else if (method == "draft.get") {
        largeReplies.append(
            responseFrame(id, {{"text", "framed draft"}}, MaxFrame));
        if (largeReplies.size() == 2)
          backend.sendCoalesced(socket,
                                {largeReplies.at(1), largeReplies.at(0)});
      } else if (method == "message.list") {
        largeReplies.append(responseFrame(
            id,
            {{"messages", QJsonArray{QJsonObject{{"id", "matched"}}}},
             {"has_more", false}},
            MaxFrame));
        if (largeReplies.size() == 2) {
          backend.sendCoalesced(socket,
                                {largeReplies.at(1), largeReplies.at(0)});
        } else {
          backend.send(
              socket,
              responseFrame(
                  id, {{"messages", QJsonArray{QJsonObject{{"id", "latest"}}}},
                       {"has_more", false}}));
        }
      }
    };

    Controller c;
    c.start(root.path() + "/framing.sock", root.path() + "/profile", false);
    QTRY_COMPARE_WITH_TIMEOUT(c.status(), QStringLiteral("fragmented status"),
                              3000);
    c.selectChat("fixture:one");
    QTRY_COMPARE_WITH_TIMEOUT(c.draft(), QStringLiteral("framed draft"), 5000);
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 1, 5000);
    QCOMPARE(c.messages()->row(0).value("id").toString(),
             QStringLiteral("matched"));
    QVERIFY(c.connected());

    c.latest();
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 1, 2000);
    QTRY_COMPARE_WITH_TIMEOUT(c.messages()->row(0).value("id").toString(),
                              QStringLiteral("latest"), 2000);
    QTRY_VERIFY_WITH_TIMEOUT(!c.loading(), 2000);
  }
  void frameBoundariesBadFramesAndReconnect() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    FakeBackend backend(root.path() + "/boundary.sock");
    bool sentOversized = false;
    bool sentOversizedFrame = false;
    backend.onRequest = [&backend, &sentOversized, &sentOversizedFrame](
                            QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      if (method == "app.status") {
        backend.send(socket, statusFrame(id, "boundary status"));
      } else if (method == "chat.list") {
        backend.send(socket, responseFrame(id, {{"chats", QJsonArray{}}}));
      } else if (method == "draft.get") {
        const auto exact =
            responseFrame(id, {{"text", "after bad frames"}}, MaxFrame);
        backend.send(socket, QByteArray("\nnot-json\n") + exact);
      } else if (method == "message.list" && !sentOversized) {
        sentOversized = true;
        backend.send(
            socket,
            responseFrame(
                id, {{"messages", QJsonArray{QJsonObject{{"id", "usable"}}}},
                     {"has_more", false}}));
      } else if (method == "message.list") {
        sentOversizedFrame = true;
        backend.send(socket, QByteArray(MaxFrame, 'x') + '\n');
      }
    };

    Controller c;
    c.start(root.path() + "/boundary.sock", root.path() + "/profile", false);
    QTRY_COMPARE_WITH_TIMEOUT(c.status(), QStringLiteral("boundary status"),
                              3000);
    c.selectChat("fixture:boundary");
    QTRY_COMPARE_WITH_TIMEOUT(c.draft(), QStringLiteral("after bad frames"),
                              5000);
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 1, 5000);
    QCOMPARE(c.messages()->row(0).value("id").toString(),
             QStringLiteral("usable"));
    QVERIFY(c.connected());

    c.latest();
    QTRY_VERIFY_WITH_TIMEOUT(sentOversizedFrame, 3000);
    backend.stopListening();
    QTRY_VERIFY_WITH_TIMEOUT(!c.connected(), 3000);
    QVERIFY(c.error().contains("exceeded"));

    FakeBackend partialBackend(root.path() + "/partial.sock");
    bool sentPartial = false;
    partialBackend.onRequest = [&partialBackend,
                                &sentPartial](QLocalSocket *socket,
                                              const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      if (method == "app.status") {
        partialBackend.send(socket, statusFrame(id, "partial status"));
      } else if (method == "chat.list") {
        partialBackend.send(socket,
                            responseFrame(id, {{"chats", QJsonArray{}}}));
      } else if (method == "draft.get" && !sentPartial) {
        sentPartial = true;
        partialBackend.send(socket, QByteArray(MaxFrame, 'p'));
        partialBackend.stopListening();
      }
    };
    Controller partial;
    partial.start(root.path() + "/partial.sock", root.path() + "/profile",
                  false);
    QTRY_COMPARE_WITH_TIMEOUT(partial.status(),
                              QStringLiteral("partial status"), 3000);
    partial.selectChat("fixture:partial");
    QTRY_VERIFY_WITH_TIMEOUT(!partial.connected(), 4000);
  }
  void reconnectClearsIncompleteFrame() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    FakeBackend backend(root.path() + "/reconnect.sock");
    bool staleSent = false;
    backend.onRequest = [&backend, &staleSent](QLocalSocket *socket,
                                               const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      if (method == "app.status") {
        backend.send(socket, statusFrame(id, backend.connectionCount == 1
                                                 ? "first"
                                                 : "reconnected"));
      } else if (method == "chat.list") {
        backend.send(socket, responseFrame(id, {{"chats", QJsonArray{}}}));
      } else if (method == "draft.get" && !staleSent) {
        staleSent = true;
        auto partial = responseFrame(id, {{"text", "stale"}});
        partial.chop(1);
        backend.send(socket, partial);
        QPointer<QLocalSocket> guarded(socket);
        QTimer::singleShot(20, &backend, [guarded] {
          if (guarded)
            guarded->disconnectFromServer();
        });
      }
    };

    Controller c;
    c.start(root.path() + "/reconnect.sock", root.path() + "/profile", false);
    QTRY_COMPARE_WITH_TIMEOUT(c.status(), QStringLiteral("first"), 3000);
    c.selectChat("fixture:stale");
    QTRY_VERIFY_WITH_TIMEOUT(backend.connectionCount >= 2, 5000);
    QTRY_COMPARE_WITH_TIMEOUT(c.status(), QStringLiteral("reconnected"), 3000);
    QVERIFY(c.connected());
    QVERIFY(c.draft() != QStringLiteral("stale"));
  }
  void draftIsolationAndStaleRepliesWithRealBackend() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    Controller c;
    c.start(QString(), root.path() + "/profile", false);
    QTRY_COMPARE_WITH_TIMEOUT(c.chats()->rowCount(), 4, 8000);
    QVERIFY(c.ownsBackend());
    c.selectChat("demo:alex");
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 50, 3000);
    c.setDraft("  pending draft\\n");
    c.selectChat("demo:maya");
    c.setDraft("typed before draft reply");
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 3, 3000);
    QCOMPARE(c.draft(), QStringLiteral("typed before draft reply"));
    c.selectChat("demo:alex");
    QTRY_COMPARE_WITH_TIMEOUT(c.draft(), QStringLiteral("  pending draft\\n"),
                              3000);
  }
  void localFindRevealAndAttachmentWithRealBackend() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    Controller c;
    c.start(QString(), root.path() + "/profile", false);
    QTRY_COMPARE_WITH_TIMEOUT(c.chats()->rowCount(), 4, 8000);
    c.selectChat("demo:alex");
    QTRY_COMPARE_WITH_TIMEOUT(c.messageCount(), 50, 3000);
    c.findMessages("Morning!");
    QTRY_VERIFY_WITH_TIMEOUT(c.searchResults()->rowCount() > 0, 3000);
    auto id = c.searchResults()
                  ->row(c.searchResults()->rowCount() - 1)
                  .value("id")
                  .toString();
    c.revealMessage(id);
    QTRY_VERIFY_WITH_TIMEOUT(c.messages()->indexOf(id) >= 0, 3000);
    QImage image(2, 2, QImage::Format_RGB32);
    image.fill(Qt::blue);
    auto file = root.path() + "/test.png";
    QVERIFY(image.save(file));
    c.stageAttachment(QUrl::fromLocalFile(file));
    QTRY_COMPARE_WITH_TIMEOUT(c.attachment().value("kind").toString(),
                              QStringLiteral("image"), 3000);
    c.setDraft("local image caption");
    c.send();
    QTRY_VERIFY_WITH_TIMEOUT(!c.sending() && c.attachment().isEmpty(), 3000);
    c.latest();
    QTRY_VERIFY_WITH_TIMEOUT(
        c.messages()->row(c.messageCount() - 1).contains("attachment"), 3000);
    QCOMPARE(c.messages()->row(c.messageCount() - 1).value("body").toString(),
             QStringLiteral("local image caption"));
  }
  void capabilityGatesProtocolMessageActions() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("capabilities.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    bool receivedReaction = false;
    int reactionRequests = 0;
    QJsonObject reactionParams;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"message.react"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{QJsonObject{{"id", "target"},
                                                        {"chat_id", "fixture:one"},
                                                        {"body", "original"}}}},
                  {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "message.react") {
        ++reactionRequests;
        receivedReaction = true;
        reactionParams = request.value("params").toObject();
        result = {{"accepted", true}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    c.selectChat("fixture:one");
    QTRY_COMPARE(c.messageCount(), 1);
    QTRY_VERIFY(c.hasCapability("message.react"));
    QVERIFY(!c.hasCapability("message.edit"));
    const auto message = c.messages()->row(0);
    auto protectedMessage = message;
    protectedMessage.insert("protected", true);
    QVERIFY(!c.performMessageAction("message.react", protectedMessage,
                                    {{"reaction", QString::fromUtf8("👍")}}));
    QCOMPARE(reactionRequests, 0);
    auto staleMessage = message;
    staleMessage.insert("id", "not-in-current-history");
    QVERIFY(!c.performMessageAction("message.react", staleMessage,
                                    {{"reaction", QString::fromUtf8("👍")}}));
    QCOMPARE(reactionRequests, 0);
    auto protectedInHistory = message;
    protectedInHistory.insert("view_once", true);
    c.messages()->replace({protectedInHistory});
    QVERIFY(!c.performMessageAction("message.react", message,
                                    {{"reaction", QString::fromUtf8("👍")}}));
    QCOMPARE(reactionRequests, 0);
    c.messages()->replace({message});
    QVERIFY(c.performMessageAction("message.react", message,
                                   {{"reaction", QString::fromUtf8("👍")}}));
    QTRY_VERIFY(receivedReaction);
    QCOMPARE(reactionParams.value("chat_id").toString(), QStringLiteral("fixture:one"));
    QCOMPARE(reactionParams.value("target_id").toString(), QStringLiteral("target"));
    QCOMPARE(reactionParams.value("reaction").toString(), QString::fromUtf8("👍"));
    QVERIFY(!reactionParams.value("client_id").toString().isEmpty());
    QVERIFY(!c.messages()->row(0).contains("reactions"));
    QVERIFY(!c.performMessageAction("message.edit", message,
                                    {{"text", "changed"}}));
    QVERIFY(!c.messages()->row(0).contains("edited"));
  }
  void localHubResultsStayBoundToSelectedChat_data() {
    QTest::addColumn<QString>("method");
    QTest::newRow("media") << QStringLiteral("media.list");
    QTest::newRow("info") << QStringLiteral("chat.info");
  }
  void localHubResultsStayBoundToSelectedChat() {
    QFETCH(QString, method);
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/hubs.sock");
    QLocalSocket *pending = nullptr;
    QString pendingID;
    int calls = 0;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto name = request.value("method").toString();
      QJsonObject result;
      if (name == "app.status") result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (name == "app.capabilities") result = {{"version", 1}, {"methods", QJsonArray{"media.list", "chat.info"}}};
      else if (name == method) {++calls; pending=socket; pendingID=request.value("id").toString(); return;}
      backend.send(socket, responseFrame(request.value("id").toString(), result));
    };
    Controller c;
    c.start(root.path() + "/hubs.sock", root.path() + "/profile", false);
    QTRY_VERIFY(c.connected() && c.hasCapability(method));
    c.selectChat("fixture:a");
    QVERIFY(!c.loadHub(method, {{"chat_id", "fixture:b"}}));
    QVERIFY(!c.loadHub(method, {}));
    QVERIFY(c.loadHub(method, {{"chat_id", "fixture:a"}}));
    QTRY_COMPARE(calls, 1);
    c.selectChat("fixture:b");
    backend.send(pending, responseFrame(pendingID, {{"chat_id", "fixture:a"}, {"title", "stale"}}));
    QTest::qWait(80);
    QVERIFY(c.routeData().isEmpty());
    QVERIFY(!c.routeLoading());
    QVERIFY(c.loadHub(method, {{"chat_id", "fixture:b"}}));
    QTRY_COMPARE(calls, 2);
    backend.send(pending, responseFrame(pendingID, {{"chat_id", "fixture:a"}, {"title", "wrong identity"}}));
    QTRY_VERIFY(!c.routeLoading());
    QVERIFY(c.routeData().isEmpty());
    QVERIFY(!c.routeError().isEmpty());
  }
  void globalMediaLibraryLoadsWithoutSelectedChat() {
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/global-media.sock");
    int calls = 0;
    QJsonObject receivedParams;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status") {
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      } else if (method == "app.capabilities") {
        result = {{"version", 1},
                  {"methods", QJsonArray{"media.library"}},
                  {"features", QJsonObject{{"shared-media-library",
                                             QJsonObject{{"available", true},
                                                         {"reason", "retained cache only"}}}}}};
      } else if (method == "media.library") {
        ++calls;
        receivedParams = request.value("params").toObject();
        result = {{"entries", QJsonArray{QJsonObject{{"chat_id", "fixture:a"},
                                                       {"chat_title", "Synthetic A"},
                                                       {"id", "same"},
                                                       {"timestamp", 10},
                                                       {"kind", "image"},
                                                       {"body", "caption"}}}},
                  {"has_more", false}, {"provenance", "local_cached"}};
      }
      backend.send(socket, responseFrame(request.value("id").toString(), result));
    };
    Controller c;
    c.start(root.path() + "/global-media.sock", root.path() + "/profile", false);
    QTRY_VERIFY(c.connected() && c.capabilitiesReady());
    QCOMPARE(c.selectedId(), QString());
    QVERIFY(c.hasCapability("media.library"));
    QCOMPARE(c.capabilityReason("media.library"), QStringLiteral("retained cache only"));
    QVERIFY(c.loadHub("media.library", {}));
    QTRY_COMPARE(calls, 1);
    QCOMPARE(receivedParams, QJsonObject());
    QTRY_VERIFY(!c.routeLoading());
    QCOMPARE(c.routeData().value("entries").toList().size(), 1);
    QCOMPARE(c.routeData().value("entries").toList().first().toMap().value("chat_title").toString(),
             QStringLiteral("Synthetic A"));
  }
  void mediaLibraryRevealDoesNotRequireChatInFilteredSidebar() {
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/filtered-media.sock");
    int contexts = 0;
    QString contextChat;
    QString contextMessage;
    QStringList calls;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto method = request.value("method").toString();
      calls.append(method);
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"media.library"}}};
      else if (method == "media.library")
        result = {{"entries", QJsonArray{QJsonObject{{"chat_id", "fixture:hidden"},
                   {"chat_title", "Cached hidden chat"}, {"id", "same"}}}},
                  {"has_more", false}, {"provenance", "local_cached"}};
      else if (method == "message.context") {
        ++contexts;
        contextChat = request.value("params").toObject().value("chat_id").toString();
        contextMessage = request.value("params").toObject().value("message_id").toString();
      }
      backend.send(socket, responseFrame(request.value("id").toString(), result));
    };
    Controller c;
    c.start(root.path() + "/filtered-media.sock", root.path() + "/profile", false);
    QTRY_VERIFY(c.connected() && c.capabilitiesReady());
    QVERIFY(c.loadHub("media.library", {}));
    QTRY_VERIFY(!c.routeLoading());
    QCOMPARE(c.chats()->rowCount(), 0);
    QVERIFY(c.revealMediaLibraryMessage("fixture:hidden", "same"));
    QTRY_COMPARE(c.selectedId(), QStringLiteral("fixture:hidden"));
    QTRY_COMPARE(contexts, 1);
    QCOMPARE(contextChat, QStringLiteral("fixture:hidden"));
    QCOMPARE(contextMessage, QStringLiteral("same"));
    QVERIFY(calls.indexOf("message.list") < calls.indexOf("message.context"));
    QVERIFY(!calls.contains("message.download"));
  }
  void notificationSubscriptionIsOptInAndDiscardsStaleResponses() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("notify.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    int polls = 0;
    QJsonObject lastParams;
    QLocalSocket *pending = nullptr;
    QString pendingID;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status") result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities") result = {{"version", 1}, {"methods", QJsonArray{"notification.poll"}}};
      else if (method == "notification.poll") {
        ++polls; lastParams = request.value("params").toObject();
        pending = socket; pendingID = request.value("id").toString(); return;
      }
      backend.send(socket, responseFrame(request.value("id").toString(), result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && c.encryptedProfileViewReady() &&
                c.hasCapability("notification.poll"));
    QTest::qWait(1100);
    QCOMPARE(polls, 0);
    QVERIFY(QMetaObject::invokeMethod(&c, "setNotificationSubscription", Q_ARG(bool, true), Q_ARG(bool, false)));
    QSignalSpy notifications(&c, SIGNAL(incomingNotification(QVariantMap)));
    QVERIFY(notifications.isValid());
    QTRY_COMPARE(polls, 1);
    QCOMPARE(lastParams.value("cursor").toString(), QString());
    QCOMPARE(lastParams.value("include_preview").toBool(), false);
    backend.send(pending, responseFrame(pendingID, {{"cursor", "epoch:0"}, {"events", QJsonArray{}}}));
    QTRY_COMPARE(polls, 2);
    QCOMPARE(lastParams.value("cursor").toString(), QStringLiteral("epoch:0"));
    QVERIFY(QMetaObject::invokeMethod(&c, "setNotificationSubscription", Q_ARG(bool, false), Q_ARG(bool, false)));
    backend.send(pending, responseFrame(pendingID, {{"cursor", "epoch:1"}, {"events", QJsonArray{QJsonObject{{"chat_id", "fixture"}, {"message_id", "inbound"}, {"title", "Synthetic"}}}}}));
    QTest::qWait(120);
    QCOMPARE(notifications.count(), 0);
    QVERIFY(QMetaObject::invokeMethod(&c, "setNotificationSubscription", Q_ARG(bool, true), Q_ARG(bool, true)));
    QTRY_COMPARE(polls, 3);
    QCOMPARE(lastParams.value("cursor").toString(), QString());
    QCOMPARE(lastParams.value("include_preview").toBool(), true);
    backend.send(pending, responseFrame(pendingID, {{"cursor", "epoch:2"}, {"events", QJsonArray{QJsonObject{{"chat_id", "fixture"}, {"message_id", "fresh"}, {"title", "Synthetic"}, {"body", "explicit preview"}}}}}));
    QTRY_COMPARE(notifications.count(), 1);
    QCOMPARE(notifications.first().first().toMap().value("message_id").toString(), QStringLiteral("fresh"));
  }
  void notificationPollReachesOptInDesktopDelivery() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("notification-delivery.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    int polls = 0;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status") {
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      } else if (method == "app.capabilities") {
        result = {{"version", 1}, {"methods", QJsonArray{"notification.poll"}}};
      } else if (method == "notification.poll") {
        ++polls;
        const bool includePreview = request.value("params")
                                        .toObject()
                                        .value("include_preview")
                                        .toBool();
        QJsonObject event{
            {"chat_id", "fixture:incoming"},
            {"message_id", includePreview ? "with-preview" : "generic"},
            {"title", "Synthetic sender"}};
        if (includePreview)
          event.insert("body", "consented preview");
        result = {{"cursor", QString("epoch:%1").arg(polls)},
                  {"reset", false},
                  {"events", QJsonArray{event}}};
      }
      backend.send(socket, responseFrame(id, result));
    };

    class RecordingDelivery final : public DesktopNotificationDelivery {
    public:
      struct Notification {
        QString title;
        QString body;
        int timeoutMs;
      };
      bool trayAvailable() const override { return true; }
      bool notificationsAvailable() const override { return true; }
      void setTrayVisible(bool) override {}
      void setToolTip(const QString &) override {}
      void showNotification(const QString &title, const QString &body,
                            int timeoutMs) override {
        shown.append({title, body, timeoutMs});
      }
      void setOpenHandler(std::function<void()>) override {}
      void setQuitHandler(std::function<void()>) override {}
      void setNotificationClickHandler(std::function<void()>) override {}
      QList<Notification> shown;
    };

    QSettings settings(root.filePath("notifications.ini"),
                       QSettings::IniFormat);
    auto *delivery = new RecordingDelivery;
    DesktopNotifications desktopNotifications(
        &settings, std::unique_ptr<DesktopNotificationDelivery>(delivery));
    Controller controller(nullptr, {}, profile.mountVerifier());
    QObject::connect(
        &controller, &Controller::incomingNotification, &desktopNotifications,
        [&desktopNotifications, &controller](const QVariantMap &event) {
          desktopNotifications.notifyLiveInboundEvent(event, controller.demo(),
                                                      false);
        });
    auto updateSubscription = [&] {
      controller.setNotificationSubscription(
          desktopNotifications.notificationsEnabled(),
          desktopNotifications.messagePreviewsEnabled());
    };
    QObject::connect(&desktopNotifications,
                     &DesktopNotifications::preferencesChanged, &controller,
                     updateSubscription);
    updateSubscription();
    controller.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(controller.connected() && controller.encryptedProfileViewReady() &&
                controller.hasCapability("notification.poll"));
    QTest::qWait(1100);
    QCOMPARE(polls, 0);

    desktopNotifications.setNotificationsEnabled(true);
    QTRY_COMPARE(polls, 1);
    QTRY_COMPARE(delivery->shown.size(), 1);
    QCOMPARE(delivery->shown.at(0).title, QStringLiteral("Synthetic sender"));
    QCOMPARE(delivery->shown.at(0).body, QStringLiteral("New message"));

    desktopNotifications.setMessagePreviewsEnabled(true);
    QTRY_VERIFY(polls >= 2);
    QTRY_COMPARE(delivery->shown.size(), 2);
    QCOMPARE(delivery->shown.at(1).body, QStringLiteral("consented preview"));
  }
  void pollVotingValidatesStoredOptionsAndDoesNotReplayUnknown_data() {
    QTest::addColumn<QString>("outcome");
    QTest::newRow("unknown-error") << QStringLiteral("unknown");
    QTest::newRow("missing-returned-target") << QStringLiteral("missing");
    QTest::newRow("accepted-authoritative-target") << QStringLiteral("accepted");
  }
  void pollVotingValidatesStoredOptionsAndDoesNotReplayUnknown() {
    QFETCH(QString, outcome);
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/poll-vote.sock");
    int votes = 0;
    QJsonObject voteParams;
    QLocalSocket *pendingSocket = nullptr;
    QString pendingID;
    const QJsonObject poll{{"question", "Plan?"},
                           {"options", QJsonArray{"Beach", "Hiking"}},
                           {"selectable_options_count", 1}};
    const QJsonObject target{{"id", "poll"}, {"chat_id", "fixture:poll"},
                             {"kind", "poll"}, {"poll", poll}, {"body", "Plan?"}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"poll.vote"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{target}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "poll.vote") {
        ++votes;
        voteParams = request.value("params").toObject();
        pendingSocket = socket;
        pendingID = id;
        return;
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c;
    c.start(root.path() + "/poll-vote.sock", root.path() + "/profile", false);
    QTRY_VERIFY(c.connected() && c.hasCapability("poll.vote"));
    c.selectChat("fixture:poll");
    QTRY_COMPARE(c.messageCount(), 1);
    const auto message = c.messages()->row(0);
    for (const auto &indices : {QVariantList{0, 1}, QVariantList{0, 0},
                                QVariantList{-1}, QVariantList{2},
                                QVariantList{0.5}, QVariantList{QStringLiteral("0")}})
      QVERIFY(!c.performMessageAction("poll.vote", message, {{"option_indices", indices}}));
    auto protectedRow = message;
    protectedRow.insert("protected", true);
    c.messages()->replace({protectedRow});
    QVERIFY(!c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{0}}}));
    c.messages()->replace({message});
    auto forged = message;
    forged.insert("poll", QVariantMap{{"options", QVariantList{"Invented"}},
                                      {"selectable_options_count", 5}});
    QVERIFY(!c.performMessageAction("poll.vote", forged, {{"option_indices", QVariantList{2}}}));
    QVERIFY(c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{0}}}));
    QVERIFY(!c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{1}}}));
    QTRY_COMPARE(votes, 1);
    QCOMPARE(voteParams.value("chat_id").toString(), QStringLiteral("fixture:poll"));
    QCOMPARE(voteParams.value("target_id").toString(), QStringLiteral("poll"));
    QCOMPARE(voteParams.value("option_indices").toArray(), QJsonArray{0});
    QVERIFY(!voteParams.value("client_id").toString().isEmpty());
    QVERIFY(!c.messages()->row(0).contains("poll_results"));
    if (outcome == "unknown") {
      backend.send(pendingSocket, QJsonDocument(QJsonObject{
          {"v", 1}, {"id", pendingID},
          {"error", QJsonObject{{"code", "unknown"}, {"message", "Outcome unknown; reconcile before retrying"}}}
      }).toJson(QJsonDocument::Compact) + '\n');
    } else {
      backend.send(pendingSocket, responseFrame(pendingID, outcome == "accepted"
          ? QJsonObject{{"message", target}} : QJsonObject{{"accepted", true}}));
    }
    if (outcome == "accepted") {
      QTest::qWait(80);
      QVERIFY(c.error().isEmpty());
      QVERIFY(c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{1}}}));
      QTRY_COMPARE(votes, 2);
    } else {
      QTRY_VERIFY(!c.error().isEmpty());
      QVERIFY(!c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{1}}}));
      QCOMPARE(votes, 1);
    }
    QVERIFY(!c.messages()->row(0).contains("poll_results"));
  }
  void pollVotingBlocksDurableUnresolvedTargetAfterControllerRestart_data() {
    QTest::addColumn<QString>("state");
    QTest::newRow("unknown") << QStringLiteral("unknown");
    QTest::newRow("pending") << QStringLiteral("pending");
    QTest::newRow("unrecognized-fail-closed") << QStringLiteral("future-state");
  }
  void pollVotingBlocksDurableUnresolvedTargetAfterControllerRestart() {
    QFETCH(QString, state);
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/durable-poll.sock");
    int sends = 0;
    const QJsonObject target{{"id", "poll"}, {"chat_id", "fixture:poll"},
                            {"kind", "poll"}, {"poll_vote_state", state},
                            {"poll", QJsonObject{{"question", "Plan?"},
                                                {"options", QJsonArray{"Beach", "Hiking"}}}}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status") result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities") result = {{"version", 1}, {"methods", QJsonArray{"poll.vote"}}};
      else if (method == "message.list") result = {{"messages", QJsonArray{target}}, {"has_more", false}};
      else if (method == "poll.vote") ++sends;
      backend.send(socket, responseFrame(request.value("id").toString(), result));
    };
    for (int restart = 0; restart < 2; ++restart) {
      Controller c;
      c.start(root.path() + "/durable-poll.sock", root.path() + "/profile", false);
      QTRY_VERIFY(c.connected() && c.hasCapability("poll.vote"));
      c.selectChat("fixture:poll");
      QTRY_COMPARE(c.messageCount(), 1);
      const auto message = c.messages()->row(0);
      QCOMPARE(message.value("poll_vote_state").toString(), state);
      QVERIFY(!c.performMessageAction("poll.vote", message, {{"option_indices", QVariantList{0}}}));
      QCOMPARE(sends, 0);
    }
  }
  void capabilityFeaturesGateRelatedMethods() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("feature-capabilities.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1},
                  {"methods", QJsonArray{"contact.list", "chat.open", "message.edit", "message.delete"}},
                  {"features", QJsonObject{
                      {"chat-creation-roster-management", QJsonObject{
                           {"available", false},
                           {"reason", "A connected account is required."}}},
                      {"edit-message", QJsonObject{{"available", false}}},
                      {"delete-message", QJsonObject{{"available", true}}}}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    QVERIFY(!c.hasCapability("contact.list"));
    QVERIFY(!c.hasCapability("chat.open"));
    QCOMPARE(c.capabilityReason("contact.list"),
             QStringLiteral("A connected account is required."));
    QCOMPARE(c.capabilityReason("chat.open"),
             QStringLiteral("A connected account is required."));
    QVERIFY(!c.hasCapability("message.edit"));
    QVERIFY(c.hasCapability("message.delete"));
  }
  void accountAndPrivacyActionsRespectFeatureBoundaries() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("profile-privacy.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    QStringList requestedMethods;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      requestedMethods.append(method);
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1},
                  {"methods", QJsonArray{"account.update", "privacy.set"}},
                  {"features", QJsonObject{
                      {"profile-edit", QJsonObject{
                           {"available", false},
                           {"reason", "Profile editing has no reviewed service contract."}}},
                      {"privacy-security-settings", QJsonObject{
                           {"available", false},
                           {"reason", "Privacy settings have no reviewed service contract."}}}}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    QVERIFY(!c.hasCapability("account.update"));
    QVERIFY(!c.hasCapability("privacy.set"));
    QVERIFY(!c.performCapabilityAction("account.update",
                                       {{"display_name", "Synthetic"}}));
    QVERIFY(!c.performCapabilityAction("privacy.set", {{"last_seen", "nobody"}}));
    QVERIFY(!requestedMethods.contains("account.update"));
    QVERIFY(!requestedMethods.contains("privacy.set"));
  }
  void localDisconnectCapabilityAndActionStayRemoteUnlinkFree() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("disconnect-scope.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    bool localDisconnectAvailable = false;
    int localDisconnectCalls = 0;
    int remoteLogoutCalls = 0;
    QStringList requestedMethods;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      requestedMethods.append(method);
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1},
                  {"methods", QJsonArray{"auth.disconnect"}},
                  {"features", QJsonObject{
                      {"local-disconnect", QJsonObject{
                           {"available", localDisconnectAvailable}}}}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "auth.disconnect") {
        ++localDisconnectCalls;
        result = {{"disconnected", false}, {"in_progress", true}};
      } else if (method == "auth.unlink" || method == "auth.logout")
        ++remoteLogoutCalls;
      backend.send(socket, responseFrame(id, result));
    };
    {
      Controller c(nullptr, {}, profile.mountVerifier());
      c.start(socketPath, profile.ciphertextRoot, true);
      QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
      QTRY_VERIFY(c.capabilitiesReady());
      QVERIFY(!c.hasCapability("auth.disconnect"));
    }
    localDisconnectAvailable = true;
    {
      Controller c(nullptr, {}, profile.mountVerifier());
      c.start(socketPath, profile.ciphertextRoot, true);
      QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
      QTRY_VERIFY(c.capabilitiesReady());
      QVERIFY(c.hasCapability("auth.disconnect"));
      c.disconnectLive();
      QTRY_COMPARE(localDisconnectCalls, 1);
      QCOMPARE(remoteLogoutCalls, 0);
      QVERIFY(requestedMethods.contains("auth.disconnect"));
      QVERIFY(!requestedMethods.contains("auth.unlink"));
      QVERIFY(!requestedMethods.contains("auth.logout"));
    }
  }
  void contactRosterPagingStateClearsOnDisconnect() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("contact-roster.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"contact.list"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "contact.list")
        result = {{"contacts", QJsonArray{QJsonObject{
                       {"jid", "fixture:contact"}, {"name", "Synthetic"},
                       {"phone", "15551234567"}, {"verified", false}}}},
                  {"has_more", true}, {"next_cursor", "fixture:contact"}};
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    c.searchContacts(QString());
    QTRY_VERIFY_WITH_TIMEOUT(c.contactsHasMore(), 2500);
    QCOMPARE(c.contacts()->rowCount(), 1);

    backend.stopListening();
    backend.disconnectClients();
    QTRY_VERIFY_WITH_TIMEOUT(!c.connected(), 1000);
    QVERIFY(!c.contactsHasMore());
    QCOMPARE(c.contacts()->rowCount(), 0);
  }
  void unreadMarkUpdateUsesBooleanInputAndAuthoritativeProjection() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("chat-update.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    QPointer<QLocalSocket> pendingSocket;
    QString pendingId;
    QJsonObject pendingParams;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"chat.update"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:one"},
                                                    {"title", "First"},
                                                    {"unread", 3},
                                                    {"marked_unread", false}}}}};
      else if (method == "chat.update") {
        pendingSocket = socket;
        pendingId = id;
        pendingParams = request.value("params").toObject();
        return;
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    QTRY_COMPARE(c.chats()->rowCount(), 1);

    QVERIFY(c.updateChat("fixture:one", {{"unread", true}}));
    QTRY_VERIFY(pendingSocket);
    QCOMPARE(pendingParams.value("chat_id").toString(), QStringLiteral("fixture:one"));
    QVERIFY(pendingParams.value("unread").isBool());
    QVERIFY(pendingParams.value("unread").toBool());
    QVERIFY(!pendingParams.contains("marked_unread"));
    QCOMPARE(c.chats()->row(0).value("unread").toInt(), 3);
    QVERIFY(!c.chats()->row(0).value("marked_unread").toBool());
    backend.send(pendingSocket, responseFrame(pendingId,
        {{"chat", QJsonObject{{"id", "fixture:one"}, {"title", "First"},
                              {"unread", 3}, {"marked_unread", true}}}}));
    QTRY_VERIFY(c.chats()->row(0).value("marked_unread").toBool());
    QCOMPARE(c.chats()->row(0).value("unread").toInt(), 3);

    pendingSocket = nullptr;
    pendingId.clear();
    QVERIFY(c.updateChat("fixture:one", {{"unread", false}}));
    QTRY_VERIFY(pendingSocket);
    QVERIFY(pendingParams.value("unread").isBool());
    QVERIFY(!pendingParams.value("unread").toBool());
    QVERIFY(c.chats()->row(0).value("marked_unread").toBool());
    backend.send(pendingSocket, responseFrame(pendingId,
        {{"chat", QJsonObject{{"id", "fixture:one"}, {"title", "First"},
                              {"unread", 3}, {"marked_unread", false}}}}));
    QTRY_VERIFY(!c.chats()->row(0).value("marked_unread").toBool());
    QCOMPARE(c.chats()->row(0).value("unread").toInt(), 3);
  }
  void staleContactResultsAreDiscardedAfterQueryClears() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socketPath = profile.socketPath(QStringLiteral("contacts.sock"));
    FakeBackend backend(socketPath);
    backend.setEncryptedStorageView(profile.statusView());
    QPointer<QLocalSocket> heldSocket;
    QString heldId;
    QString requestedQuery;
    int chatOpenRequests = 0;
    QJsonObject chatOpenParams;
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", QJsonArray{"contact.list", "chat.open"}}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{}}};
      else if (method == "contact.list") {
        requestedQuery = request.value("params").toObject().value("query").toString();
        if (requestedQuery == "stale") {
          heldSocket = socket;
          heldId = id;
          return;
        }
        result = {{"contacts", QJsonArray{QJsonObject{{"recipient", "verified:alice"}, {"display_name", "Alice"}}}},
                  {"has_more", false}, {"next_cursor", ""}};
      } else if (method == "chat.open") {
        ++chatOpenRequests;
        chatOpenParams = request.value("params").toObject();
        const auto title = chatOpenRequests == 1 ? QStringLiteral("unverified@lid") : QStringLiteral("Alice");
        const auto chatId = chatOpenRequests == 1 ? QStringLiteral("opaque:alice") : QStringLiteral("fixture:alice");
        result = {{"chat", QJsonObject{{"id", chatId}, {"title", title}, {"initials", "A"}}}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socketPath, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    c.searchContacts("stale");
    QTRY_VERIFY_WITH_TIMEOUT(heldSocket, 2000);
    QCOMPARE(requestedQuery, QStringLiteral("stale"));
    c.searchContacts("");
    QTRY_COMPARE_WITH_TIMEOUT(c.contacts()->rowCount(), 1, 3000);
    QCOMPARE(c.contacts()->row(0).value("display_name").toString(), QStringLiteral("Alice"));
    backend.send(heldSocket, responseFrame(heldId,
        {{"contacts", QJsonArray{QJsonObject{{"recipient", "stale:recipient"}, {"display_name", "Stale result"}}}},
         {"has_more", false}, {"next_cursor", ""}}));
    QTest::qWait(250);
    QCOMPARE(c.contacts()->rowCount(), 1);
    QCOMPARE(c.contacts()->row(0).value("display_name").toString(), QStringLiteral("Alice"));
    c.searchContacts("Alice");
    QTRY_COMPARE_WITH_TIMEOUT(c.contacts()->rowCount(), 1, 3000);
    QCOMPARE(c.contacts()->row(0).value("display_name").toString(), QStringLiteral("Alice"));
    c.openChatRecipient("verified:alice");
    QTRY_COMPARE_WITH_TIMEOUT(chatOpenRequests, 1, 3000);
    QTRY_VERIFY_WITH_TIMEOUT(!c.error().isEmpty(), 3000);
    QCOMPARE(chatOpenParams.value("recipient").toString(), QStringLiteral("verified:alice"));
    QVERIFY(c.selectedId().isEmpty());
    c.clearError();
    c.openChatRecipient("verified:alice");
    QTRY_COMPARE_WITH_TIMEOUT(chatOpenRequests, 2, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(c.selectedId(), QStringLiteral("fixture:alice"), 3000);
    QCOMPARE(c.selectedTitle(), QStringLiteral("Alice"));
  }
  void protocolMessageActionsUseDeclaredParameters() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("message-actions.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    QList<QJsonObject> actionRequests;
    const QJsonArray methods{
        "message.reply", "message.react", "message.edit", "message.delete",
        "message.forward", "message.star", "message.pin"};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", methods}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{
            QJsonObject{{"id", "fixture:one"}, {"title", "First"}},
            QJsonObject{{"id", "fixture:two"}, {"title", "Second"}}}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{QJsonObject{
            {"id", "target"}, {"chat_id", "fixture:one"},
            {"body", "original"}, {"outgoing", true}}}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (methods.contains(method))
        actionRequests.append(request);
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    QTRY_COMPARE(c.chats()->rowCount(), 2);
    c.selectChat("fixture:one");
    QTRY_COMPARE(c.messageCount(), 1);
    const auto target = c.messages()->row(0);
    QVERIFY(c.performMessageAction("message.reply", target, {{"text", "reply"}}));
    QVERIFY(c.performMessageAction("message.react", target, {{"reaction", ""}}));
    QVERIFY(c.performMessageAction("message.edit", target, {{"text", "edited"}}));
    QVERIFY(c.performMessageAction("message.delete", target, {{"scope", "local"}}));
    QVERIFY(c.performMessageAction("message.delete", target, {{"scope", "everyone"}}));
    QVERIFY(c.performMessageAction("message.forward", target,
                                   {{"target_chat_id", "fixture:two"}}));
    QVERIFY(c.performMessageAction("message.star", target, {{"starred", true}}));
    QVERIFY(c.performMessageAction("message.pin", target,
                                   {{"pinned", true}, {"duration_seconds", 86400}}));
    QTRY_COMPARE(actionRequests.size(), 8);
    const QStringList expectedMethods{
        "message.reply", "message.react", "message.edit", "message.delete",
        "message.delete", "message.forward", "message.star", "message.pin"};
    for (int i = 0; i < expectedMethods.size(); ++i) {
      const auto request = actionRequests.at(i);
      QCOMPARE(request.value("method").toString(), expectedMethods.at(i));
      QCOMPARE(request.value("params").toObject().value("chat_id").toString(),
               QStringLiteral("fixture:one"));
    }
    const auto reply = actionRequests.at(0).value("params").toObject();
    QCOMPARE(reply.value("target_id").toString(), QStringLiteral("target"));
    QCOMPARE(reply.value("text").toString(), QStringLiteral("reply"));
    QVERIFY(!reply.value("client_id").toString().isEmpty());
    const auto reaction = actionRequests.at(1).value("params").toObject();
    QCOMPARE(reaction.value("target_id").toString(), QStringLiteral("target"));
    QVERIFY(reaction.value("reaction").toString().isEmpty());
    const auto edit = actionRequests.at(2).value("params").toObject();
    QCOMPARE(edit.value("id").toString(), QStringLiteral("target"));
    QCOMPARE(edit.value("text").toString(), QStringLiteral("edited"));
    QCOMPARE(actionRequests.at(3).value("params").toObject().value("scope").toString(), QStringLiteral("local"));
    QCOMPARE(actionRequests.at(4).value("params").toObject().value("scope").toString(), QStringLiteral("everyone"));
    const auto forward = actionRequests.at(5).value("params").toObject();
    QCOMPARE(forward.value("target_chat_id").toString(), QStringLiteral("fixture:two"));
    const auto star = actionRequests.at(6).value("params").toObject();
    QVERIFY(star.value("starred").toBool());
    QVERIFY(!star.contains("client_id"));
    const auto pin = actionRequests.at(7).value("params").toObject();
    QVERIFY(pin.value("pinned").toBool());
    QCOMPARE(pin.value("duration_seconds").toInt(), 86400);
    QVERIFY(!pin.contains("client_id"));
    QCOMPARE(c.messages()->row(0).value("body").toString(), QStringLiteral("original"));
  }
  void listReplyUsesPrivateRowIndicesAndLocksTarget() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("list-reply.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    QList<QJsonObject> actionRequests;
    const QJsonArray methods{"list.reply"};
    const QJsonObject listProjection{
        {"title", "Pick one"}, {"button_label", "Choose"},
        {"sections", QJsonArray{QJsonObject{{"title", "Options"}, {"rows", QJsonArray{
            QJsonObject{{"title", "First"}}, QJsonObject{{"title", "Second"}}}}}}}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", methods}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:one"}, {"title", "One"}}}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{QJsonObject{
            {"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Pick one"},
            {"timestamp", 1000}, {"outgoing", false}, {"kind", "list"},
            {"list", listProjection}, {"list_reply_available", true}}}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "list.reply") {
        actionRequests.append(request);
        result = {{"target", QJsonObject{
                      {"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Pick one"},
                      {"timestamp", 1000}, {"outgoing", false}, {"kind", "list"},
                      {"list", listProjection}, {"list_reply_available", false},
                      {"list_reply_state", "accepted"}}},
                  {"message", QJsonObject{{"id", "reply-1"}, {"chat_id", "fixture:one"},
                                           {"body", "Second"}, {"timestamp", 2000},
                                           {"outgoing", true}, {"status", "accepted"}}},
                  {"status", "accepted"}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller controller(nullptr, {}, profile.mountVerifier());
    controller.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(controller.connected() && !controller.demo());
    QTRY_VERIFY(controller.capabilitiesReady());
    QTRY_COMPARE(controller.chats()->rowCount(), 1);
    controller.selectChat("fixture:one");
    QTRY_COMPARE(controller.messageCount(), 1);
    const auto target = controller.messages()->row(0);
    QVERIFY(!controller.performMessageAction("list.reply", target,
                                              {{"section_index", 0}, {"row_index", 2}}));
    QVERIFY(controller.performMessageAction("list.reply", target,
                                            {{"section_index", 0}, {"row_index", 1},
                                             {"row_id", "forged-row-id"}}));
    QVERIFY(!controller.performMessageAction("list.reply", target,
                                              {{"section_index", 0}, {"row_index", 0}}));
    QTRY_COMPARE(actionRequests.size(), 1);
    const auto params = actionRequests.first().value("params").toObject();
    QCOMPARE(params.value("target_id").toString(), QStringLiteral("target"));
    QCOMPARE(params.value("section_index").toInt(), 0);
    QCOMPARE(params.value("row_index").toInt(), 1);
    QVERIFY(!params.contains("row_id"));
    QVERIFY(!params.value("client_id").toString().isEmpty());
    QTRY_COMPARE(controller.messages()->row(controller.messages()->indexOf("target")).value("list_reply_state").toString(), QStringLiteral("accepted"));
    const auto updatedTarget = controller.messages()->row(controller.messages()->indexOf("target"));
    QVERIFY(!updatedTarget.value("list_reply_available").toBool());
    QVERIFY(!controller.performMessageAction("list.reply", updatedTarget,
                                              {{"section_index", 0}, {"row_index", 0}}));
  }
  void buttonReplyUsesPrivateDisplayIndexAndReconcilesLocalTarget() {
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/button-reply.sock");
    QList<QJsonObject> actionRequests;
    const QJsonArray methods{"button.reply"};
    const QJsonObject buttonsProjection{
        {"header", "Synthetic question"}, {"body", "Choose one"},
        {"footer", "Local demo only"},
        {"buttons", QJsonArray{"First response", "Second response"}}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", methods}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:one"}, {"title", "One"}}}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{QJsonObject{
            {"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Choose one"},
            {"timestamp", 1000}, {"outgoing", false}, {"kind", "buttons"},
            {"buttons", buttonsProjection}, {"button_reply_available", true}}}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "button.reply") {
        actionRequests.append(request);
        result = {{"target", QJsonObject{
                      {"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Choose one"},
                      {"timestamp", 1000}, {"outgoing", false}, {"kind", "buttons"},
                      {"buttons", buttonsProjection}, {"button_reply_available", false},
                      {"button_reply_state", "local"}}},
                  {"message", QJsonObject{
                      {"id", "reply-1"}, {"chat_id", "fixture:one"},
                      {"body", "Second response"}, {"timestamp", 2000},
                      {"outgoing", true}, {"status", "demo_sent"},
                      {"kind", "text"}, {"reply_to_id", "target"}}},
                  {"status", "demo_sent"}};
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller c;
    c.start(root.path() + "/button-reply.sock", root.path() + "/profile", false);
    QTRY_VERIFY(c.connected() && c.demo());
    QTRY_VERIFY(c.capabilitiesReady());
    QTRY_COMPARE(c.chats()->rowCount(), 1);
    c.selectChat("fixture:one");
    QTRY_COMPARE(c.messageCount(), 1);
    const auto target = c.messages()->row(0);
    QVERIFY(!c.performMessageAction("button.reply", target, {{"button_index", 2}}));
    QVERIFY(!c.performMessageAction("button.reply", target, {{"button_index", 1.5}}));
    QVERIFY(!c.performMessageAction("button.reply", target, {{"button_index", "1"}}));
    QCOMPARE(actionRequests.size(), 0);
    QVERIFY(c.performMessageAction("button.reply", target, {{"button_index", 1}}));
    QVERIFY(!c.performMessageAction("button.reply", target, {{"button_index", 0}}));
    QTRY_COMPARE(actionRequests.size(), 1);
    const auto params = actionRequests.first().value("params").toObject();
    QCOMPARE(params.value("chat_id").toString(), QStringLiteral("fixture:one"));
    QCOMPARE(params.value("target_id").toString(), QStringLiteral("target"));
    QCOMPARE(params.value("button_index").toInt(), 1);
    QVERIFY(!params.contains("button_id"));
    QVERIFY(!params.contains("id"));
    QVERIFY(!params.value("client_id").toString().isEmpty());
    QTRY_COMPARE(c.messageCount(), 2);
    const int targetIndex = c.messages()->indexOf("target");
    QVERIFY(targetIndex >= 0);
    const auto updatedTarget = c.messages()->row(targetIndex);
    QCOMPARE(updatedTarget.value("button_reply_state").toString(), QStringLiteral("local"));
    QVERIFY(!updatedTarget.value("button_reply_available").toBool());
    const auto response = c.messages()->row(c.messages()->indexOf("reply-1"));
    QCOMPARE(response.value("body").toString(), QStringLiteral("Second response"));
    QCOMPARE(response.value("reply_to_id").toString(), QStringLiteral("target"));
    QCOMPARE(response.value("status").toString(), QStringLiteral("demo_sent"));
    QVERIFY(!c.performMessageAction("button.reply", updatedTarget, {{"button_index", 0}}));
  }
  void buttonReplyIsRejectedOutsideDemoMode() {
    QTemporaryDir root;
    SyntheticEncryptedProfile profile(root.path());
    QVERIFY(profile.valid);
    const auto socket = profile.socketPath(QStringLiteral("button-reply-live.sock"));
    FakeBackend backend(socket);
    backend.setEncryptedStorageView(profile.statusView());
    int actionRequests = 0;
    const QJsonArray methods{"button.reply"};
    const QJsonObject buttonsProjection{
        {"body", "Choose one"}, {"buttons", QJsonArray{"First response"}}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "live"}, {"state", "connected"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", methods}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:one"}, {"title", "One"}}}}};
      else if (method == "message.list")
        result = {{"messages", QJsonArray{QJsonObject{
            {"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Choose one"},
            {"timestamp", 1000}, {"outgoing", false}, {"kind", "buttons"},
            {"buttons", buttonsProjection}, {"button_reply_available", true}}}}, {"has_more", false}};
      else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "button.reply")
        ++actionRequests;
      backend.send(socket, responseFrame(id, result));
    };
    Controller c(nullptr, {}, profile.mountVerifier());
    c.start(socket, profile.ciphertextRoot, true);
    QTRY_VERIFY(c.connected() && !c.demo() && c.encryptedProfileViewReady());
    QTRY_VERIFY(c.capabilitiesReady());
    QTRY_COMPARE(c.chats()->rowCount(), 1);
    c.selectChat("fixture:one");
    QTRY_COMPARE(c.messageCount(), 1);
    QVERIFY(!c.performMessageAction("button.reply", c.messages()->row(0),
                                   {{"button_index", 0}}));
    QTest::qWait(100);
    QCOMPARE(actionRequests, 0);
  }
  void buttonReplyUnknownOutcomeStaysBlockedAcrossHydration() {
    QTemporaryDir root;
    FakeBackend backend(root.path() + "/button-reply-unknown.sock");
    int actionRequests = 0;
    bool hydrateUnknown = false;
    QLocalSocket *pendingSocket = nullptr;
    QString pendingID;
    const QJsonArray methods{"button.reply"};
    const QJsonObject buttonsProjection{
        {"body", "Choose one"}, {"buttons", QJsonArray{"First response", "Second response"}}};
    backend.onRequest = [&](QLocalSocket *socket, const QJsonObject &request) {
      const auto id = request.value("id").toString();
      const auto method = request.value("method").toString();
      QJsonObject result;
      if (method == "app.status")
        result = {{"mode", "demo"}, {"state", "demo"}, {"revision", 1}};
      else if (method == "app.capabilities")
        result = {{"version", 1}, {"methods", methods}};
      else if (method == "chat.list")
        result = {{"chats", QJsonArray{QJsonObject{{"id", "fixture:one"}, {"title", "One"}}}}};
      else if (method == "message.list") {
        QJsonObject target{{"id", "target"}, {"chat_id", "fixture:one"}, {"body", "Choose one"},
                           {"timestamp", 1000}, {"outgoing", false}, {"kind", "buttons"},
                           {"buttons", buttonsProjection},
                           {"button_reply_available", !hydrateUnknown}};
        if (hydrateUnknown)
          target.insert("button_reply_state", "unknown");
        result = {{"messages", QJsonArray{target}}, {"has_more", false}};
      } else if (method == "draft.get")
        result = {{"text", ""}};
      else if (method == "button.reply") {
        ++actionRequests;
        pendingSocket = socket;
        pendingID = id;
        return;
      }
      backend.send(socket, responseFrame(id, result));
    };
    Controller first;
    first.start(root.path() + "/button-reply-unknown.sock", root.path() + "/profile-one", false);
    QTRY_VERIFY(first.connected() && first.demo() && first.hasCapability("button.reply"));
    QTRY_COMPARE(first.chats()->rowCount(), 1);
    first.selectChat("fixture:one");
    QTRY_COMPARE(first.messageCount(), 1);
    const auto target = first.messages()->row(0);
    QVERIFY(first.performMessageAction("button.reply", target, {{"button_index", 0}}));
    QVERIFY(!first.performMessageAction("button.reply", target, {{"button_index", 1}}));
    QTRY_COMPARE(actionRequests, 1);
    QVERIFY(pendingSocket != nullptr);
    backend.send(pendingSocket, QJsonDocument(QJsonObject{
        {"v", 1}, {"id", pendingID},
        {"error", QJsonObject{{"code", "unknown_outcome"},
                              {"message", "Outcome unknown; reconcile before retrying"}}}
    }).toJson(QJsonDocument::Compact) + '\n');
    QTRY_VERIFY(!first.error().isEmpty());
    QVERIFY(first.buttonReplyBlockedTargets().contains(QStringLiteral("target")));
    QVERIFY(!first.performMessageAction("button.reply", target, {{"button_index", 1}}));
    QCOMPARE(actionRequests, 1);

    hydrateUnknown = true;
    Controller reopened;
    reopened.start(root.path() + "/button-reply-unknown.sock", root.path() + "/profile-two", false);
    QTRY_VERIFY(reopened.connected() && reopened.demo() && reopened.hasCapability("button.reply"));
    QTRY_COMPARE(reopened.chats()->rowCount(), 1);
    reopened.selectChat("fixture:one");
    QTRY_COMPARE(reopened.messageCount(), 1);
    const auto hydrated = reopened.messages()->row(0);
    QCOMPARE(hydrated.value("button_reply_state").toString(), QStringLiteral("unknown"));
    QVERIFY(!hydrated.value("button_reply_available").toBool());
    QVERIFY(!reopened.performMessageAction("button.reply", hydrated, {{"button_index", 1}}));
    QCOMPARE(actionRequests, 1);
  }
  void externalSocketDoesNotSpawn() {
    Controller c;
    c.start("/nonexistent-whatsfeather-test.sock", QString(), false);
    QVERIFY(!c.ownsBackend());
    QCOMPARE(c.messageCount(), 0);
    QVERIFY(c.selectedId().isEmpty());
  }
};
QTEST_GUILESS_MAIN(CoreTest)
#include "test_core.moc"
