#include "controller.h"
#include "localexpressionstore.h"

#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QAbstractItemModel>
#include <QJsonArray>
#include <QJsonDocument>
#include <QLocalServer>
#include <QLocalSocket>
#include <QPointer>
#include <QSignalSpy>
#include <QSet>
#include <QTemporaryDir>
#include <QTest>

#include <functional>

namespace {
class StorageBackend final : public QObject {
public:
  explicit StorageBackend(const QString &path) : m_path(path) {
    QLocalServer::removeServer(m_path);
    if (!m_server.listen(m_path))
      qFatal("could not listen on the private storage test socket");
    connect(&m_server, &QLocalServer::newConnection, this, [this] {
      while (m_server.hasPendingConnections()) {
        auto *peer = m_server.nextPendingConnection();
        m_peers.append(peer);
        connect(peer, &QLocalSocket::readyRead, this, [this, peer] {
          auto &buffer = m_input[peer];
          buffer += peer->readAll();
          while (true) {
            const auto newline = buffer.indexOf('\n');
            if (newline < 0)
              break;
            const auto frame = buffer.left(newline);
            buffer.remove(0, newline + 1);
            QJsonParseError parseError;
            const auto doc = QJsonDocument::fromJson(frame, &parseError);
            if (parseError.error != QJsonParseError::NoError || !doc.isObject())
              continue;
            const auto request = doc.object();
            const auto id = request.value(QStringLiteral("id")).toString();
            const auto method = request.value(QStringLiteral("method")).toString();
            const auto params = request.value(QStringLiteral("params")).toObject();
            ++methodCalls[method];
            QJsonObject result;
            if (method == QStringLiteral("app.status")) {
              result = {{QStringLiteral("mode"), QStringLiteral("live")},
                        {QStringLiteral("state"), QStringLiteral("connected")},
                        {QStringLiteral("message"), QStringLiteral("Synthetic encrypted view")},
                        {QStringLiteral("revision"), statusRevision}};
              if (!storageView.isEmpty())
                result.insert(QStringLiteral("storage_view"), storageView);
              if (!statusAccount.isEmpty())
                result.insert(QStringLiteral("account"), statusAccount);
            } else if (method == QStringLiteral("app.capabilities")) {
              result = {{QStringLiteral("version"), 1},
                        {QStringLiteral("methods"), QJsonArray{QStringLiteral("attachment.prepare"),
                                                               QStringLiteral("privacy.set"),
                                                               QStringLiteral("chat.info"),
                                                               QStringLiteral("contact.list"),
                                                               QStringLiteral("contact.prepare"),
                                                               QStringLiteral("gif.search"),
                                                               QStringLiteral("message.location.send")}},
                        {QStringLiteral("features"),
                         QJsonObject{{QStringLiteral("stickers"),
                                      QJsonObject{{QStringLiteral("available"), true}}}}}};
            } else if (method == QStringLiteral("auth.connect")) {
              ++connectCalls;
            } else if (method == QStringLiteral("chat.list")) {
              result = {{QStringLiteral("chats"), chatRows}};
            } else if (method == QStringLiteral("message.list")) {
              result = {{QStringLiteral("messages"), messageRows},
                        {QStringLiteral("has_more"), false}};
            } else if (method == QStringLiteral("draft.get")) {
              result = {{QStringLiteral("text"), draftText}};
            } else if (method == QStringLiteral("account.get")) {
              result = accountResponse;
            } else if (method == QStringLiteral("contact.list")) {
              result = {{QStringLiteral("contacts"), contactRows},
                        {QStringLiteral("next_cursor"), QString()},
                        {QStringLiteral("has_more"), false}};
            } else if (method == QStringLiteral("contact.prepare")) {
              result = {{QStringLiteral("contacts"),
                         QJsonArray{QJsonObject{{QStringLiteral("name"), QStringLiteral("Synthetic vCard")},
                                               {QStringLiteral("phones"), QJsonArray{}}}}}};
            } else if (method == QStringLiteral("chat.info")) {
              result = {{QStringLiteral("id"), params.value(QStringLiteral("chat_id"))},
                        {QStringLiteral("title"), QStringLiteral("Synthetic private chat")},
                        {QStringLiteral("about"), QStringLiteral("Synthetic private profile metadata")}};
            } else if (method == QStringLiteral("message.search")) {
              result = {{QStringLiteral("messages"), searchRows}};
            } else if (method == QStringLiteral("gif.search")) {
              result = {{QStringLiteral("provider"), QStringLiteral("commons")},
                        {QStringLiteral("items"), QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("synthetic-gif-id")},
                                                                         {QStringLiteral("title"), QStringLiteral("Synthetic GIF result")}}}}};
            } else if (method == QStringLiteral("attachment.prepare")) {
              ++prepareCount;
              const auto sourcePath = params.value(QStringLiteral("path")).toString();
              preparedPath = preparedPathOverride.isEmpty() ? sourcePath : preparedPathOverride;
              const QFileInfo file(preparedPath);
              const auto mode = params.value(QStringLiteral("mode")).toString();
              const bool sticker = mode == QStringLiteral("sticker");
              result = {{QStringLiteral("kind"), QStringLiteral("sticker")},
                        {QStringLiteral("state"), QStringLiteral("ready")},
                        {QStringLiteral("mode"), mode},
                        {QStringLiteral("mime"), QStringLiteral("image/webp")},
                        {QStringLiteral("width"), 512},
                        {QStringLiteral("height"), 512},
                        {QStringLiteral("size"), file.size()},
                        {QStringLiteral("prepared_size"), file.size()},
                        {QStringLiteral("original_size"), QFileInfo(sourcePath).size()},
                        {QStringLiteral("prepared_path"), preparedPath}};
              if (!sticker)
                result.insert(QStringLiteral("kind"), QStringLiteral("image"));
            }
            const auto response = QJsonDocument(
                QJsonObject{{QStringLiteral("id"), id},
                            {QStringLiteral("result"), result}})
                                     .toJson(QJsonDocument::Compact) + '\n';
            if (heldMethods.contains(method)) {
              heldReplies[method].append({peer, response});
              continue;
            }
            peer->write(response);
            peer->flush();
          }
        });
        connect(peer, &QLocalSocket::disconnected, this, [this, peer] {
          m_input.remove(peer);
          m_peers.removeOne(peer);
          peer->deleteLater();
        });
      }
    });
  }

  ~StorageBackend() override {
    for (auto *peer : std::as_const(m_peers))
      if (peer)
        peer->abort();
    m_server.close();
    QLocalServer::removeServer(m_path);
  }

  void pause() {
    m_server.close();
    for (auto *peer : m_peers)
      if (peer)
        peer->abort();
  }

  bool resume() {
    QLocalServer::removeServer(m_path);
    return m_server.listen(m_path);
  }

  void disconnectPeers() {
    for (auto *peer : m_peers)
      if (peer)
        peer->abort();
  }

  void releaseHeld(const QString &method) {
    const auto replies = heldReplies.take(method);
    for (const auto &held : replies) {
      if (!held.peer || held.peer->state() != QLocalSocket::ConnectedState)
        continue;
      held.peer->write(held.bytes);
      held.peer->flush();
    }
  }

  QJsonObject storageView;
  QJsonObject statusAccount;
  QJsonObject accountResponse;
  QJsonArray chatRows;
  QJsonArray messageRows;
  QJsonArray contactRows;
  QJsonArray searchRows;
  QString draftText;
  int statusRevision = 1;
  QString preparedPath;
  QString preparedPathOverride;
  QHash<QString, int> methodCalls;
  QSet<QString> heldMethods;
  struct HeldReply {
    QPointer<QLocalSocket> peer;
    QByteArray bytes;
  };
  QHash<QString, QList<HeldReply>> heldReplies;
  int prepareCount = 0;
  int connectCalls = 0;

private:
  QString m_path;
  QLocalServer m_server;
  QList<QLocalSocket *> m_peers;
  QHash<QLocalSocket *, QByteArray> m_input;
};

QJsonObject encryptedStorageView(const QString &ciphertextRoot,
                                 const QString &runtimeDir,
                                 const QString &profileRoot,
                                 const QString &viewId =
                                     QStringLiteral("0123456789abcdef0123456789abcdef"),
                                 const QString &mountId = QStringLiteral("42")) {
  return {{QStringLiteral("version"), 1},
          {QStringLiteral("mode"), QStringLiteral("encrypted")},
          {QStringLiteral("ciphertext_root"), ciphertextRoot},
          {QStringLiteral("runtime_dir"), runtimeDir},
          {QStringLiteral("profile_root"), profileRoot},
          {QStringLiteral("view_id"), viewId},
          {QStringLiteral("mount_id"), mountId}};
}

bool makePrivateDirectory(const QString &path) {
  return QDir().mkpath(path) &&
         QFile::setPermissions(path, QFileDevice::ReadOwner |
                                         QFileDevice::WriteOwner |
                                         QFileDevice::ExeOwner);
}

Controller::ProfileMountVerifier syntheticMountVerifier() {
  return [](const QString &ciphertextRoot, const QString &runtimeDir,
            const QString &profileRoot, const QString &mountId,
            QString *error) {
    Q_UNUSED(ciphertextRoot);
    if ((mountId == QStringLiteral("42") || mountId == QStringLiteral("43")) &&
        QFileInfo(profileRoot).absolutePath() == runtimeDir)
      return true;
    if (error)
      *error = QStringLiteral("Synthetic mount identity did not match the fixture.");
    return false;
  };
}
} // namespace

class ProfileStorageTest final : public QObject {
  Q_OBJECT
private slots:
  void realMountedEncryptedProfileControllerAcceptance() {
    const auto socket = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_SOCKET");
    const auto ciphertextRoot =
        qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_CIPHERTEXT_ROOT");
    const auto runtimeDir = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_RUNTIME_DIR");
    const auto profileRoot = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_MOUNT_ROOT");
    const auto mountId = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_MOUNT_ID");
    const auto decoyRoot = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_DECOY_ROOT");
    const auto fixture = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_STICKER_FIXTURE");
    const auto action = qEnvironmentVariable("WHATSFEATHER_TEST_PROFILE_ACTION");
    if (socket.isEmpty() && ciphertextRoot.isEmpty() && runtimeDir.isEmpty() &&
        profileRoot.isEmpty() && mountId.isEmpty() && decoyRoot.isEmpty() &&
        fixture.isEmpty() && action.isEmpty())
      QSKIP("Set explicit WHATSFEATHER_TEST_PROFILE_* values from the opt-in real-FUSE harness.");
    QVERIFY2(!socket.isEmpty() && !ciphertextRoot.isEmpty() &&
                 !runtimeDir.isEmpty() && !profileRoot.isEmpty() &&
                 !mountId.isEmpty() && !decoyRoot.isEmpty() &&
                 !fixture.isEmpty() && !action.isEmpty(),
             "partial real-FUSE opt-in arguments are invalid and must not be skipped");

    QCOMPARE(QFileInfo(socket).absolutePath(), runtimeDir);
    QVERIFY(QFileInfo(ciphertextRoot).isDir());
    QVERIFY(QFileInfo(runtimeDir).isDir());
    QVERIFY(QFileInfo(profileRoot).isDir());
    QVERIFY(QFileInfo(decoyRoot).isDir());
    QVERIFY(QFileInfo(fixture).isFile());
    QVERIFY(QFileInfo(profileRoot).absolutePath() == runtimeDir);
    qInfo() << "[native-mount-namespace]"
            << QFileInfo(QStringLiteral("/proc/self/ns/mnt")).symLinkTarget()
            << "profile-present=" << QFileInfo(profileRoot).isDir();
    QFile childMountInfo(QStringLiteral("/proc/self/mountinfo"));
    QVERIFY(childMountInfo.open(QIODevice::ReadOnly | QIODevice::Text));
    while (true) {
      const auto line = childMountInfo.readLine();
      if (line.isEmpty())
        break;
      if (line.contains(profileRoot.toLocal8Bit()) ||
          line.startsWith(mountId.toLocal8Bit() + ' '))
        qInfo().noquote() << "[native-mountinfo]" << QString::fromLocal8Bit(line).trimmed();
    }

    {
      Controller controller;
      controller.start(socket, ciphertextRoot, true);
      QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 5000);
      QTRY_VERIFY_WITH_TIMEOUT(
          controller.encryptedProfileViewReady() ||
              controller.expressionStoreError() !=
                  QStringLiteral("Waiting for the encrypted profile view."),
          5000);
      QVERIFY2(controller.encryptedProfileViewReady(),
               qPrintable(controller.expressionStoreError()));
      QTRY_VERIFY_WITH_TIMEOUT(controller.capabilitiesReady(), 5000);
      QVERIFY(!controller.demo());
      QCOMPARE(controller.connectionState(), QStringLiteral("disconnected"));

      controller.selectChat(QStringLiteral("synthetic-chat"));
      QTRY_COMPARE_WITH_TIMEOUT(controller.selectedId(),
                                QStringLiteral("synthetic-chat"), 5000);

      if (action == QStringLiteral("write")) {
        QVERIFY2(controller.recordRecentEmoji(QStringLiteral("🧪")),
                 qPrintable(controller.expressionStoreError()));
        QVERIFY2(controller.toggleFavoriteEmoji(QStringLiteral("🛡️")),
                 qPrintable(controller.expressionStoreError()));
        controller.stageAttachmentAs(QUrl::fromLocalFile(fixture),
                                     QStringLiteral("sticker"));
        QTRY_COMPARE_WITH_TIMEOUT(
            controller.attachment().value(QStringLiteral("state")).toString(),
            QStringLiteral("ready"), 10000);
        const auto preparedPath = controller.attachment()
                                      .value(QStringLiteral("prepared_path")).toString();
        QVERIFY2(preparedPath.startsWith(
                     QDir(profileRoot).filePath(QStringLiteral("media/prepared")) +
                         QDir::separator()),
                 qPrintable(QStringLiteral("prepared sticker escaped the real encrypted mount: %1")
                                .arg(preparedPath)));
        QVERIFY2(controller.addCurrentStickerToPack(
                     QStringLiteral("Native FUSE fixture")),
                 qPrintable(controller.expressionStoreError()));
        QCOMPARE(controller.recentEmojis().value(0), QStringLiteral("🧪"));
        QCOMPARE(controller.favoriteEmojis(),
                 QStringList{QStringLiteral("🛡️")});
        QCOMPARE(controller.localStickerPacks().size(), 1);
        QVERIFY(QFileInfo::exists(QDir(profileRoot).filePath(
            QStringLiteral("local-expressions/preferences.json"))));
        QVERIFY(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
            QStringLiteral("local-expressions/preferences.json"))));
        QVERIFY(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
            QStringLiteral("media"))));
      } else if (action == QStringLiteral("verify")) {
        QCOMPARE(controller.recentEmojis().value(0), QStringLiteral("🧪"));
        QCOMPARE(controller.favoriteEmojis(),
                 QStringList{QStringLiteral("🛡️")});
        const auto packs = controller.localStickerPacks();
        QCOMPARE(packs.size(), 1);
        const auto pack = packs.first().toMap();
        const auto stickers = pack.value(QStringLiteral("stickers")).toList();
        QCOMPARE(stickers.size(), 1);
        const auto sticker = stickers.first().toMap();
        const auto stickerPath = QUrl(sticker.value(QStringLiteral("url")).toString())
                                     .toLocalFile();
        QVERIFY(stickerPath.startsWith(QDir(profileRoot).filePath(
                                           QStringLiteral("local-expressions/sticker-packs")) +
                                       QDir::separator()));
        QVERIFY(QFileInfo(stickerPath).isFile());
        QVERIFY(controller.stageLocalSticker(
            pack.value(QStringLiteral("id")).toString(),
            sticker.value(QStringLiteral("id")).toString()));
        QTRY_COMPARE_WITH_TIMEOUT(
            controller.attachment().value(QStringLiteral("state")).toString(),
            QStringLiteral("ready"), 10000);
        const auto preparedPath = controller.attachment()
                                      .value(QStringLiteral("prepared_path")).toString();
        QVERIFY2(preparedPath.startsWith(
                     QDir(profileRoot).filePath(QStringLiteral("media/prepared")) +
                         QDir::separator()),
                 qPrintable(QStringLiteral("reopened sticker preparation escaped the new mount: %1")
                                .arg(preparedPath)));
      } else {
        QFAIL("The real encrypted Controller test action must be write or verify.");
      }
    }

    if (action == QStringLiteral("write")) {
      const auto rejectReportedView = [&](const QString &socketName,
                                          const QString &reportedRoot,
                                          const QString &reportedMountId) {
        const auto forgedSocket = QDir(runtimeDir).filePath(socketName);
        StorageBackend forgedBackend(forgedSocket);
        forgedBackend.storageView = encryptedStorageView(
            reportedRoot, runtimeDir, profileRoot,
            QStringLiteral("fedcba9876543210fedcba9876543210"),
            reportedMountId);
        Controller forgedController;
        forgedController.start(forgedSocket, reportedRoot, true);
        QTRY_VERIFY_WITH_TIMEOUT(forgedController.connected(), 5000);
        QTRY_COMPARE_WITH_TIMEOUT(forgedController.connectionState(),
                                  QStringLiteral("connected"), 5000);
        QVERIFY2(!forgedController.encryptedProfileViewReady(),
                 "the default verifier accepted an unbound real FUSE storage view");
        QVERIFY(!forgedController.recordRecentEmoji(QStringLiteral("🧭")));
      };
      rejectReportedView(QStringLiteral("forged-source.sock"), decoyRoot,
                         mountId);
      const auto staleMountId = mountId == QStringLiteral("1")
                                    ? QStringLiteral("2")
                                    : QStringLiteral("1");
      rejectReportedView(QStringLiteral("forged-stale-mount.sock"),
                         ciphertextRoot, staleMountId);
      QVERIFY(!QFileInfo::exists(QDir(decoyRoot).filePath(
          QStringLiteral("local-expressions/preferences.json"))));
    }
  }

  void liveExpressionsDoNotWriteBeforeBackendConfirmsAnEncryptedView() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    const auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));

    Controller controller;
    controller.start(backend.storageView.isEmpty()
                         ? QDir(runtimeDir).filePath(QStringLiteral("daemon.sock"))
                         : QString(),
                     ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.connectionState(),
                              QStringLiteral("connected"), 3000);
    QVERIFY(!controller.encryptedProfileViewReady());
    controller.connectLive();
    QVERIFY(!controller.hasCapability(QStringLiteral("privacy.set")));
    QVERIFY(controller.capabilityReason(QStringLiteral("privacy.set"))
                .contains(QStringLiteral("encrypted profile view"), Qt::CaseInsensitive));
    QVERIFY(!controller.performCapabilityAction(
        QStringLiteral("privacy.set"),
        {{QStringLiteral("last_seen"), QStringLiteral("nobody")}}));
    controller.selectChat(QStringLiteral("synthetic-unverified-chat"));
    controller.setDraft(QStringLiteral("Synthetic draft before view verification"));
    controller.search(QStringLiteral("synthetic query before verification"));
    QCoreApplication::processEvents();
    QTest::qWait(250);
    QCOMPARE(backend.connectCalls, 0);
    QCOMPARE(backend.methodCalls.value(QStringLiteral("privacy.set")), 0);
    QCOMPARE(backend.methodCalls.value(QStringLiteral("chat.list")), 0);
    QCOMPARE(backend.methodCalls.value(QStringLiteral("draft.get")), 0);
    QCOMPARE(backend.methodCalls.value(QStringLiteral("draft.set")), 0);
    QVERIFY(controller.selectedId().isEmpty());
    QVERIFY(controller.draft().isEmpty());

    QVERIFY2(!controller.recordRecentEmoji(QStringLiteral("🛡️")),
             "live expression writes must wait for a backend-confirmed decrypted profile view");
    QVERIFY2(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
                 QStringLiteral("local-expressions/preferences.json"))),
             "live expression metadata must never be written into the ciphertext root");
  }

  void encryptedViewInvalidationPurgesReachableStateRejectsStaleRepliesAndReloads() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    const auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    const auto firstMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic-first"));
    const auto secondMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic-second"));
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    QVERIFY(makePrivateDirectory(firstMount));
    QVERIFY(makePrivateDirectory(secondMount));

    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));
    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, firstMount);
    backend.statusAccount = {{QStringLiteral("id"), QStringLiteral("synthetic-account-first")},
                             {QStringLiteral("name"), QStringLiteral("Synthetic private account")}};
    backend.accountResponse = {{QStringLiteral("id"), QStringLiteral("synthetic-account-stale")},
                               {QStringLiteral("name"), QStringLiteral("Stale synthetic account response")}};
    const auto firstChat = QStringLiteral("synthetic-private-chat-first");
    backend.chatRows = QJsonArray{QJsonObject{{QStringLiteral("id"), firstChat},
                                             {QStringLiteral("title"), QStringLiteral("Synthetic private conversation")}}};
    backend.messageRows = QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("synthetic-private-message-first")},
                                                {QStringLiteral("chat_id"), firstChat},
                                                {QStringLiteral("body"), QStringLiteral("Synthetic private message body")}}};
    backend.searchRows = QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("synthetic-search-result")},
                                                {QStringLiteral("chat_id"), firstChat},
                                                {QStringLiteral("body"), QStringLiteral("Synthetic private search result")}}};
    backend.contactRows = QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("synthetic-contact-id")},
                                                 {QStringLiteral("name"), QStringLiteral("Synthetic private contact")}}};
    backend.draftText = QStringLiteral("Synthetic draft from the first view");

    const auto mediaDir = QDir(firstMount).filePath(QStringLiteral("media"));
    const auto preparedDir = QDir(mediaDir).filePath(QStringLiteral("prepared"));
    QVERIFY(makePrivateDirectory(mediaDir));
    QVERIFY(makePrivateDirectory(preparedDir));
    const auto fixture = QFINDTESTDATA("backend/internal/app/testdata/sticker-static.webp");
    QVERIFY2(!fixture.isEmpty(), "synthetic static WebP fixture not found");
    backend.preparedPathOverride = QDir(preparedDir).filePath(QStringLiteral("synthetic-prepared.webp"));
    QVERIFY(QFile::copy(fixture, backend.preparedPathOverride));
    QVERIFY(QFile::setPermissions(backend.preparedPathOverride,
                                  QFileDevice::ReadOwner | QFileDevice::WriteOwner));

    const auto contactPath = QDir(root.path()).filePath(QStringLiteral("synthetic-contact.vcf"));
    QFile contactFile(contactPath);
    QVERIFY(contactFile.open(QIODevice::WriteOnly | QIODevice::Text));
    QVERIFY(contactFile.write("BEGIN:VCARD\nVERSION:3.0\nFN:Synthetic Test Contact\nEND:VCARD\n") > 0);
    contactFile.close();
    QVERIFY(QFile::setPermissions(contactPath, QFileDevice::ReadOwner | QFileDevice::WriteOwner));

    LocalExpressionStore seed(firstMount);
    QString storeError;
    QVERIFY2(seed.load(&storeError), qPrintable(storeError));
    QVERIFY2(seed.recordRecentEmoji(QStringLiteral("🧪"), &storeError), qPrintable(storeError));
    QVERIFY2(seed.toggleFavoriteEmoji(QStringLiteral("🛡️"), &storeError), qPrintable(storeError));
    LocalExpressionStore secondSeed(secondMount);
    QVERIFY2(secondSeed.load(&storeError), qPrintable(storeError));
    QVERIFY2(secondSeed.recordRecentEmoji(QStringLiteral("🦉"), &storeError), qPrintable(storeError));
    QVERIFY2(secondSeed.toggleFavoriteEmoji(QStringLiteral("🧠"), &storeError), qPrintable(storeError));

    Controller controller(nullptr, {}, syntheticMountVerifier());
    controller.start(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")), ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.capabilitiesReady(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.chats()->rowCount(), 1, 3000);
    QCOMPARE(controller.account().value(QStringLiteral("id")).toString(),
             QStringLiteral("synthetic-account-first"));

    controller.selectChat(firstChat);
    QTRY_COMPARE_WITH_TIMEOUT(controller.messages()->rowCount(), 1, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.draft(), backend.draftText, 3000);
    QVERIFY(controller.loadHub(QStringLiteral("chat.info"),
                               {{QStringLiteral("chat_id"), firstChat}}));
    QTRY_VERIFY_WITH_TIMEOUT(!controller.routeData().isEmpty(), 3000);
    controller.findMessages(QStringLiteral("synthetic query"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.searchResults()->rowCount(), 1, 3000);
    controller.searchContacts(QStringLiteral("synthetic"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.contacts()->rowCount(), 1, 3000);
    controller.searchGifs(QStringLiteral("synthetic"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.gifItems().size(), 1, 3000);
    controller.stageAttachmentAs(QUrl::fromLocalFile(fixture), QStringLiteral("photo"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.attachment().value(QStringLiteral("state")).toString(),
                              QStringLiteral("ready"), 3000);
    controller.stageContactFile(QUrl::fromLocalFile(contactPath));
    QTRY_COMPARE_WITH_TIMEOUT(controller.contactCards().size(), 1, 3000);
    QVERIFY(!controller.sendLocation({}));
    QVERIFY(!controller.locationShareError().isEmpty());
    QCOMPARE(controller.recentEmojis(), QStringList{QStringLiteral("🧪")});
    QCOMPARE(controller.favoriteEmojis(), QStringList{QStringLiteral("🛡️")});

    QSignalSpy chatResets(controller.chats(), &QAbstractItemModel::modelReset);
    QSignalSpy messageResets(controller.messages(), &QAbstractItemModel::modelReset);
    QSignalSpy searchResets(controller.searchResults(), &QAbstractItemModel::modelReset);
    QSignalSpy contactResets(controller.contacts(), &QAbstractItemModel::modelReset);
    QSignalSpy accountChanges(&controller, &Controller::accountChanged);
    QSignalSpy selectionChanges(&controller, &Controller::selectionChanged);
    QSignalSpy draftChanges(&controller, &Controller::draftChanged);
    QSignalSpy routeChanges(&controller, &Controller::routeChanged);
    QSignalSpy attachmentChanges(&controller, &Controller::attachmentChanged);
    QSignalSpy contactShareChanges(&controller, &Controller::contactShareChanged);
    QSignalSpy locationShareChanges(&controller, &Controller::locationShareChanged);
    QSignalSpy gifsChanges(&controller, &Controller::gifsChanged);
    QSignalSpy expressionsChanges(&controller, &Controller::expressionsChanged);
    QSignalSpy capabilitiesChanges(&controller, &Controller::capabilitiesChanged);

    backend.heldMethods.insert(QStringLiteral("account.get"));
    backend.statusAccount = {};
    backend.statusRevision = 2;
    const int accountRequestsBeforeHold = backend.methodCalls.value(QStringLiteral("account.get"));
    controller.disconnectLive();
    QTRY_COMPARE_WITH_TIMEOUT(backend.methodCalls.value(QStringLiteral("account.get")),
                              accountRequestsBeforeHold + 1, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(backend.heldReplies.value(QStringLiteral("account.get")).size(),
                              1, 3000);

    const int draftWritesBeforePurge = backend.methodCalls.value(QStringLiteral("draft.set"));
    controller.setDraft(QStringLiteral("Synthetic pending draft that must not be saved"));
    backend.storageView = {};
    backend.statusRevision = 3;
    controller.disconnectLive();
    QTRY_VERIFY_WITH_TIMEOUT(!controller.encryptedProfileViewReady(), 3000);
    QTest::qWait(500);

    QCOMPARE(controller.chats()->rowCount(), 0);
    QCOMPARE(controller.messages()->rowCount(), 0);
    QCOMPARE(controller.searchResults()->rowCount(), 0);
    QCOMPARE(controller.contacts()->rowCount(), 0);
    QVERIFY(controller.account().isEmpty());
    QVERIFY(controller.selectedId().isEmpty());
    QVERIFY(controller.selectedTitle().isEmpty());
    QVERIFY(controller.selectedInitials().isEmpty());
    QVERIFY(controller.draft().isEmpty());
    QVERIFY(controller.findQuery().isEmpty());
    QVERIFY(!controller.searching());
    QVERIFY(controller.routeData().isEmpty());
    QVERIFY(controller.routeError().isEmpty());
    QVERIFY(controller.routeMethod().isEmpty());
    QVERIFY(!controller.routeLoading());
    QVERIFY(controller.attachment().isEmpty());
    QVERIFY(controller.contactCards().isEmpty());
    QVERIFY(controller.contactShareError().isEmpty());
    QVERIFY(!controller.contactPreparing());
    QVERIFY(!controller.contactSending());
    QVERIFY(!controller.contactOutcomeUnknown());
    QVERIFY(controller.locationShareError().isEmpty());
    QVERIFY(!controller.locationSending());
    QVERIFY(!controller.locationOutcomeUnknown());
    QVERIFY(!controller.locationShareAccepted());
    QVERIFY(controller.gifItems().isEmpty());
    QVERIFY(!controller.gifsLoading());
    QVERIFY(controller.gifError().isEmpty());
    QVERIFY(controller.gifProvider().isEmpty());
    QVERIFY(controller.recentEmojis().isEmpty());
    QVERIFY(controller.favoriteEmojis().isEmpty());
    QVERIFY(controller.localStickerPacks().isEmpty());
    QVERIFY(controller.expressionStoreError().contains(QStringLiteral("encrypted profile view"),
                                                        Qt::CaseInsensitive));
    QVERIFY(!controller.contactsLoading());
    QVERIFY(!controller.contactsHasMore());
    QVERIFY(controller.contactsError().isEmpty());
    QVERIFY(!controller.hasMore());
    QVERIFY(!controller.loading());
    QVERIFY(!controller.historyWindow());
    QVERIFY(!controller.sending());
    QVERIFY(controller.qr().isEmpty());
    QVERIFY(controller.notice().isEmpty());
    QCOMPARE(controller.status(), QStringLiteral("Encrypted profile view unavailable."));
    QVERIFY(!controller.communitiesRefreshing());
    QVERIFY(!controller.hasCapability(QStringLiteral("privacy.set")));
    QVERIFY(controller.capabilityReason(QStringLiteral("privacy.set"))
                .contains(QStringLiteral("encrypted profile view"), Qt::CaseInsensitive));
    QCOMPARE(backend.methodCalls.value(QStringLiteral("draft.set")), draftWritesBeforePurge);
    QCOMPARE(backend.connectCalls, 0);
    QVERIFY(chatResets.count() > 0);
    QVERIFY(messageResets.count() > 0);
    QVERIFY(searchResets.count() > 0);
    QVERIFY(contactResets.count() > 0);
    QVERIFY(accountChanges.count() > 0);
    QVERIFY(selectionChanges.count() > 0);
    QVERIFY(draftChanges.count() > 0);
    QVERIFY(routeChanges.count() > 0);
    QVERIFY(attachmentChanges.count() > 0);
    QVERIFY(contactShareChanges.count() > 0);
    QVERIFY(locationShareChanges.count() > 0);
    QVERIFY(gifsChanges.count() > 0);
    QVERIFY(expressionsChanges.count() > 0);
    QVERIFY(capabilitiesChanges.count() > 0);

    using SignalCounts = QList<qsizetype>;
    const SignalCounts resetCounts{chatResets.count(), messageResets.count(),
                                   searchResets.count(), contactResets.count(),
                                   accountChanges.count(), selectionChanges.count(),
                                   draftChanges.count(), routeChanges.count(),
                                   attachmentChanges.count(), contactShareChanges.count(),
                                   locationShareChanges.count(), gifsChanges.count(),
                                   expressionsChanges.count(), capabilitiesChanges.count()};
    backend.releaseHeld(QStringLiteral("account.get"));
    QTest::qWait(50);
    QVERIFY(controller.account().isEmpty());

    backend.statusRevision = 4;
    controller.disconnectLive();
    QTRY_VERIFY_WITH_TIMEOUT(!controller.encryptedProfileViewReady(), 3000);
    const SignalCounts repeatedInvalidationCounts{
        chatResets.count(), messageResets.count(), searchResets.count(), contactResets.count(),
        accountChanges.count(), selectionChanges.count(), draftChanges.count(),
        routeChanges.count(), attachmentChanges.count(), contactShareChanges.count(),
        locationShareChanges.count(), gifsChanges.count(), expressionsChanges.count(),
        capabilitiesChanges.count()};
    QCOMPARE(repeatedInvalidationCounts, resetCounts);

    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, secondMount,
                                               QStringLiteral("fedcba9876543210fedcba9876543210"),
                                               QStringLiteral("43"));
    backend.statusRevision = 5;
    backend.statusAccount = {{QStringLiteral("id"), QStringLiteral("synthetic-account-second")},
                             {QStringLiteral("name"), QStringLiteral("Fresh synthetic account")}};
    const auto secondChat = QStringLiteral("synthetic-private-chat-second");
    backend.chatRows = QJsonArray{QJsonObject{{QStringLiteral("id"), secondChat},
                                             {QStringLiteral("title"), QStringLiteral("Fresh synthetic conversation")}}};
    backend.messageRows = QJsonArray{QJsonObject{{QStringLiteral("id"), QStringLiteral("synthetic-private-message-second")},
                                                {QStringLiteral("chat_id"), secondChat},
                                                {QStringLiteral("body"), QStringLiteral("Fresh synthetic message")}}};
    backend.draftText = QStringLiteral("Fresh draft for the second view");
    backend.heldMethods.remove(QStringLiteral("account.get"));
    controller.disconnectLive();
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.chats()->rowCount(), 1, 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.capabilitiesReady(), 3000);
    QCOMPARE(controller.chats()->row(0).value(QStringLiteral("id")).toString(), secondChat);
    QCOMPARE(controller.account().value(QStringLiteral("id")).toString(),
             QStringLiteral("synthetic-account-second"));
    QVERIFY(controller.hasCapability(QStringLiteral("privacy.set")));
    QCOMPARE(controller.recentEmojis(), QStringList{QStringLiteral("🦉")});
    QCOMPARE(controller.favoriteEmojis(), QStringList{QStringLiteral("🧠")});
    controller.selectChat(secondChat);
    QTRY_COMPARE_WITH_TIMEOUT(controller.messages()->rowCount(), 1, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.draft(), backend.draftText, 3000);
    backend.releaseHeld(QStringLiteral("account.get"));
    QTest::qWait(50);
    QCOMPARE(controller.account().value(QStringLiteral("id")).toString(),
             QStringLiteral("synthetic-account-second"));
    QCOMPARE(backend.connectCalls, 0);

    backend.pause();
    QTRY_VERIFY_WITH_TIMEOUT(!controller.connected(), 3000);
    QCOMPARE(controller.chats()->rowCount(), 0);
    QCOMPARE(controller.messages()->rowCount(), 0);
    QCOMPARE(controller.searchResults()->rowCount(), 0);
    QCOMPARE(controller.contacts()->rowCount(), 0);
    QVERIFY(controller.account().isEmpty());
    QVERIFY(controller.selectedId().isEmpty());
    QVERIFY(controller.selectedTitle().isEmpty());
    QVERIFY(controller.draft().isEmpty());
    QVERIFY(controller.routeData().isEmpty());
    QVERIFY(controller.attachment().isEmpty());
    QVERIFY(controller.contactCards().isEmpty());
    QVERIFY(controller.gifItems().isEmpty());
    QVERIFY(controller.recentEmojis().isEmpty());
    QVERIFY(controller.favoriteEmojis().isEmpty());
    QVERIFY(!controller.hasCapability(QStringLiteral("privacy.set")));

    QVERIFY(backend.resume());
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.capabilitiesReady(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.chats()->rowCount(), 1, 3000);
    QCOMPARE(controller.chats()->row(0).value(QStringLiteral("id")).toString(), secondChat);
    QCOMPARE(controller.account().value(QStringLiteral("id")).toString(),
             QStringLiteral("synthetic-account-second"));
    QVERIFY(controller.hasCapability(QStringLiteral("privacy.set")));
    QCOMPARE(controller.recentEmojis(), QStringList{QStringLiteral("🦉")});
    QCOMPARE(controller.favoriteEmojis(), QStringList{QStringLiteral("🧠")});
    controller.selectChat(secondChat);
    QTRY_COMPARE_WITH_TIMEOUT(controller.messages()->rowCount(), 1, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.draft(), backend.draftText, 3000);
  }

  void liveStickerMediaResolvesFromTheConfirmedMountedProfileRoot() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    const auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    const auto profileMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic"));
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    QVERIFY(makePrivateDirectory(profileMount));
    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));
    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, profileMount);

    const auto fixture = QFINDTESTDATA("backend/internal/app/testdata/sticker-static.webp");
    QVERIFY2(!fixture.isEmpty(), "synthetic static WebP fixture not found");
    const auto mediaDir = QDir(profileMount).filePath(QStringLiteral("media"));
    const auto preparedDir = QDir(mediaDir).filePath(QStringLiteral("prepared"));
    QVERIFY(makePrivateDirectory(mediaDir));
    QVERIFY(makePrivateDirectory(preparedDir));
    backend.preparedPathOverride = QDir(preparedDir).filePath(QStringLiteral("synthetic-sticker.webp"));
    QVERIFY(QFile::copy(fixture, backend.preparedPathOverride));
    QVERIFY(QFile::setPermissions(backend.preparedPathOverride,
                                  QFileDevice::ReadOwner | QFileDevice::WriteOwner));
    LocalExpressionStore seed(profileMount);
    QString storeError;
    QVERIFY2(seed.load(&storeError), qPrintable(storeError));
    QVERIFY2(seed.addStickerToPack(QStringLiteral("Synthetic mounted pack"), fixture,
                                   &storeError),
             qPrintable(storeError));
    const auto pack = seed.stickerPacks().first().toMap();
    const auto packId = pack.value(QStringLiteral("id")).toString();
    const auto stickerId = pack.value(QStringLiteral("stickers")).toList()
                               .first().toMap().value(QStringLiteral("id")).toString();

    Controller controller(nullptr, {}, syntheticMountVerifier());
    controller.start(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")),
                     ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.capabilitiesReady(), 3000);
    controller.selectChat(QStringLiteral("fixture:encrypted-profile"));
    QTRY_COMPARE_WITH_TIMEOUT(controller.selectedId(),
                              QStringLiteral("fixture:encrypted-profile"), 3000);

    QVERIFY2(controller.stageLocalSticker(packId, stickerId),
             "sticker packs must load from the daemon-confirmed decrypted profile view");
    QTRY_COMPARE_WITH_TIMEOUT(backend.prepareCount, 1, 3000);
    QVERIFY2(backend.preparedPath.startsWith(preparedDir + QDir::separator()),
             qPrintable(QStringLiteral("sticker media path escaped the mounted profile: %1")
                            .arg(backend.preparedPath)));
    QCOMPARE(controller.attachment().value(QStringLiteral("prepared_path")).toString(),
             backend.preparedPath);
    QVERIFY(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
        QStringLiteral("local-expressions/preferences.json"))));
  }

  void reconnectReloadsExpressionStateFromTheNewMountedProfile() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    const auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    const auto firstMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-first"));
    const auto secondMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-second"));
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    QVERIFY(makePrivateDirectory(firstMount));
    QVERIFY(makePrivateDirectory(secondMount));

    LocalExpressionStore firstSeed(firstMount);
    LocalExpressionStore secondSeed(secondMount);
    QString storeError;
    QVERIFY(firstSeed.load(&storeError));
    QVERIFY(secondSeed.load(&storeError));
    QVERIFY(firstSeed.toggleFavoriteEmoji(QStringLiteral("🦉"), &storeError));
    QVERIFY(secondSeed.toggleFavoriteEmoji(QStringLiteral("🧠"), &storeError));

    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));
    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, firstMount);
    Controller controller(nullptr, {}, syntheticMountVerifier());
    controller.start(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")),
                     ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    QCOMPARE(controller.favoriteEmojis(), QStringList{QStringLiteral("🦉")});

    backend.pause();
    QTRY_VERIFY_WITH_TIMEOUT(!controller.connected(), 3000);
    QVERIFY(!controller.encryptedProfileViewReady());
    QVERIFY(!controller.recordRecentEmoji(QStringLiteral("🦊")));
    backend.storageView = encryptedStorageView(
        ciphertextRoot, runtimeDir, secondMount,
        QStringLiteral("fedcba9876543210fedcba9876543210"),
        QStringLiteral("43"));
    QVERIFY(backend.resume());
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 5000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 5000);
    QCOMPARE(controller.favoriteEmojis(), QStringList{QStringLiteral("🧠")});
    QVERIFY(controller.recordRecentEmoji(QStringLiteral("🦊")));
    QVERIFY(QFileInfo::exists(QDir(secondMount).filePath(
        QStringLiteral("local-expressions/preferences.json"))));
    QVERIFY(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
        QStringLiteral("local-expressions/preferences.json"))));
  }

  void liveRejectsUntrustedStorageView_data() {
    QTest::addColumn<QString>("invalidCase");
    QTest::newRow("malformed-version") << QStringLiteral("malformed");
    QTest::newRow("foreign-profile-root") << QStringLiteral("foreign-root");
    QTest::newRow("stale-mount-identity") << QStringLiteral("stale-mount");
    QTest::newRow("symlink-profile-root") << QStringLiteral("symlink-root");
    QTest::newRow("ciphertext-runtime-overlap")
        << QStringLiteral("ciphertext-runtime-overlap");
  }

  void liveRejectsUntrustedStorageView() {
    QFETCH(QString, invalidCase);
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    const auto profileMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic"));
    const auto foreignRoot = QDir(root.path()).filePath(QStringLiteral("foreign-profile"));
    const auto symlinkRoot = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-symlink"));
    if (invalidCase == QStringLiteral("ciphertext-runtime-overlap"))
      ciphertextRoot = runtimeDir;
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    QVERIFY(makePrivateDirectory(profileMount));
    QVERIFY(makePrivateDirectory(foreignRoot));
    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));
    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, profileMount);
    if (invalidCase == QStringLiteral("malformed"))
      backend.storageView.insert(QStringLiteral("version"), 2);
    else if (invalidCase == QStringLiteral("foreign-root"))
      backend.storageView.insert(QStringLiteral("profile_root"), foreignRoot);
    else if (invalidCase == QStringLiteral("stale-mount"))
      backend.storageView.insert(QStringLiteral("mount_id"), QStringLiteral("41"));
    else if (invalidCase == QStringLiteral("symlink-root")) {
      QVERIFY(QFile::link(foreignRoot, symlinkRoot));
      QVERIFY(QFileInfo(symlinkRoot).isSymLink());
      backend.storageView.insert(QStringLiteral("profile_root"), symlinkRoot);
    }

    Controller controller(nullptr, {}, syntheticMountVerifier());
    controller.start(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")),
                     ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.connectionState(),
                              QStringLiteral("connected"), 3000);
    QVERIFY(!controller.encryptedProfileViewReady());
    QVERIFY(!controller.recordRecentEmoji(QStringLiteral("🧭")));
    QVERIFY(!QFileInfo::exists(QDir(ciphertextRoot).filePath(
        QStringLiteral("local-expressions/preferences.json"))));
    QVERIFY(!QFileInfo::exists(QDir(profileMount).filePath(
        QStringLiteral("local-expressions/preferences.json"))));
    controller.connectLive();
    QCoreApplication::processEvents();
    QCOMPARE(backend.connectCalls, 0);
  }

  void liveAttachmentRejectsPreparedMediaInCiphertextRoot() {
    QTemporaryDir root;
    QVERIFY(root.isValid());
    const auto runtimeDir = QDir(root.path()).filePath(QStringLiteral("runtime"));
    const auto ciphertextRoot = QDir(root.path()).filePath(QStringLiteral("ciphertext"));
    const auto profileMount = QDir(runtimeDir).filePath(QStringLiteral("wf-pv-synthetic"));
    QVERIFY(makePrivateDirectory(runtimeDir));
    QVERIFY(makePrivateDirectory(ciphertextRoot));
    QVERIFY(makePrivateDirectory(profileMount));
    StorageBackend backend(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")));
    backend.storageView = encryptedStorageView(ciphertextRoot, runtimeDir, profileMount);
    const auto wrongMediaDir = QDir(ciphertextRoot).filePath(QStringLiteral("media/prepared"));
    QVERIFY(makePrivateDirectory(QDir(ciphertextRoot).filePath(QStringLiteral("media"))));
    QVERIFY(makePrivateDirectory(wrongMediaDir));
    const auto fixture = QFINDTESTDATA("backend/internal/app/testdata/sticker-static.webp");
    QVERIFY(!fixture.isEmpty());
    backend.preparedPathOverride = QDir(wrongMediaDir).filePath(QStringLiteral("wrong.webp"));
    QVERIFY(QFile::copy(fixture, backend.preparedPathOverride));
    QVERIFY(QFile::setPermissions(backend.preparedPathOverride,
                                  QFileDevice::ReadOwner | QFileDevice::WriteOwner));

    Controller controller(nullptr, {}, syntheticMountVerifier());
    controller.start(QDir(runtimeDir).filePath(QStringLiteral("daemon.sock")),
                     ciphertextRoot, true);
    QTRY_VERIFY_WITH_TIMEOUT(controller.connected(), 3000);
    QTRY_VERIFY_WITH_TIMEOUT(controller.encryptedProfileViewReady(), 3000);
    controller.selectChat(QStringLiteral("fixture:encrypted-profile"));
    controller.stageAttachmentAs(QUrl::fromLocalFile(fixture),
                                  QStringLiteral("photo"));
    QTRY_COMPARE_WITH_TIMEOUT(backend.prepareCount, 1, 3000);
    QTRY_COMPARE_WITH_TIMEOUT(controller.attachment().value(QStringLiteral("state")).toString(),
                              QStringLiteral("error"), 3000);
    QCOMPARE(controller.attachment().value(QStringLiteral("prepared_path")).toString(),
             QString());
    QCOMPARE(backend.preparedPath, backend.preparedPathOverride);
  }
};

QTEST_GUILESS_MAIN(ProfileStorageTest)
#include "test_profile_storage.moc"
