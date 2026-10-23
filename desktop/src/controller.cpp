#include "controller.h"
#include <QCoreApplication>
#include <QDateTime>
#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QJsonArray>
#include <QJsonDocument>
#include <QSettings>
#include <QStandardPaths>
#include <QUuid>
#include <utility>
#include <sys/stat.h>

namespace {
constexpr qint64 MaxFrame = 256 * 1024;
constexpr qint64 ReadChunk = 4096;

QString capabilityFeatureId(const QString &method) {
  if (method == "attachment.album.prepare" ||
      method == "attachment.album.release" || method == "message.album.send")
    return QStringLiteral("multi-attachment-albums");
  if (method == "message.location.send")
    return QStringLiteral("location-messages");
  if (method == "contact.prepare" || method == "message.contact.send")
    return QStringLiteral("contact-cards");
  if (method == "contact.list" || method == "chat.open")
    return QStringLiteral("chat-creation-roster-management");
  if (method == "media.list")
    return QStringLiteral("media-library-local");
  if (method == "media.library")
    return QStringLiteral("shared-media-library");
  if (method == "chat.info")
    return QStringLiteral("chat-info-local");
  if (method == "community.list" || method == "community.refresh")
    return QStringLiteral("community-folders");
  if (method == "message.react")
    return QStringLiteral("reactions");
  if (method == "message.reply")
    return QStringLiteral("reply");
  if (method == "message.edit")
    return QStringLiteral("edit-message");
  if (method == "message.delete")
    return QStringLiteral("delete-message");
  if (method == "message.forward")
    return QStringLiteral("forward-message");
  if (method == "message.star")
    return QStringLiteral("star-message");
  if (method == "message.pin")
    return QStringLiteral("message-pin");
  if (method == "chat.update")
    return QStringLiteral("chat-organization");
  if (method == "account.get")
    return QStringLiteral("account-profile-view");
  if (method == "account.update")
    return QStringLiteral("profile-edit");
  if (method == "privacy.set")
    return QStringLiteral("privacy-security-settings");
  if (method == "auth.disconnect")
    return QStringLiteral("local-disconnect");
  if (method == "gif.search" || method == "gif.providers" ||
      method == "gif.prepare")
    return QStringLiteral("gif-search");
  if (method == "status.list")
    return QStringLiteral("whatsapp-status-stories");
  if (method == "channel.list" || method == "channel.info" ||
      method == "channel.history")
    return QStringLiteral("channels");
  if (method == "group.list" || method == "group.info" ||
      method == "group.create" || method == "group.join" ||
      method == "group.leave" || method == "group.update" ||
      method == "group.participants")
    return QStringLiteral("groups-management");
  return method;
}
} // namespace

Controller::Controller(QObject *p,
                       VoiceNoteRecorder::CaptureFactory voiceCaptureFactory)
    : QObject(p), m_voiceRecorder(std::move(voiceCaptureFactory)) {
  m_socket.setReadBufferSize(MaxFrame);
  m_findTimer.setInterval(180);
  m_findTimer.setSingleShot(true);
  connect(&m_findTimer, &QTimer::timeout, this, &Controller::runFind);
  m_poll.setInterval(2000);
  connect(&m_poll, &QTimer::timeout, this, &Controller::statusPoll);
  m_retry.setInterval(200);
  connect(&m_retry, &QTimer::timeout, this, [this] {
    if (++m_retries > 50) {
      m_retry.stop();
      m_status = "Backend unavailable";
      m_error = "The local backend did not start. Check its executable and "
                "profile directory.";
      emit stateChanged();
      return;
    }
    if (m_socket.state() == QLocalSocket::UnconnectedState)
      m_socket.connectToServer(m_path);
  });
  connect(&m_socket, &QLocalSocket::connected, this, [this] {
    m_retry.stop();
    m_retries = 0;
    m_revision = -1;
    m_accountRevision = -2;
    if (m_deferredExpressionLoad) {
      m_deferredExpressionLoad = false;
      QString expressionError;
      m_expressionStoreError.clear();
      if (!m_expressionStore.load(&expressionError))
        m_expressionStoreError = expressionError;
      emit expressionsChanged();
    }
    m_poll.start();
    refreshCapabilities();
    statusPoll();
  });
  connect(&m_socket, &QLocalSocket::readyRead, this, &Controller::read);
  connect(&m_socket, &QLocalSocket::disconnected, this, [this] {
    m_buffer.clear();
    m_poll.stop();
    m_statusPending = false;
    m_status = "Local backend disconnected";
    m_connectionState = "disconnected";
    m_accountRevision = -2;
    m_capabilities.clear();
    m_capabilityMethods.clear();
    m_capabilitiesReady = false;
    m_capabilitiesPending = false;
    m_contacts.replace({});
    ++m_contactSerial;
    m_contactTimer.stop();
    m_contactCursor.clear();
    m_contactsHasMore = false;
    m_contactsLoading = false;
    m_contactsError.clear();
    emit contactsChanged();
    emit capabilitiesChanged();
    auto pending = std::move(m_pending);
    m_pending.clear();
    for (auto it = pending.begin(); it != pending.end(); ++it)
      it->callback({}, {{"code", "disconnected"},
                        {"message", "Backend disconnected. Check message "
                                    "history before retrying a send."}});
    m_retry.start();
    emit stateChanged();
  });
  m_timeout.setInterval(500);
  connect(&m_timeout, &QTimer::timeout, this, [this] {
    auto now = QDateTime::currentMSecsSinceEpoch();
    QStringList expired;
    for (auto it = m_pending.begin(); it != m_pending.end(); ++it)
      if (now - it->started > 30000)
        expired << it.key();
    for (const auto &id : expired) {
      auto cb = m_pending.take(id).callback;
      cb({}, {{"code", "timeout"},
              {"message",
               "Operation timed out. Check history before retrying a send."}});
    }
  });
  m_timeout.start();
  m_draftTimer.setInterval(450);
  m_draftTimer.setSingleShot(true);
  connect(&m_draftTimer, &QTimer::timeout, this, &Controller::persistDraft);
  m_searchTimer.setInterval(180);
  m_searchTimer.setSingleShot(true);
  connect(&m_searchTimer, &QTimer::timeout, this, &Controller::refreshChats);
  m_contactTimer.setInterval(180);
  m_contactTimer.setSingleShot(true);
  connect(&m_contactTimer, &QTimer::timeout, this, [this] {
    const auto query = m_contactQuery.trimmed().left(100);
    const auto cursor = m_contactCursor;
    const int serial = m_contactSerial;
    if (!hasCapability("contact.list")) {
      m_contactsLoading = false;
      m_contactsHasMore = false;
      m_contacts.replace({});
      emit contactsChanged();
      return;
    }
    request("contact.list", {{"query", query}, {"limit", 50}, {"cursor", cursor}},
            [this, query, cursor, serial](QJsonValue value, QJsonObject error) {
      if (serial != m_contactSerial ||
          query != m_contactQuery.trimmed().left(100))
        return;
      m_contactsLoading = false;
      if (!error.isEmpty()) {
        m_contactsError = error.value("message").toString();
        if (cursor.isEmpty()) {
          m_contacts.replace({});
          m_contactCursor.clear();
          m_contactsHasMore = false;
        } else {
          m_contactsHasMore = true;
        }
      } else {
        const auto response = value.toObject();
        const auto page = response.value("contacts").toArray().toVariantList();
        if (cursor.isEmpty()) {
          m_contacts.replace(page);
        } else {
          QVariantList merged;
          QSet<QString> seen;
          for (int i = 0; i < m_contacts.rowCount(); ++i) {
            const auto row = m_contacts.row(i);
            const auto jid = row.value("jid").toString();
            if (!jid.isEmpty() && !seen.contains(jid)) {
              seen.insert(jid);
              merged.append(row);
            }
          }
          for (const auto &item : page) {
            const auto row = item.toMap();
            const auto jid = row.value("jid").toString();
            if (!jid.isEmpty() && !seen.contains(jid)) {
              seen.insert(jid);
              merged.append(row);
            }
          }
          m_contacts.replace(merged);
        }
        const auto nextCursor = response.value("next_cursor").toString();
        const bool hasMore = response.value("has_more").toBool();
        m_contactsError.clear();
        if (hasMore && (nextCursor.isEmpty() || nextCursor == cursor)) {
          m_contactCursor = cursor;
          m_contactsHasMore = false;
          m_contactsError = "The contact roster returned an invalid continuation cursor.";
        } else {
          m_contactCursor = nextCursor;
          m_contactsHasMore = hasMore;
        }
        if (m_contacts.rowCount() >= 200 && m_contactsHasMore) {
          m_contactsHasMore = false;
          m_contactsError = "Showing the first 200 contacts. Refine your search to find more.";
        }
      }
      emit contactsChanged();
    });
  });
  connect(&m_process, &QProcess::errorOccurred, this,
          [this](QProcess::ProcessError) {
            m_error = "Cannot start the local backend executable.";
            emit stateChanged();
          });
  connect(&m_voiceRecorder, &VoiceNoteRecorder::stateChanged, this,
          [this] { emit stateChanged(); });
  connect(&m_voiceRecorder, &VoiceNoteRecorder::failed, this,
          [this](const QString &message) {
            m_error = message;
            emit stateChanged();
          });
  connect(&m_voiceRecorder, &VoiceNoteRecorder::ready, this,
          [this](const QString &path) {
            if (m_id.isEmpty() || !connected() || !m_attachment.isEmpty() ||
                m_voiceStagePending) {
              m_error = "Voice note could not be staged for the current conversation.";
              m_voiceRecorder.discard();
              emit stateChanged();
              return;
            }
            const int expectedSerial = m_attachmentSerial + 1;
            m_voiceStagePending = true;
            m_voiceAttachmentSerial = expectedSerial;
            m_voiceDiscardPending = false;
            stageAttachmentAs(QUrl::fromLocalFile(path),
                              QStringLiteral("voice-note"));
            if (m_attachmentSerial != expectedSerial &&
                m_voiceStagePending && m_voiceAttachmentSerial == expectedSerial) {
              m_voiceStagePending = false;
              m_voiceAttachmentSerial = -1;
              m_voiceRecorder.discard();
            }
          });
}
Controller::~Controller() {
  m_voiceRecorder.cancel();
  m_poll.stop();
  m_retry.stop();
  m_timeout.stop();
  m_draftTimer.stop();
  persistDraft();
  m_socket.flush();
  m_socket.disconnect(this);
  m_socket.abort();
  if (m_process.state() != QProcess::NotRunning) {
    m_process.terminate();
    if (!m_process.waitForFinished(2500)) {
      m_process.kill();
      m_process.waitForFinished(1000);
    }
  }
}
void Controller::start(const QString &socket, const QString &dataDir,
                       bool live, const QString &liveTestScopePath) {
  if (!liveTestScopePath.isEmpty() && (!live || !socket.isEmpty())) {
    m_error = "Live-test scope requires live mode and the managed local backend.";
    emit stateChanged();
    return;
  }
  if (liveTestScopePath.isEmpty()) {
    const QFileInfo marker(QDir(dataDir).filePath(QStringLiteral(".live-test-scope")));
    if (marker.exists() || marker.isSymLink()) {
      m_error = "Scoped profile cannot be opened in ordinary mode.";
      emit stateChanged();
      return;
    }
  }
  m_expressionStore.setProfileRoot(dataDir);
  m_dataDir = QFileInfo(dataDir).absoluteFilePath();
  m_expressionStoreError.clear();
  m_deferredExpressionLoad = !liveTestScopePath.isEmpty();
  if (!m_deferredExpressionLoad) {
    QString expressionError;
    if (!m_expressionStore.load(&expressionError))
      m_expressionStoreError = expressionError;
    emit expressionsChanged();
  }
  m_demo = !live;
  if (!socket.isEmpty()) {
    m_path = socket;
  } else {
    QString runtime =
        QStandardPaths::writableLocation(QStandardPaths::RuntimeLocation) +
        "/whatsfeather";
    if (!QDir().mkpath(runtime)) {
      m_error = "Cannot create runtime directory";
      emit stateChanged();
      return;
    }
    QFile::setPermissions(runtime, QFileDevice::ReadOwner |
                                       QFileDevice::WriteOwner |
                                       QFileDevice::ExeOwner);
    m_path = runtime + "/ui-" +
             QUuid::createUuid().toString(QUuid::Id128).left(8) + ".sock";
    QString backend = qEnvironmentVariable("WHATSFEATHER_BACKEND");
    if (backend.isEmpty())
      backend = QCoreApplication::applicationDirPath() + "/whatsfeatherd";
    m_process.setProgram(backend);
    QStringList arguments{live ? "--live" : "--demo", "--socket", m_path,
                          "--data-dir", dataDir};
    if (!liveTestScopePath.isEmpty())
      arguments << "--live-test-scope" << liveTestScopePath;
    m_process.setArguments(arguments);
    m_process.start();
  }
  m_retry.start();
  emit stateChanged();
}
void Controller::request(const QString &method, const QJsonObject &params,
                         Callback cb) {
  if (!connected()) {
    cb({}, {{"code", "disconnected"},
            {"message", "Local backend is not connected"}});
    return;
  }
  if (m_pending.size() >= 32) {
    cb({}, {{"code", "busy"}, {"message", "Too many pending operations"}});
    return;
  }
  auto id = QString::number(++m_next);
  m_pending.insert(id, {std::move(cb), QDateTime::currentMSecsSinceEpoch()});
  auto data = QJsonDocument(QJsonObject{{"v", 1},
                                        {"id", id},
                                        {"method", method},
                                        {"params", params}})
                  .toJson(QJsonDocument::Compact) +
              '\n';
  if (m_socket.write(data) >= 0)
    emit rpcRequestSent(method);
}
void Controller::read() {
  while (m_socket.state() == QLocalSocket::ConnectedState) {
    while (true) {
      const auto n = m_buffer.indexOf('\n');
      if (n < 0)
        break;
      if (n + 1 > MaxFrame) {
        m_socket.abort();
        m_error = "Backend frame exceeded limit";
        emit stateChanged();
        return;
      }
      const auto line = m_buffer.left(n);
      m_buffer.remove(0, n + 1);
      QJsonParseError e;
      const auto doc = QJsonDocument::fromJson(line, &e);
      if (e.error != QJsonParseError::NoError || !doc.isObject()) {
        m_error = "Invalid backend response";
        emit stateChanged();
        continue;
      }
      const auto o = doc.object();
      const auto id = o.value("id").toString();
      if (m_pending.contains(id)) {
        auto cb = m_pending.take(id).callback;
        cb(o.value("result"), o.value("error").toObject());
      }
    }

    if (m_buffer.size() >= MaxFrame) {
      m_socket.abort();
      m_error = "Backend frame exceeded limit";
      emit stateChanged();
      return;
    }
    if (m_socket.bytesAvailable() <= 0)
      return;

    const auto room = MaxFrame - m_buffer.size();
    const auto chunk = m_socket.read(qMin(ReadChunk, room));
    if (chunk.isEmpty())
      return;
    m_buffer += chunk;
  }
}
void Controller::statusPoll() {
  if (m_statusPending)
    return;
  m_statusPending = true;
  request("app.status", {}, [this](QJsonValue value, QJsonObject e) {
    m_statusPending = false;
    if (!e.isEmpty()) {
      m_error = e.value("message").toString();
      emit stateChanged();
      return;
    }
    auto o = value.toObject();
    const auto previousConnectionState = m_connectionState;
    m_demo = o.value("mode").toString() == "demo";
    m_connectionState = o.value("state").toString();
    if (m_connectionState.isEmpty())
      m_connectionState = m_demo ? "demo" : "unknown";
    m_status = o.value("message").toString();
    m_qr = o.value("qr_png").toString();
    const auto statusAccount = o.value("account").toObject().toVariantMap();
    if (previousConnectionState != m_connectionState)
      m_accountRevision = -2;
    auto rev = o.value("revision").toDouble();
    if (rev != m_revision) {
      m_revision = rev;
      refreshChats();
      if (!m_id.isEmpty() && !m_history)
        fetchMessages(false);
      refreshAccount(statusAccount);
    } else if (!statusAccount.isEmpty() && statusAccount != m_account) {
      m_account = statusAccount;
      emit accountChanged();
    } else if (m_account.isEmpty() || m_accountRevision != m_revision) {
      refreshAccount();
    }
    pollNotifications();
    emit stateChanged();
  });
}
void Controller::refreshAccount(const QVariantMap &fromStatus) {
  if (!fromStatus.isEmpty()) {
    if (fromStatus != m_account) {
      m_account = fromStatus;
      emit accountChanged();
    }
    m_accountRevision = m_revision;
    return;
  }
  if (m_accountPending || m_accountRevision == m_revision || !connected())
    return;
  m_accountPending = true;
  const auto revision = m_revision;
  request("account.get", {}, [this, revision](QJsonValue value, QJsonObject e) {
    m_accountPending = false;
    m_accountRevision = revision;
    // Older test fakes do not know account.get. The rest of the UI remains
    // usable and an unavailable account is more truthful than guessed data.
    if (!e.isEmpty()) {
      const QVariantMap unavailable{{"available", false},
                                    {"state", m_connectionState}};
      if (unavailable != m_account) {
        m_account = unavailable;
        emit accountChanged();
      }
      // Old test fakes may not know account.get; do not turn that compatibility
      // path into a visible backend error or an invented identity.
      return;
    }
    const auto account = value.toObject().toVariantMap();
    if (account != m_account) {
      m_account = account;
      emit accountChanged();
    }
  });
}
void Controller::refreshCapabilities() {
  if (!connected() || m_capabilitiesPending || m_capabilitiesReady)
    return;
  m_capabilitiesPending = true;
  request("app.capabilities", {}, [this](QJsonValue value, QJsonObject error) {
    m_capabilitiesPending = false;
    m_capabilitiesReady = true;
    m_capabilities.clear();
    m_capabilityMethods.clear();
    if (error.isEmpty()) {
      const auto object = value.toObject();
      if (object.value("version").toInt() == 1) {
        m_capabilities = object.toVariantMap();
        for (const auto &entry : object.value("methods").toArray()) {
          const auto method = entry.toString();
          if (!method.isEmpty() && !m_capabilityMethods.contains(method))
            m_capabilityMethods.append(method);
        }
      }
    }
    emit capabilitiesChanged();
  });
}
bool Controller::hasCapability(const QString &method) const {
  if (!m_capabilitiesReady || !m_capabilityMethods.contains(method))
    return false;
  const auto featureId = capabilityFeatureId(method);
  const auto features = m_capabilities.value("features").toMap();
  if (!features.contains(featureId))
    return true;
  const auto feature = features.value(featureId).toMap();
  return feature.value("available").toBool();
}
QString Controller::capabilityReason(const QString &method) const {
  if (!m_capabilitiesReady)
    return QStringLiteral("Checking backend support.");
  const auto feature = m_capabilities.value("features").toMap()
                           .value(capabilityFeatureId(method)).toMap();
  const auto reason = feature.value("reason").toString().trimmed();
  if (!reason.isEmpty())
    return reason;
  if (m_capabilityMethods.contains(method) &&
      feature.value("available", true).toBool())
    return QStringLiteral("Available in this backend.");
  return QStringLiteral("This backend does not advertise this operation.");
}
void Controller::refreshChats() {
  auto query = m_query;
  request("chat.list", {{"query", query}},
          [this, query](QJsonValue value, QJsonObject e) {
            if (query != m_query)
              return;
            if (!e.isEmpty()) {
              m_error = e.value("message").toString();
              emit stateChanged();
              return;
            }
            m_chats.replace(
                value.toObject().value("chats").toArray().toVariantList());
          });
}
void Controller::search(const QString &q) {
  m_query = q;
  m_searchTimer.start();
}
void Controller::selectChat(const QString &id) {
  if (id == m_id || m_contactSending || m_locationSending ||
      (m_sending && m_attachment.value("kind").toString() ==
                        QStringLiteral("album")))
    return;
  persistDraft();
  m_draftTimer.stop();
  ++m_generation;
  if (m_pendingMediaChat != id) {
    m_pendingMediaChat.clear();
    m_pendingMediaMessage.clear();
  }
  if (m_routeMethod == QStringLiteral("chat.info") ||
      m_routeMethod == QStringLiteral("media.list") ||
      m_routeMethod == QStringLiteral("media.library")) {
    ++m_routeSerial;
    m_routeData.clear();
    m_routeError.clear();
    m_routeLoading = false;
    emit routeChanged();
  }
  clearContactShare();
  clearLocationShare();
  clearAttachment();
  m_findTimer.stop();
  m_findQuery.clear();
  m_searchResults.replace({});
  m_searching = false;
  emit findChanged();
  m_draftVersion = 0;
  m_id = id;
  m_title = id;
  m_initials = "";
  int row = m_chats.indexOf(id);
  if (row >= 0) {
    auto c = m_chats.row(row);
    m_title = c.value("title").toString();
    if (m_title.contains('@'))
      m_title = c.value("sender_name").toString();
    if (m_title.isEmpty() || m_title.contains('@'))
      m_title = "Unknown contact";
    m_initials = c.value("initials").toString();
  }
  m_draft.clear();
  m_history = false;
  m_hasMore = false;
  m_loading = false;
  m_messages.replace({});
  emit selectionChanged();
  emit pollVotesChanged();
  emit listRepliesChanged();
  emit draftChanged();
  emit messagesChanged();
  if (id.isEmpty())
    return;
  int gen = m_generation;
  int version = m_draftVersion;
  request("draft.get", {{"chat_id", id}},
          [this, gen, version](QJsonValue v, QJsonObject e) {
            if (gen != m_generation || version != m_draftVersion)
              return;
            if (!e.isEmpty()) {
              m_error = e.value("message").toString();
              emit stateChanged();
              return;
            }
            m_draft = v.toObject().value("text").toString();
            emit draftChanged();
          });
  fetchMessages(true);
}
void Controller::setDraft(const QString &t) {
  if (t == m_draft)
    return;
  m_draft = t;
  ++m_draftVersion;
  m_draftTimer.start();
  emit draftChanged();
}
void Controller::persistDraft() {
  if (m_id.isEmpty() || !connected())
    return;
  const auto id = m_id, text = m_draft;
  request("draft.set", {{"chat_id", id}, {"text", text}},
          [this](QJsonValue, QJsonObject e) {
            if (!e.isEmpty()) {
              m_error =
                  "Draft could not be saved: " + e.value("message").toString();
              emit stateChanged();
            }
          });
}
void Controller::fetchMessages(bool reset) {
  if (m_id.isEmpty() || m_loading)
    return;
  m_loading = true;
  emit messagesChanged();
  int gen = m_generation;
  request("message.list", {{"chat_id", m_id}, {"limit", 50}},
          [this, gen, reset](QJsonValue value, QJsonObject e) {
            if (gen != m_generation)
              return;
            m_loading = false;
            if (!e.isEmpty()) {
              if (m_pendingMediaChat == m_id) {
                m_pendingMediaChat.clear();
                m_pendingMediaMessage.clear();
              }
              m_error = e.value("message").toString();
              emit stateChanged();
              emit messagesChanged();
              return;
            }
            auto o = value.toObject();
            auto list = o.value("messages").toArray().toVariantList();
            if (reset)
              m_messages.replace(list);
            else
              m_messages.mergeTail(list);
            m_hasMore = o.value("has_more").toBool();
            emit messagesChanged();
            emit tailUpdated();
            finishPendingMediaReveal(gen);
          });
}
void Controller::finishPendingMediaReveal(int generation) {
  if (generation != m_generation || m_loading || m_pendingMediaChat.isEmpty())
    return;
  if (m_pendingMediaChat != m_id) {
    m_pendingMediaChat.clear();
    m_pendingMediaMessage.clear();
    return;
  }
  const auto messageId = m_pendingMediaMessage;
  m_pendingMediaChat.clear();
  m_pendingMediaMessage.clear();
  if (!messageId.isEmpty())
    revealMessage(messageId);
}
void Controller::loadOlder() {
  if (m_loading || !m_hasMore || m_messages.rowCount() == 0)
    return;
  m_loading = true;
  emit messagesChanged();
  int gen = m_generation;
  auto first = m_messages.row(0);
  request("message.list",
          {{"chat_id", m_id},
           {"limit", 50},
           {"before",
            QJsonObject{{"timestamp", first.value("timestamp").toLongLong()},
                        {"id", first.value("id").toString()}}}},
          [this, gen](QJsonValue value, QJsonObject e) {
            if (gen != m_generation)
              return;
            m_loading = false;
            if (!e.isEmpty()) {
              m_error = e.value("message").toString();
              emit stateChanged();
              emit messagesChanged();
              return;
            }
            auto o = value.toObject();
            m_messages.prepend(o.value("messages").toArray().toVariantList());
            m_hasMore = o.value("has_more").toBool();
            m_history = true;
            emit messagesChanged();
            emit olderLoaded();
          });
}
void Controller::latest() {
  m_history = false;
  m_loading = false;
  fetchMessages(true);
  emit messagesChanged();
}
bool Controller::startVoiceNoteRecording() {
  if (!connected() || m_id.isEmpty() || m_sending || !m_attachment.isEmpty() ||
      !m_draft.isEmpty() || m_voiceStagePending || m_voiceRecorder.recording() ||
      m_voiceRecorder.encoding())
    return false;
  m_error.clear();
  const bool started = m_voiceRecorder.startRecording();
  if (!started)
    m_error = m_voiceRecorder.error();
  emit stateChanged();
  return started;
}

void Controller::stopVoiceNoteRecording() {
  m_voiceRecorder.stopRecording();
}

void Controller::cancelVoiceNoteRecording() {
  if (m_voiceRecorder.recording() || m_voiceRecorder.encoding())
    m_voiceRecorder.cancel();
}

void Controller::send() {
  if (m_attachment.value("kind").toString() == QStringLiteral("album") ||
      !m_attachmentPath.isEmpty()) {
    sendAttachment();
    return;
  }
  if (m_sending || m_id.isEmpty() || m_draft.trimmed().isEmpty())
    return;
  if (m_draft.toUcs4().size() > 16000) {
    m_error = "Message exceeds 16000 characters";
    emit stateChanged();
    return;
  }
  persistDraft();
  m_draftTimer.stop();
  m_sending = true;
  emit stateChanged();
  const auto id = m_id, text = m_draft;
  int gen = m_generation, version = m_draftVersion;
  const auto clientId = QStringLiteral("WF") +
                        QUuid::createUuid().toString(QUuid::Id128).toUpper();
  request("message.send",
          {{"chat_id", id}, {"text", text}, {"client_id", clientId}},
          [this, gen, version, id, text](QJsonValue value, QJsonObject e) {
            m_sending = false;
            if (!e.isEmpty()) {
              m_error = e.value("message").toString();
              emit stateChanged();
              return;
            }
            auto msg = value.toObject().value("message").toObject();
            auto state = msg.value("status").toString();
            bool safe = state == "demo_sent" || state == "accepted" ||
                        state == "delivered" || state == "read";
            if (gen == m_generation) {
              if (!m_history)
                m_messages.mergeTail({msg.toVariantMap()});
              if (safe && version == m_draftVersion && m_draft == text) {
                m_draft.clear();
                ++m_draftVersion;
                emit draftChanged();
                persistDraft();
              }
              if (!safe)
                m_error =
                    "Delivery is uncertain. Check history before retrying.";
              emit messagesChanged();
              emit tailUpdated();
            } else if (safe) {
              request("draft.get", {{"chat_id", id}},
                      [this, id, text](QJsonValue v, QJsonObject er) {
                        if (er.isEmpty() &&
                            v.toObject().value("text").toString() == text)
                          request("draft.set", {{"chat_id", id}, {"text", ""}},
                                  [](QJsonValue, QJsonObject) {});
                      });
            }
            refreshChats();
            emit stateChanged();
          });
}
void Controller::saveTheme(bool dark) { QSettings().setValue("dark", dark); }
void Controller::downloadMessage(const QString &id) {
  if (m_id.isEmpty() || id.isEmpty() || m_mediaBusy.value(id, false))
    return;
  const int row = m_messages.indexOf(id);
  if (row < 0)
    return;
  auto message = m_messages.row(row);
  auto attachment = message.value("attachment").toMap();
  if (attachment.isEmpty())
    return;
  const auto state = attachment.value("state").toString();
  if (state == "ready" && QUrl(attachment.value("url").toString()).isLocalFile())
    return;
  const auto chat = m_id;
  const int generation = m_generation;
  m_mediaBusy[id] = true;
  m_mediaErrors.remove(id);
  attachment["state"] = "downloading";
  attachment["error"] = "";
  message["attachment"] = attachment;
  m_messages.mergeTail({message});
  emit messagesChanged();
  request("message.download", {{"chat_id", chat}, {"id", id}},
          [this, chat, id, generation](QJsonValue value, QJsonObject e) {
    m_mediaBusy.remove(id);
    if (generation != m_generation || chat != m_id)
      return;
    if (!e.isEmpty()) {
      const auto error = e.value("message").toString();
      m_mediaErrors[id] = error;
      const int row = m_messages.indexOf(id);
      if (row >= 0) {
        auto message = m_messages.row(row);
        auto attachment = message.value("attachment").toMap();
        attachment["state"] = "error";
        attachment["error"] = error;
        message["attachment"] = attachment;
        m_messages.mergeTail({message});
      }
      emit messagesChanged();
      return;
    }
    const auto message = value.toObject().value("message").toObject().toVariantMap();
    if (message.value("chat_id").toString() != chat || message.value("id").toString() != id)
      return;
    m_messages.mergeTail({message});
    emit messagesChanged();
  });
}
void Controller::openAttachment(const QString &id) {
  const int row = m_messages.indexOf(id);
  if (row < 0)
    return;
  const auto attachment = m_messages.row(row).value("attachment").toMap();
  const QUrl url(attachment.value("url").toString());
  if (attachment.value("state").toString() != "ready" || !url.isLocalFile())
    return;
  const QFileInfo file(url.toLocalFile());
  if (!file.exists() || !file.isFile())
    return;
  QDesktopServices::openUrl(url);
}
void Controller::openExternalLink(const QString &link) {
  const QUrl url(link);
  if ((url.scheme().compare("http", Qt::CaseInsensitive) == 0 ||
       url.scheme().compare("https", Qt::CaseInsensitive) == 0) &&
      !url.host().isEmpty())
    QDesktopServices::openUrl(url);
}
void Controller::requestQuit() {
  m_draftTimer.stop();
  if (m_id.isEmpty()) {
    m_canQuit = true;
    emit stateChanged();
    QCoreApplication::quit();
    return;
  }
  request("draft.set", {{"chat_id", m_id}, {"text", m_draft}},
          [this](QJsonValue, QJsonObject e) {
            if (e.isEmpty()) {
              m_canQuit = true;
              emit stateChanged();
              QCoreApplication::quit();
            } else {
              m_error = "Cannot close safely: draft was not saved. " +
                        e.value("message").toString();
              emit stateChanged();
            }
          });
}
void Controller::clearError() {
  m_error.clear();
  m_notice.clear();
  emit stateChanged();
}
QString Controller::timeLabel(qint64 n) const {
  return n > 0 ? QDateTime::fromMSecsSinceEpoch(n).toString("HH:mm")
               : QString();
}
QString Controller::messageStatusLabel(const QString &status) const {
  if (status == QStringLiteral("demo_sent"))
    return QStringLiteral("Saved locally");
  if (status == QStringLiteral("demo_received"))
    return QStringLiteral("Received (demo)");
  if (status == QStringLiteral("queued"))
    return QStringLiteral("Queued");
  if (status == QStringLiteral("sending"))
    return QStringLiteral("Sending");
  if (status == QStringLiteral("accepted"))
    return QStringLiteral("Sent");
  if (status == QStringLiteral("delivered"))
    return QStringLiteral("Delivered");
  if (status == QStringLiteral("read"))
    return QStringLiteral("Read");
  if (status == QStringLiteral("unknown"))
    return QStringLiteral("Outcome unknown");
  if (status == QStringLiteral("failed") ||
      status == QStringLiteral("not_dispatched"))
    return QStringLiteral("Not sent");
  if (status == QStringLiteral("received"))
    return QStringLiteral("Received");
  if (status == QStringLiteral("pending"))
    return QStringLiteral("Pending");
  return QStringLiteral("Status unavailable");
}
QString Controller::messageStatusDescription(const QString &status) const {
  if (status == QStringLiteral("demo_sent"))
    return QStringLiteral("Saved locally · not sent to WhatsApp");
  if (status == QStringLiteral("demo_received"))
    return QStringLiteral("Received in synthetic local demo");
  if (status == QStringLiteral("queued"))
    return QStringLiteral("Queued locally · not sent to WhatsApp");
  if (status == QStringLiteral("sending"))
    return QStringLiteral("Sending to WhatsApp");
  if (status == QStringLiteral("accepted"))
    return QStringLiteral("Sent · awaiting delivery receipt");
  if (status == QStringLiteral("delivered"))
    return QStringLiteral("Delivered · awaiting read receipt");
  if (status == QStringLiteral("read"))
    return QStringLiteral("Read");
  if (status == QStringLiteral("unknown"))
    return QStringLiteral("Outcome unknown · check conversation before retrying");
  if (status == QStringLiteral("failed") ||
      status == QStringLiteral("not_dispatched"))
    return QStringLiteral("Not sent");
  if (status == QStringLiteral("received"))
    return QStringLiteral("Received");
  if (status == QStringLiteral("pending"))
    return QStringLiteral("Pending");
  return QStringLiteral("Status unavailable");
}
void Controller::connectLive() {
  if (m_demo)
    return;
  request("auth.connect", {}, [this](QJsonValue, QJsonObject e) {
    if (!e.isEmpty())
      m_error = e.value("message").toString();
    m_accountRevision = -2;
    statusPoll();
    emit stateChanged();
  });
}
void Controller::disconnectLive() {
  if (m_demo)
    return;
  request("auth.disconnect", {}, [this](QJsonValue, QJsonObject e) {
    if (!e.isEmpty())
      m_error = e.value("message").toString();
    m_accountRevision = -2;
    statusPoll();
    emit stateChanged();
  });
}
