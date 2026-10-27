#include "controller.h"
#include <QClipboard>
#include <QDateTime>
#include <QDir>
#include <QGuiApplication>
#include <QFileInfo>
#include <QFileDevice>
#include <QJsonArray>
#include <QJsonDocument>
#include <QLocale>
#include <QMetaType>
#include <QRegularExpression>
#include <QSet>
#include <QUrl>
#include <QUuid>
#include <algorithm>
#include <cmath>
#include <unistd.h>

namespace {
bool safeContactDisplayText(const QString &value, int maxRunes,
                            bool allowEscapedNewline) {
  if (value.trimmed().isEmpty() || value.toUcs4().size() > maxRunes)
    return false;
  for (const auto character : value) {
    if (character.category() == QChar::Other_Control &&
        !(allowEscapedNewline && character == QLatin1Char('\n')))
      return false;
  }
  return true;
}

bool validLocationText(const QString &value) {
  if (value.toUcs4().size() > 256)
    return false;
  for (const auto codepoint : value.toUcs4())
    if (QChar::category(codepoint) == QChar::Other_Control)
      return false;
  return true;
}

bool parseLocationCoordinate(const QVariant &value, double minimum,
                             double maximum, double *output) {
  if (!output)
    return false;
  const auto text = value.toString().trimmed();
  static const QRegularExpression decimal(QStringLiteral("^[+-]?(?:[0-9]+(?:\\.[0-9]*)?|\\.[0-9]+)(?:[eE][+-]?[0-9]+)?$"));
  if (text.isEmpty() || text.size() > 32 || !decimal.match(text).hasMatch())
    return false;
  bool ok = false;
  const double coordinate = QLocale::c().toDouble(text, &ok);
  if (!ok || !std::isfinite(coordinate) || coordinate < minimum || coordinate > maximum)
    return false;
  *output = coordinate;
  return true;
}

bool pathWithinDirectory(const QString &root, const QString &path) {
  if (root.isEmpty() || path.isEmpty() || !QFileInfo(path).isAbsolute())
    return false;
  const auto cleanRoot = QDir::cleanPath(QFileInfo(root).absoluteFilePath());
  const auto cleanPath = QDir::cleanPath(QFileInfo(path).absoluteFilePath());
  const auto relative = QDir(cleanRoot).relativeFilePath(cleanPath);
  return relative != QStringLiteral(".") && relative != QStringLiteral("..") &&
         !relative.startsWith(QStringLiteral("..") + QDir::separator());
}

bool privateRegularFile(const QString &root, const QString &path) {
  const QFileInfo info(path);
  if (!pathWithinDirectory(root, path) || !info.isFile() || info.isSymLink())
    return false;
  const auto canonicalRoot = QFileInfo(root).canonicalFilePath();
  const auto canonicalPath = info.canonicalFilePath();
  return !canonicalRoot.isEmpty() && !canonicalPath.isEmpty() &&
         pathWithinDirectory(canonicalRoot, canonicalPath);
}

bool privateOwnedDirectory(const QString &path) {
  const QFileInfo info(path);
  constexpr auto sharedPermissions = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                                    QFileDevice::ExeGroup | QFileDevice::ReadOther |
                                    QFileDevice::WriteOther | QFileDevice::ExeOther;
  return info.exists() && info.isDir() && !info.isSymLink() &&
         info.ownerId() == static_cast<uint>(::geteuid()) &&
         !(info.permissions() & sharedPermissions);
}

bool privatePreparedMediaFile(const QString &profileRoot, const QString &path) {
  const auto mediaRoot = QDir(profileRoot).filePath(QStringLiteral("media"));
  const auto preparedRoot = QDir(mediaRoot).filePath(QStringLiteral("prepared"));
  const QFileInfo file(path);
  constexpr auto sharedPermissions = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                                    QFileDevice::ExeGroup | QFileDevice::ReadOther |
                                    QFileDevice::WriteOther | QFileDevice::ExeOther;
  return privateOwnedDirectory(mediaRoot) && privateOwnedDirectory(preparedRoot) &&
         privateRegularFile(preparedRoot, path) &&
         file.ownerId() == static_cast<uint>(::geteuid()) &&
         !(file.permissions() & sharedPermissions);
}

bool validAlbumItem(const QJsonObject &item, const QString &profileRoot,
                    const QString &sourcePath, qint64 *preparedSize) {
  if (!preparedSize || item.value(QStringLiteral("kind")).toString() != QStringLiteral("image") ||
      item.value(QStringLiteral("mode")).toString() != QStringLiteral("photo") ||
      item.value(QStringLiteral("state")).toString() != QStringLiteral("ready"))
    return false;
  const auto name = item.value(QStringLiteral("name")).toString();
  const auto mime = item.value(QStringLiteral("mime")).toString();
  const auto hash = item.value(QStringLiteral("sha256")).toString();
  if (name.isEmpty() || name.size() > 255 || mime.isEmpty() ||
      !mime.startsWith(QStringLiteral("image/"), Qt::CaseInsensitive) ||
      mime.compare(QStringLiteral("image/gif"), Qt::CaseInsensitive) == 0 ||
      !QRegularExpression(QStringLiteral("^[0-9a-fA-F]{64}$")).match(hash).hasMatch())
    return false;
  const auto path = item.value(QStringLiteral("prepared_path")).toString();
  const auto stageRoot = QDir(profileRoot).filePath(QStringLiteral("media/prepared"));
  if (path == QFileInfo(sourcePath).absoluteFilePath() ||
      !privateRegularFile(stageRoot, path))
    return false;
  const auto rawSize = item.value(QStringLiteral("prepared_size")).toDouble(-1);
  const auto rawOriginalSize = item.value(QStringLiteral("original_size")).toDouble(-1);
  const auto size = static_cast<qint64>(rawSize);
  const auto originalSize = static_cast<qint64>(rawOriginalSize);
  if (!std::isfinite(rawSize) || !std::isfinite(rawOriginalSize) ||
      rawSize != static_cast<double>(size) ||
      rawOriginalSize != static_cast<double>(originalSize) ||
      size <= 0 || size > 20 * 1024 * 1024 || originalSize <= 0 ||
      originalSize > 64 * 1024 * 1024 || QFileInfo(path).size() != size)
    return false;
  const QUrl url(item.value(QStringLiteral("url")).toString());
  if (!url.isLocalFile() ||
      QDir::cleanPath(QFileInfo(url.toLocalFile()).absoluteFilePath()) !=
          QDir::cleanPath(QFileInfo(path).absoluteFilePath()))
    return false;
  *preparedSize = size;
  return true;
}

bool validSentAlbumItem(const QJsonObject &item, const QString &profileRoot,
                        const QJsonObject &reviewed, qint64 *preparedSize) {
  if (!preparedSize || item.contains(QStringLiteral("prepared_path")) ||
      item.value(QStringLiteral("kind")).toString() != QStringLiteral("image") ||
      item.value(QStringLiteral("mode")).toString() != QStringLiteral("photo") ||
      item.value(QStringLiteral("state")).toString() != QStringLiteral("ready"))
    return false;
  const auto mime = item.value(QStringLiteral("mime")).toString();
  const auto hash = item.value(QStringLiteral("sha256")).toString();
  const auto name = item.value(QStringLiteral("name")).toString();
  if (name.isEmpty() || name.size() > 255 ||
      mime.compare(reviewed.value(QStringLiteral("mime")).toString(), Qt::CaseInsensitive) != 0 ||
      hash != reviewed.value(QStringLiteral("sha256")).toString() ||
      name != reviewed.value(QStringLiteral("name")).toString() ||
      !QRegularExpression(QStringLiteral("^[0-9a-fA-F]{64}$")).match(hash).hasMatch() ||
      !mime.startsWith(QStringLiteral("image/"), Qt::CaseInsensitive) ||
      mime.compare(QStringLiteral("image/gif"), Qt::CaseInsensitive) == 0)
    return false;
  const auto reviewedPreparedSize =
      reviewed.value(QStringLiteral("prepared_size")).toDouble(-1);
  const auto rawPreparedSize = item.value(QStringLiteral("prepared_size")).toDouble(-1);
  const auto rawSize = item.value(QStringLiteral("size")).toDouble(-1);
  const auto prepared = static_cast<qint64>(rawPreparedSize);
  const auto size = static_cast<qint64>(rawSize);
  if (!std::isfinite(reviewedPreparedSize) ||
      !std::isfinite(rawPreparedSize) || !std::isfinite(rawSize) ||
      reviewedPreparedSize != static_cast<double>(prepared) ||
      rawPreparedSize != static_cast<double>(prepared) ||
      rawSize != static_cast<double>(size) || prepared <= 0 ||
      prepared > 20 * 1024 * 1024 || size != prepared ||
      prepared != static_cast<qint64>(reviewedPreparedSize))
    return false;
  const QUrl url(item.value(QStringLiteral("url")).toString());
  const auto mediaRoot = QDir(profileRoot).filePath(QStringLiteral("media"));
  const auto mediaPath = url.toLocalFile();
  if (!url.isLocalFile() || !privateRegularFile(mediaRoot, mediaPath))
    return false;
  *preparedSize = prepared;
  return true;
}

bool boundedContactProjection(const QJsonArray &input, QVariantList *output) {
  if (!output || input.isEmpty() || input.size() > 10 ||
      QJsonDocument(input).toJson(QJsonDocument::Compact).size() > 64 * 1024)
    return false;
  QVariantList cards;
  for (const auto &value : input) {
    if (!value.isObject())
      return false;
    const auto source = value.toObject();
    const auto name = source.value(QStringLiteral("name")).toString();
    if (!safeContactDisplayText(name, 256, true))
      return false;
    QVariantMap card{{QStringLiteral("name"), name}};
    for (const auto &field : {QStringLiteral("phones"), QStringLiteral("emails")}) {
      const auto values = source.value(field).toArray();
      const int maximum = field == QStringLiteral("phones") ? 64 : 254;
      if (values.size() > 5)
        return false;
      QVariantList safeValues;
      for (const auto &entry : values) {
        if (!entry.isString() ||
            !safeContactDisplayText(entry.toString(), maximum, false))
          return false;
        safeValues.append(entry.toString());
      }
      if (!safeValues.isEmpty())
        card.insert(field, safeValues);
    }
    const auto organization = source.value(QStringLiteral("organization")).toString();
    if (!organization.isEmpty()) {
      if (!safeContactDisplayText(organization, 256, true))
        return false;
      card.insert(QStringLiteral("organization"), organization);
    }
    cards.append(card);
  }
  *output = cards;
  return true;
}
} // namespace

bool Controller::performMessageAction(const QString &method,
                                      const QVariantMap &message,
                                      const QVariantMap &options) {
  static const QSet<QString> supported{QStringLiteral("message.reply"),
                                       QStringLiteral("message.react"),
                                       QStringLiteral("message.edit"),
                                       QStringLiteral("message.delete"),
                                       QStringLiteral("message.forward"),
                                       QStringLiteral("message.star"),
                                       QStringLiteral("message.pin"),
                                       QStringLiteral("poll.vote"),
                                       QStringLiteral("list.reply"),
                                       QStringLiteral("button.reply")};
  const auto chatId = message.value("chat_id").toString();
  const auto id = message.value("id").toString();
  if (!supported.contains(method) || chatId.isEmpty() || chatId != m_id ||
      id.isEmpty() || !hasCapability(method))
    return false;
  const int targetRow = m_messages.indexOf(id);
  if (targetRow < 0)
    return false;
  const auto storedMessage = m_messages.row(targetRow);
  if (storedMessage.value("chat_id").toString() != chatId ||
      message.value("view_once").toBool() || message.value("ephemeral").toBool() ||
      message.value("protected").toBool() ||
      storedMessage.value("view_once").toBool() ||
      storedMessage.value("ephemeral").toBool() ||
      storedMessage.value("protected").toBool())
    return false;

  QJsonObject params{{"chat_id", chatId}};
  const bool needsClientId = method != "message.star" && method != "message.pin";
  if (method == "message.reply" || method == "message.react" || method == "poll.vote" ||
      method == "list.reply" || method == "button.reply")
    params.insert("target_id", id);
  else
    params.insert("id", id);
  if (needsClientId)
    params.insert("client_id", QStringLiteral("WF") +
                                    QUuid::createUuid().toString(QUuid::Id128).toUpper());

  QString buttonReplyLabel;
  if (method == "poll.vote") {
    if (storedMessage.value("kind").toString() != "poll" ||
        storedMessage.value("deleted").toBool() ||
        !storedMessage.value("poll_vote_state").toString().isEmpty() ||
        m_pollVoteLocks.value(chatId).contains(id))
      return false;
    const auto poll = storedMessage.value("poll").toMap();
    const auto labels = poll.value("options").toList();
    const int maxChoices = poll.value("selectable_options_count", labels.size()).toInt();
    const auto choices = QJsonValue::fromVariant(options.value("option_indices"));
    if (labels.isEmpty() || labels.size() > 12 || maxChoices <= 0 || maxChoices > labels.size() ||
        !choices.isArray() || choices.toArray().size() > maxChoices)
      return false;
    QSet<int> seen;
    for (const auto &choice : choices.toArray()) {
      // Only exact JSON numbers: never coerce strings, booleans or fractions.
      const double number = choice.toDouble(-1);
      if (!choice.isDouble() || !std::isfinite(number) || number < 0 || number >= labels.size() ||
          number != static_cast<int>(number) || seen.contains(static_cast<int>(number)))
        return false;
      seen.insert(static_cast<int>(number));
    }
    params.insert("option_indices", choices);
    m_pollVoteLocks[chatId].insert(id);
    emit pollVotesChanged();
  } else if (method == "list.reply") {
    if (storedMessage.value("kind").toString() != "list" ||
        storedMessage.value("outgoing").toBool() ||
        storedMessage.value("deleted").toBool() ||
        !storedMessage.value("list_reply_available").toBool() ||
        !storedMessage.value("list_reply_state").toString().isEmpty() ||
        m_listReplyLocks.value(chatId).contains(id))
      return false;
    const auto sections = storedMessage.value("list").toMap().value("sections").toList();
    const auto section = QJsonValue::fromVariant(options.value("section_index"));
    const auto row = QJsonValue::fromVariant(options.value("row_index"));
    if (!section.isDouble() || !row.isDouble())
      return false;
    const double sectionNumber = section.toDouble(-1);
    const double rowNumber = row.toDouble(-1);
    if (!std::isfinite(sectionNumber) || !std::isfinite(rowNumber) ||
        sectionNumber < 0 || sectionNumber >= sections.size() ||
        sectionNumber != std::floor(sectionNumber))
      return false;
    const auto rows = sections.at(static_cast<qsizetype>(sectionNumber))
                          .toMap().value("rows").toList();
    if (rowNumber < 0 || rowNumber >= rows.size() ||
        rowNumber != std::floor(rowNumber))
      return false;
    params.insert("section_index", static_cast<int>(sectionNumber));
    params.insert("row_index", static_cast<int>(rowNumber));
    m_listReplyLocks[chatId].insert(id);
    emit listRepliesChanged();
  } else if (method == "button.reply") {
    if (!m_demo || storedMessage.value("kind").toString() != "buttons" ||
        storedMessage.value("outgoing").toBool() ||
        storedMessage.value("deleted").toBool() ||
        !storedMessage.value("button_reply_available").toBool() ||
        !storedMessage.value("button_reply_state").toString().isEmpty() ||
        m_buttonReplyLocks.value(chatId).contains(id))
      return false;
    const auto labels = storedMessage.value("buttons").toMap().value("buttons").toList();
    const auto selection = QJsonValue::fromVariant(options.value("button_index"));
    if (labels.isEmpty() || labels.size() > 3 || !selection.isDouble())
      return false;
    const double selectionNumber = selection.toDouble(-1);
    if (!std::isfinite(selectionNumber) || selectionNumber < 0 ||
        selectionNumber >= labels.size() || selectionNumber != std::floor(selectionNumber))
      return false;
    buttonReplyLabel = labels.at(static_cast<qsizetype>(selectionNumber)).toString();
    if (buttonReplyLabel.trimmed().isEmpty() || buttonReplyLabel.toUcs4().size() > 256)
      return false;
    params.insert("button_index", static_cast<int>(selectionNumber));
    m_buttonReplyLocks[chatId].insert(id);
    emit buttonRepliesChanged();
  } else if (method == "message.reply") {
    const auto text = options.value("text").toString();
    if (text.trimmed().isEmpty() || text.toUcs4().size() > 16000)
      return false;
    params.insert("text", text);
  } else if (method == "message.react") {
    const auto reaction = options.value("reaction").toString();
    if (reaction.toUcs4().size() > 16)
      return false;
    params.insert("reaction", reaction);
  } else if (method == "message.edit") {
    const auto text = options.value("text").toString();
    if (text.toUcs4().size() > 16000)
      return false;
    params.insert("text", text);
  } else if (method == "message.delete") {
    const auto scope = options.value("scope").toString();
    if (scope != "local" && scope != "everyone")
      return false;
    params.insert("scope", scope);
  } else if (method == "message.forward") {
    const auto target = options.value("target_chat_id").toString();
    if (target.isEmpty() || target == chatId || m_chats.indexOf(target) < 0)
      return false;
    params.insert("target_chat_id", target);
  } else if (method == "message.star") {
    params.insert("starred", options.value("starred").toBool());
  } else if (method == "message.pin") {
    const auto seconds = options.value("duration_seconds", 0).toInt();
    if (seconds < 0)
      return false;
    params.insert("pinned", options.value("pinned").toBool());
    params.insert("duration_seconds", seconds);
  }

  const int generation = m_generation;
  request(method, params, [this, method, chatId, id, generation,
                           buttonReplyLabel](QJsonValue value, QJsonObject error) {
    QJsonObject listReplyTarget;
    QJsonObject buttonReplyTarget;
    if (method == "list.reply") {
      const auto action = value.toObject();
      const auto target = action.value("target").toObject();
      const auto returned = action.value("message").toObject();
      if (error.isEmpty() && target.value("chat_id").toString() == chatId &&
          target.value("id").toString() == id &&
          target.value("list_reply_state").toString() == "accepted" &&
          !target.value("list_reply_available").toBool() &&
          returned.value("chat_id").toString() == chatId &&
          !returned.value("id").toString().isEmpty() &&
          returned.value("outgoing").toBool() &&
          returned.value("status").toString() == "accepted") {
        listReplyTarget = target;
      } else if (error.isEmpty()) {
        error = {{"code", "unknown"},
                 {"message", "List response outcome is unknown. Check this message before trying again."}};
      } else if (error.value("code").toString() == "not_dispatched") {
        m_listReplyLocks[chatId].remove(id);
        emit listRepliesChanged();
      }
    }
    if (method == "button.reply") {
      const auto action = value.toObject();
      const auto target = action.value("target").toObject();
      const auto returned = action.value("message").toObject();
      if (error.isEmpty() && m_demo && action.value("status").toString() == "demo_sent" &&
          target.value("chat_id").toString() == chatId &&
          target.value("id").toString() == id &&
          target.value("kind").toString() == "buttons" &&
          target.value("button_reply_state").toString() == "local" &&
          !target.value("button_reply_available").toBool() &&
          returned.value("chat_id").toString() == chatId &&
          !returned.value("id").toString().isEmpty() &&
          returned.value("id").toString() != id &&
          returned.value("kind").toString() == "text" &&
          returned.value("body").toString() == buttonReplyLabel &&
          returned.value("reply_to_id").toString() == id &&
          returned.value("outgoing").toBool() &&
          returned.value("status").toString() == "demo_sent") {
        buttonReplyTarget = target;
        m_buttonReplyLocks[chatId].remove(id);
        emit buttonRepliesChanged();
      } else if (error.isEmpty()) {
        error = {{"code", "unknown"},
                 {"message", "Button response outcome is unknown. Check this message before trying again."}};
      } else if (error.value("code").toString() == "not_dispatched") {
        m_buttonReplyLocks[chatId].remove(id);
        emit buttonRepliesChanged();
      }
    }
    if (method == "poll.vote") {
      const auto returned = value.toObject().value("message").toObject();
      if (error.isEmpty() && returned.value("chat_id").toString() == chatId &&
          returned.value("id").toString() == id) {
        m_pollVoteLocks[chatId].remove(id);
        emit pollVotesChanged();
      } else if (error.isEmpty()) {
        error = {{"code", "unknown"},
                 {"message", "Vote outcome is unknown. Check this poll before trying again."}};
      }
    }
    if (generation != m_generation || chatId != m_id)
      return;
    if (!error.isEmpty()) {
      const auto code = error.value("code").toString();
      m_error = (code == "timeout" || code == "disconnected")
                    ? QStringLiteral("Action outcome is unknown. Check this conversation before trying again.")
                    : error.value("message").toString();
      emit stateChanged();
      return;
    }
    const auto returned = value.toObject().value("message").toObject();
    QVariantList updates;
    bool flagUpdated = false;
    if (!listReplyTarget.isEmpty())
      updates.append(listReplyTarget.toVariantMap());
    if (!buttonReplyTarget.isEmpty())
      updates.append(buttonReplyTarget.toVariantMap());
    if (returned.value("chat_id").toString() == chatId &&
        !returned.value("id").toString().isEmpty())
      updates.append(returned.toVariantMap());
    if (method == "message.star" || method == "message.pin") {
      const auto action = value.toObject();
      const int index = m_messages.indexOf(id);
      if (action.value("chat_id").toString() == chatId &&
          action.value("id").toString() == id &&
          action.value("value").isBool() &&
          action.value("provenance").toString() == QStringLiteral("local_only") &&
          index >= 0) {
        auto updated = m_messages.row(index);
        if (updated.value("chat_id").toString() == chatId) {
          updated.insert(method == "message.star" ? QStringLiteral("starred")
                                                   : QStringLiteral("pinned"),
                         action.value("value").toBool());
          updated.insert(QStringLiteral("mutation_provenance"),
                         action.value("provenance").toString());
          m_messages.updateRow(updated);
          flagUpdated = true;
        }
      }
    }
    if (!updates.isEmpty()) {
      m_messages.mergeTail(updates);
      emit messagesChanged();
      emit tailUpdated();
    } else if (flagUpdated) {
      emit messagesChanged();
    }
    // No UI state is changed optimistically; action response projections are
    // authoritative.
  });
  return true;
}
void Controller::searchContacts(const QString &query) {
  m_contactQuery = query;
  ++m_contactSerial;
  m_contactTimer.stop();
  m_contacts.replace({});
  m_contactCursor.clear();
  m_contactsHasMore = false;
  m_contactsLoading = false;
  m_contactsError.clear();
  if (!hasCapability("contact.list")) {
    m_contactsLoading = false;
    m_contactsError = capabilityReason("contact.list");
    emit contactsChanged();
    return;
  }
  m_contactsLoading = true;
  emit contactsChanged();
  m_contactTimer.start();
}
void Controller::loadMoreContacts() {
  if (!m_contactsHasMore || m_contactsLoading || m_contactCursor.isEmpty() ||
      !hasCapability("contact.list"))
    return;
  m_contactsLoading = true;
  m_contactsError.clear();
  emit contactsChanged();
  m_contactTimer.start();
}
void Controller::openChatRecipient(const QString &recipient) {
  const auto value = recipient.trimmed();
  if (value.isEmpty() || value.size() > 100 || !hasCapability("chat.open"))
    return;
  request("chat.open", {{"recipient", value}},
          [this](QJsonValue result, QJsonObject error) {
    if (!error.isEmpty()) {
      m_error = error.value("message").toString();
      emit stateChanged();
      return;
    }
    const auto chat = result.toObject().value("chat").toObject().toVariantMap();
    const auto id = chat.value("id").toString();
    const auto title = chat.value("title").toString().trimmed();
    if (id.isEmpty() || title.isEmpty() || title.contains('@')) {
      m_error = "The backend did not return a verified contact identity.";
      emit stateChanged();
      return;
    }
    m_chats.mergeTail({chat});
    selectChat(id);
    refreshChats();
  });
}
bool Controller::updateChat(const QString &chatId, const QVariantMap &changes) {
  static const QSet<QString> allowed{QStringLiteral("archived"),
                                     QStringLiteral("pinned"),
                                     QStringLiteral("muted"),
                                     QStringLiteral("unread"),
                                     QStringLiteral("favorite"),
                                     QStringLiteral("list_ids")};
  if (chatId.isEmpty() || m_chats.indexOf(chatId) < 0 || changes.isEmpty() ||
      !hasCapability("chat.update"))
    return false;
  QJsonObject params{{"chat_id", chatId}};
  for (auto it = changes.cbegin(); it != changes.cend(); ++it) {
    if (!allowed.contains(it.key()))
      return false;
    params.insert(it.key(), QJsonValue::fromVariant(it.value()));
  }
  request("chat.update", params, [this, chatId](QJsonValue value, QJsonObject error) {
    if (!error.isEmpty()) {
      m_error = error.value("message").toString();
      emit stateChanged();
      return;
    }
    const auto chat = value.toObject().value("chat").toObject().toVariantMap();
    if (chat.value("id").toString() == chatId)
      m_chats.mergeTail({chat});
    // Do not change local flags until a server/local projection confirms them.
  });
  return true;
}
bool Controller::refreshCommunities() {
  if (m_demo || (m_liveMode && !m_profileViewReady) ||
      m_connectionState != "connected" || m_communitiesRefreshing ||
      !hasCapability("community.refresh"))
    return false;
  m_communitiesRefreshing = true;
  emit stateChanged();
  request("community.refresh", {}, [this](QJsonValue value, QJsonObject error) {
    m_communitiesRefreshing = false;
    if (!error.isEmpty()) {
      m_error = error.value("message").toString();
    } else if (value.toObject().value("refreshed").toBool()) {
      m_notice = "Community folders refreshed. No messages or group memberships were changed.";
      refreshChats();
      m_capabilitiesReady = false;
      refreshCapabilities();
    } else {
      m_error = "Community refresh was not confirmed; previous folders are retained.";
    }
    emit stateChanged();
  });
  return true;
}
bool Controller::loadHub(const QString &method, const QVariantMap &params) {
  const bool selectedChatOnly = method == QStringLiteral("chat.info") || method == QStringLiteral("media.list");
  if (selectedChatOnly && (m_id.isEmpty() || params.value("chat_id").toString() != m_id))
    return false;
  if (method == "contact.list") {
    searchContacts(params.value("query").toString());
    return true;
  }
  static const QSet<QString> readable{QStringLiteral("chat.info"),
                                      QStringLiteral("media.list"),
                                      QStringLiteral("media.library"),
                                      QStringLiteral("privacy.get"),
                                      QStringLiteral("group.list"),
                                      QStringLiteral("group.info"),
                                      QStringLiteral("channel.list"),
                                      QStringLiteral("channel.info"),
                                      QStringLiteral("channel.history"),
                                      QStringLiteral("community.list"),
                                      QStringLiteral("community.info"),
                                      QStringLiteral("status.list")};
  if (!readable.contains(method) || !hasCapability(method)) {
    m_routeError = capabilityReason(method);
    m_routeMethod = method;
    m_routeLoading = false;
    emit routeChanged();
    return false;
  }
  if (m_routeLoading)
    return false;
  ++m_routeSerial;
  const int serial = m_routeSerial;
  m_routeMethod = method;
  m_routeError.clear();
  m_routeData.clear();
  m_routeLoading = true;
  emit routeChanged();
  const auto requestedChat = selectedChatOnly ? m_id : QString();
  request(method, QJsonObject::fromVariantMap(params),
          [this, method, serial, selectedChatOnly, requestedChat](QJsonValue value, QJsonObject error) {
    if (serial != m_routeSerial || (selectedChatOnly && requestedChat != m_id))
      return;
    m_routeLoading = false;
    const auto returnedChat = value.toObject().value(method == QStringLiteral("chat.info") ? "id" : "chat_id").toString();
    if (error.isEmpty() && selectedChatOnly && returnedChat != requestedChat)
      error = {{"message", "Conversation details did not match the selected chat."}};
    if (!error.isEmpty()) {
      m_routeError = error.value("message").toString();
    } else {
      m_routeData = sanitizeProfileMediaUrls(value.toObject().toVariantMap()).toMap();
      if (method == "contact.list")
        m_contacts.replace(value.toObject().value("contacts").toArray().toVariantList());
    }
    emit routeChanged();
  });
  return true;
}
bool Controller::performCapabilityAction(const QString &method,
                                         const QVariantMap &params) {
  static const QSet<QString> writable{QStringLiteral("account.update"),
                                      QStringLiteral("privacy.set"),
                                      QStringLiteral("group.create"),
                                      QStringLiteral("group.join"),
                                      QStringLiteral("group.leave"),
                                      QStringLiteral("group.update"),
                                      QStringLiteral("group.participants")};
  if (!writable.contains(method) || !hasCapability(method))
    return false;
  ++m_routeSerial;
  const int serial = m_routeSerial;
  m_routeMethod = method;
  m_routeError.clear();
  m_routeLoading = true;
  emit routeChanged();
  request(method, QJsonObject::fromVariantMap(params),
          [this, serial, method](QJsonValue value, QJsonObject error) {
    if (serial != m_routeSerial)
      return;
    m_routeLoading = false;
    if (!error.isEmpty()) {
      m_routeError = error.value("message").toString();
      if (error.value("code").toString() == "timeout" ||
          error.value("code").toString() == "disconnected")
        m_routeError = "Outcome is unknown. Review service state before retrying.";
    } else {
      m_routeData = sanitizeProfileMediaUrls(value.toObject().toVariantMap()).toMap();
      if (method == QStringLiteral("group.create") ||
          method == QStringLiteral("group.update")) {
        const auto chat = value.toObject().value(QStringLiteral("chat")).toObject().toVariantMap();
        if (!chat.value(QStringLiteral("id")).toString().isEmpty())
          m_chats.mergeTail({chat});
      }
    }
    emit routeChanged();
  });
  return true;
}
void Controller::openSelfChat() {
  request("chat.self", {}, [this](QJsonValue value, QJsonObject error) {
    if (!error.isEmpty()) {
      m_error = error.value("message").toString();
      emit stateChanged();
      return;
    }
    const auto chat = value.toObject().value("chat").toObject().toVariantMap();
    const auto id = chat.value("id").toString();
    if (id.isEmpty()) return;
    m_chats.mergeTail({chat});
    selectChat(id);
    refreshChats();
  });
}
void Controller::refreshHistory() {
  if (m_id.isEmpty() || m_syncPending) return;
  const auto chat = m_id;
  const auto generation = m_generation;
  m_syncPending = true;
  request("history.sync", {{"chat_id", chat}},
          [this, chat, generation](QJsonValue value, QJsonObject error) {
    m_syncPending = false;
    if (chat != m_id || generation != m_generation) return;
    if (!error.isEmpty()) m_error = error.value("message").toString();
    else {
      const auto result = value.toObject();
      if (result.value("requested").toBool()) {
        const auto count = result.value("count").toInt();
        const auto timestamp = static_cast<qint64>(result.value("anchor_timestamp").toDouble());
        const auto anchor = QDateTime::fromMSecsSinceEpoch(timestamp).toLocalTime();
        if (count > 0 && count <= 50 && result.contains("anchor_timestamp") && anchor.isValid())
          m_notice = QStringLiteral("Requested up to %1 older messages before %2. Messages may arrive asynchronously; this does not confirm a complete sync.")
                         .arg(count)
                         .arg(anchor.toString(Qt::ISODate));
        else
          m_notice = "History requested from your phone. Messages will appear as they arrive; this does not confirm a complete sync.";
      }
    }
    emit stateChanged();
  });
}

void Controller::findMessages(const QString &query) {
  m_findQuery = query;
  m_findTimer.stop();
  m_searchResults.replace({});
  m_searching = !query.trimmed().isEmpty() && !m_id.isEmpty();
  emit findChanged();
  if (m_searching)
    m_findTimer.start();
}
void Controller::runFind() {
  auto query = m_findQuery;
  if (m_liveMode && !m_profileViewReady) {
    m_searching = false;
    m_searchResults.replace({});
    emit findChanged();
    return;
  }
  int gen = m_generation;
  request("message.search", {{"chat_id", m_id}, {"query", query}},
          [this, query, gen](QJsonValue value, QJsonObject error) {
            if (gen != m_generation || query != m_findQuery)
              return;
            m_searching = false;
            if (error.isEmpty())
              m_searchResults.replace(sanitizeProfileMediaUrls(
                  value.toObject().value("messages").toArray().toVariantList()).toList());
            else {
              m_error = error.value("message").toString();
              emit stateChanged();
            }
            emit findChanged();
          });
}
bool Controller::revealMediaLibraryMessage(const QString &chatId,
                                           const QString &messageId) {
  const auto validToken = [](const QString &value) {
    if (value.isEmpty() || value.size() > 512 || value != value.trimmed())
      return false;
    for (const auto ch : value)
      if (ch.isNull() || ch.category() == QChar::Other_Control)
        return false;
    return true;
  };
  // The sidebar may be search-filtered or paginated. A cached library target
  // need not be present there; message.list/context validate its exact identity.
  if ((m_liveMode && !m_profileViewReady) || !connected() ||
      !hasCapability(QStringLiteral("media.library")) ||
      !validToken(chatId) || !validToken(messageId)) {
    m_error = QStringLiteral("This cached media item cannot be opened in the selected conversation.");
    emit stateChanged();
    return false;
  }
  m_pendingMediaChat = chatId;
  m_pendingMediaMessage = messageId;
  if (m_id != chatId) {
    selectChat(chatId);
    if (m_id == chatId)
      return true;
    m_pendingMediaChat.clear();
    m_pendingMediaMessage.clear();
    m_error = QStringLiteral("Finish the current conversation action before opening this cached media item.");
    emit stateChanged();
    return false;
  }
  if (!m_loading)
    finishPendingMediaReveal(m_generation);
  return true;
}
void Controller::revealMessage(const QString &id) {
  if (m_id.isEmpty() || (m_liveMode && !m_profileViewReady))
    return;
  int gen = m_generation;
  m_loading = true;
  emit messagesChanged();
  request("message.context", {{"chat_id", m_id}, {"message_id", id}},
          [this, gen, id](QJsonValue value, QJsonObject error) {
            if (gen != m_generation)
              return;
            m_loading = false;
            if (error.isEmpty()) {
              auto o = value.toObject();
              const auto rows = sanitizeProfileMediaUrls(
                  o.value("messages").toArray().toVariantList()).toList();
              const bool exactTarget = std::any_of(rows.cbegin(), rows.cend(),
                  [this, &id](const QVariant &item) {
                    const auto row = item.toMap();
                    return row.value("chat_id").toString() == m_id &&
                           row.value("id").toString() == id;
                  });
              if (!exactTarget) {
                m_error = QStringLiteral("The requested message was not found in this conversation.");
                emit stateChanged();
                emit messagesChanged();
              } else {
                m_messages.replace(rows);
                m_hasMore = o.value("has_more").toBool();
                m_history = true;
                emit messagesChanged();
                emit messageRevealed(id);
              }
            } else {
              m_error = error.value("message").toString();
              emit stateChanged();
              emit messagesChanged();
            }
            finishPendingMediaReveal(gen);
          });
}
void Controller::stageAttachment(const QUrl &file) {
  if (m_liveMode) {
    stageAttachmentAs(file, QStringLiteral("photo"));
    return;
  }
  if (m_id.isEmpty() || !file.isLocalFile()) { m_error = "Select a conversation and a local file"; emit stateChanged(); return; }
  int gen=m_generation, serial=++m_attachmentSerial; auto path=file.toLocalFile();
  request("attachment.inspect", {{"path", path}}, [this,path,gen,serial](QJsonValue value,QJsonObject error) {
    if (gen!=m_generation || serial!=m_attachmentSerial) return;
    if (!error.isEmpty()) { m_error=error.value("message").toString(); emit stateChanged(); return; }
    m_attachment=value.toObject().toVariantMap(); m_attachment["state"]="ready"; m_attachment["mode"]="photo"; m_attachment["path"]=path; m_attachment["original_path"]=path;
    m_attachmentPath=path; m_attachmentOriginalPath=path; m_attachmentMode="photo"; m_attachmentSendId.clear(); emit attachmentChanged();
  });
}
void Controller::stageAttachmentAlbum(const QVariantList &files) {
  const auto failSelection = [this](const QString &message) {
    m_error = message;
    emit stateChanged();
  };
  if (!m_demo || !connected() || m_id.isEmpty() || m_sending ||
      !hasCapability(QStringLiteral("attachment.album.prepare")) ||
      !hasCapability(QStringLiteral("message.album.send")) ||
      !hasCapability(QStringLiteral("attachment.album.release"))) {
    failSelection(QStringLiteral("Photo albums are available only in the local demo backend."));
    return;
  }
  if (files.size() < 2 || files.size() > 10) {
    failSelection(QStringLiteral("Choose between 2 and 10 still images for an album."));
    return;
  }

  QStringList paths;
  QSet<QString> seen;
  qint64 sourceBytes = 0;
  for (const auto &entry : files) {
    const auto url = entry.toUrl();
    if (!url.isLocalFile()) {
      failSelection(QStringLiteral("Choose local image files."));
      return;
    }
    const QFileInfo info(url.toLocalFile());
    const auto canonicalPath = info.canonicalFilePath();
    if (!info.isFile() || info.isSymLink() || canonicalPath.isEmpty() ||
        info.size() <= 0 || info.size() > 64 * 1024 * 1024 ||
        sourceBytes + info.size() > 256 * 1024 * 1024) {
      failSelection(QStringLiteral("Each selected album file must be a regular image no larger than 64 MiB; the selection is bounded to 256 MiB."));
      return;
    }
    if (seen.contains(canonicalPath)) {
      failSelection(QStringLiteral("An image can appear only once in an album."));
      return;
    }
    seen.insert(canonicalPath);
    paths.append(canonicalPath);
    sourceBytes += info.size();
  }

  if (!m_attachment.isEmpty())
    clearAttachment();
  const int generation = m_generation;
  const int serial = ++m_attachmentSerial;
  m_attachmentPath.clear();
  m_attachmentOriginalPath.clear();
  m_attachmentMode = QStringLiteral("album");
  m_attachmentSendId.clear();
  m_albumPreparedPaths.clear();
  m_error.clear();
  m_attachment = {{QStringLiteral("name"), QStringLiteral("Photo album")},
                  {QStringLiteral("kind"), QStringLiteral("album")},
                  {QStringLiteral("mode"), QStringLiteral("photo")},
                  {QStringLiteral("state"), QStringLiteral("preparing")},
                  {QStringLiteral("selected_count"), paths.size()},
                  {QStringLiteral("provenance"), QStringLiteral("local_demo")}};
  emit attachmentChanged();
  emit stateChanged();

  QJsonArray jsonPaths;
  for (const auto &path : paths)
    jsonPaths.append(path);
  request(QStringLiteral("attachment.album.prepare"),
          {{QStringLiteral("paths"), jsonPaths}},
          [this, paths, generation, serial](QJsonValue value, QJsonObject error) {
    const auto responsePaths = [&] {
      QStringList result;
      const auto items = value.toObject().value(QStringLiteral("items")).toArray();
      for (const auto &entry : items) {
        const auto path = entry.toObject().value(QStringLiteral("prepared_path")).toString();
        if (!path.isEmpty() && QFileInfo(path).isAbsolute() && !result.contains(path))
          result.append(path);
      }
      return result;
    };
    if (generation != m_generation || serial != m_attachmentSerial) {
      if (error.isEmpty())
        releaseAlbumPaths(responsePaths());
      return;
    }
    if (!error.isEmpty()) {
      m_error = error.value(QStringLiteral("message")).toString();
      m_attachment[QStringLiteral("state")] = QStringLiteral("error");
      m_attachment[QStringLiteral("error")] = m_error;
      emit attachmentChanged();
      emit stateChanged();
      return;
    }

    const auto result = value.toObject();
    const auto items = result.value(QStringLiteral("items")).toArray();
    const auto rawTotal = result.value(QStringLiteral("total_prepared_size")).toDouble(-1);
    const auto total = static_cast<qint64>(rawTotal);
    bool valid = items.size() == paths.size() && items.size() >= 2 &&
                 items.size() <= 10 &&
                 result.value(QStringLiteral("provenance")).toString() ==
                     QStringLiteral("local_demo") &&
                 std::isfinite(rawTotal) && rawTotal == static_cast<double>(total) &&
                 total > 0 && total <= 50 * 1024 * 1024 &&
                 QJsonDocument(result).toJson(QJsonDocument::Compact).size() <= 64 * 1024;
    qint64 calculatedTotal = 0;
    QStringList preparedPaths;
    for (int i = 0; valid && i < items.size(); ++i) {
      qint64 itemSize = 0;
      const auto item = items.at(i).toObject();
      if (!validAlbumItem(item, m_dataDir, paths.at(i), &itemSize) ||
          calculatedTotal + itemSize > 50 * 1024 * 1024) {
        valid = false;
        break;
      }
      calculatedTotal += itemSize;
      preparedPaths.append(item.value(QStringLiteral("prepared_path")).toString());
    }
    if (!valid || calculatedTotal != total) {
      releaseAlbumPaths(responsePaths());
      m_error = QStringLiteral("Album preparation did not return a validated bounded set of private still images.");
      m_attachment[QStringLiteral("state")] = QStringLiteral("error");
      m_attachment[QStringLiteral("error")] = m_error;
      emit attachmentChanged();
      emit stateChanged();
      return;
    }
    m_albumPreparedPaths = preparedPaths;
    m_attachment.insert(QStringLiteral("name"), QStringLiteral("%1 photos").arg(items.size()));
    m_attachment.insert(QStringLiteral("items"), items.toVariantList());
    m_attachment.insert(QStringLiteral("total_prepared_size"), total);
    m_attachment.insert(QStringLiteral("state"), QStringLiteral("ready"));
    emit attachmentChanged();
    emit stateChanged();
  });
}
void Controller::stageAttachmentAs(const QUrl &file, const QString &mode) {
  stageAttachmentAsWithDisplayName(file, mode, QString());
}

void Controller::stageAttachmentAsWithDisplayName(const QUrl &file,
                                                  const QString &mode,
                                                  const QString &displayName) {
  if (m_id.isEmpty() || !file.isLocalFile()) {
    m_error = "Select a conversation and a local file";
    emit stateChanged();
    return;
  }
  if (m_liveMode && !m_profileViewReady) {
    m_error = QStringLiteral("The encrypted profile view must be verified before preparing media.");
    emit stateChanged();
    return;
  }
  int gen = m_generation, serial = ++m_attachmentSerial;
  auto path = file.toLocalFile();
  m_attachmentOriginalPath = path;
  m_attachmentPath.clear();
  m_attachmentSendId.clear();
  m_attachmentMode = mode.isEmpty() ? QStringLiteral("photo") : mode;
  m_attachment = {{"name", QFileInfo(path).fileName()}, {"state", "preparing"},
                  {"mode", m_attachmentMode}, {"path", path}, {"original_path", path}, {"original_size", QFileInfo(path).size()}};
  if (!displayName.isEmpty())
    m_attachment["display_name"] = displayName;
  emit attachmentChanged();
  request("attachment.prepare", {{"path", path}, {"mode", m_attachmentMode}},
          [this, path, gen, serial, displayName](QJsonValue value, QJsonObject error) {
            if (gen != m_generation || serial != m_attachmentSerial) {
              finishVoiceStaging(serial);
              return;
            }
            if (m_liveMode && !error.isEmpty()) {
              m_attachment["state"] = "error";
              m_attachment["error"] =
                  QStringLiteral("The encrypted backend could not safely prepare this media.");
              m_error = m_attachment["error"].toString();
              finishVoiceStaging(serial);
              emit attachmentChanged();
              emit stateChanged();
              return;
            }
            if (!error.isEmpty() && error.value("code").toString() != "method_not_found") {
              m_attachment["state"] = "error"; m_attachment["error"] = error.value("message").toString();
              finishVoiceStaging(serial);
              emit attachmentChanged(); return;
            }
            const auto preparedMeta = value.toObject();
            const auto preparedPath = preparedMeta.value("prepared_path").toString();
            if (m_liveMode &&
                (preparedMeta.isEmpty() ||
                 !privatePreparedMediaFile(m_dataDir, preparedPath) ||
                 !isActiveProfileMediaFile(preparedPath))) {
              m_attachment["state"] = "error";
              m_attachment["error"] =
                  QStringLiteral("Live media preparation returned a file outside the verified encrypted profile.");
              m_error = m_attachment["error"].toString();
              finishVoiceStaging(serial);
              emit attachmentChanged();
              emit stateChanged();
              return;
            }
            const bool audioMode = m_attachmentMode == "audio" ||
                                   m_attachmentMode == "voice-note";
            if (audioMode) {
              const auto preparedPath =
                  preparedMeta.value("prepared_path").toString();
              const bool expectedVoiceNote = m_attachmentMode == "voice-note";
              const auto preparedSize =
                  preparedMeta.value("prepared_size").toDouble(-1);
              const bool validAudioStage =
                  error.isEmpty() && !preparedMeta.isEmpty() &&
                  preparedMeta.value("state").toString() == "ready" &&
                  preparedMeta.value("kind").toString() == "audio" &&
                  preparedMeta.value("mime").toString() ==
                      "audio/ogg; codecs=opus" &&
                  preparedMeta.value("audio_codec").toString() == "opus" &&
                  preparedMeta.value("mode").toString() == m_attachmentMode &&
                  preparedMeta.value("is_voice_note").isBool() &&
                  preparedMeta.value("is_voice_note").toBool() ==
                      expectedVoiceNote &&
                  !preparedPath.isEmpty() && QFileInfo(preparedPath).isAbsolute() &&
                  preparedPath != path && preparedSize > 0 &&
                  preparedSize <= 20 * 1024 * 1024;
              if (!validAudioStage) {
                m_attachment["state"] = "error";
                m_attachment["error"] =
                    "Audio preparation did not return a validated private OGG/Opus file.";
                finishVoiceStaging(serial);
                emit attachmentChanged();
                return;
              }
            }
            if (!error.isEmpty() || preparedMeta.isEmpty()) {
              request("attachment.inspect", {{"path", path}},
                [this, path, gen, serial, displayName](QJsonValue v, QJsonObject e) {
                  if (gen != m_generation || serial != m_attachmentSerial) {
                    finishVoiceStaging(serial);
                    return;
                  }
                  if (!e.isEmpty()) { m_attachment["state"]="error"; m_attachment["error"]=e.value("message").toString(); finishVoiceStaging(serial); emit attachmentChanged(); return; }
                  m_attachment = v.toObject().toVariantMap();
                  if (!displayName.isEmpty())
                    m_attachment["display_name"] = displayName;
                  m_attachment["mode"] = m_attachmentMode; m_attachment["path"] = path; m_attachment["original_path"] = path;
                  m_attachment["state"] = "ready"; m_attachmentPath = path; m_attachmentSendId.clear(); finishVoiceStaging(serial); emit attachmentChanged();
                });
              return;
            }
            m_attachment = preparedMeta.toVariantMap();
            if (!displayName.isEmpty())
              m_attachment["display_name"] = displayName;
            m_attachment["original_path"] = path;
            m_attachment["path"] = path;
            m_attachment["state"] = "ready"; m_attachmentPath = m_attachment.value("prepared_path").toString();
            if (m_attachmentPath.isEmpty()) m_attachmentPath = path;
            m_attachment["mode"] = m_attachmentMode; m_attachmentSendId.clear(); finishVoiceStaging(serial); emit attachmentChanged();
          });
}
void Controller::stageContactFile(const QUrl &file) {
  if (m_contactSending || m_id.isEmpty() || !file.isLocalFile() ||
      !hasCapability(QStringLiteral("contact.prepare"))) {
    m_contactShareError = QStringLiteral("Choose a .vcf file in an available conversation.");
    emit contactShareChanged();
    return;
  }
  const auto path = file.toLocalFile();
  const QFileInfo info(path);
  if (!info.isFile() || info.isSymLink() || info.size() <= 0 ||
      info.size() > 64 * 1024 ||
      info.suffix().compare(QStringLiteral("vcf"), Qt::CaseInsensitive) != 0) {
    m_contactShareError = QStringLiteral("Choose one regular local .vcf file no larger than 64 KiB.");
    emit contactShareChanged();
    return;
  }
  const int generation = m_generation;
  const int serial = ++m_contactFileSerial;
  m_contactPath = path;
  m_contactCards.clear();
  m_contactSendId.clear();
  m_contactShareError.clear();
  m_contactPreparing = true;
  m_contactOutcomeUnknown = false;
  emit contactShareChanged();
  request("contact.prepare", {{"path", path}},
          [this, generation, serial](QJsonValue value, QJsonObject error) {
    if (generation != m_generation || serial != m_contactFileSerial)
      return;
    m_contactPreparing = false;
    if (!error.isEmpty()) {
      m_contactShareError = error.value("message").toString();
      emit contactShareChanged();
      return;
    }
    QVariantList cards;
    if (!boundedContactProjection(value.toObject().value("contacts").toArray(), &cards)) {
      m_contactShareError = QStringLiteral("The selected file did not return a safe bounded contact projection.");
      emit contactShareChanged();
      return;
    }
    m_contactCards = cards;
    emit contactShareChanged();
  });
}

bool Controller::sendContactCards() {
  if (m_contactSending || m_sending || m_contactPreparing ||
      m_contactOutcomeUnknown || m_id.isEmpty() || m_contactCards.isEmpty() ||
      !connected() || !hasCapability(QStringLiteral("message.contact.send")))
    return false;
  const auto chat = m_id;
  const int generation = m_generation;
  const int serial = m_contactFileSerial;
  if (m_contactSendId.isEmpty())
    m_contactSendId = QStringLiteral("WF") +
                      QUuid::createUuid().toString(QUuid::Id128).toUpper();
  const auto clientId = m_contactSendId;
  const auto contacts = m_contactCards;
  QJsonArray jsonContacts;
  for (const auto &card : contacts)
    jsonContacts.append(QJsonObject::fromVariantMap(card.toMap()));
  m_contactSending = true;
  m_contactShareError.clear();
  emit contactShareChanged();
  request("message.contact.send",
          {{"chat_id", chat}, {"contacts", jsonContacts}, {"client_id", clientId}},
          [this, chat, generation, serial, clientId](QJsonValue value, QJsonObject error) {
    m_contactSending = false;
    if (!error.isEmpty()) {
      const auto code = error.value("code").toString();
      m_contactOutcomeUnknown = code == QStringLiteral("unknown_outcome") ||
                                code == QStringLiteral("timeout") ||
                                code == QStringLiteral("disconnected");
      m_contactShareError = m_contactOutcomeUnknown
          ? QStringLiteral("Outcome is unknown. Check this conversation; this share will not be sent again automatically.")
          : error.value("message").toString();
      if (m_contactOutcomeUnknown && generation == m_generation && chat == m_id) {
        fetchMessages(true);
        refreshChats();
      }
      emit contactShareChanged();
      emit stateChanged();
      return;
    }
    const auto message = value.toObject().value("message").toObject();
    const auto status = message.value("status").toString();
    if (message.value("id").toString() != clientId ||
        message.value("chat_id").toString() != chat ||
        message.value("kind").toString() != QStringLiteral("contact")) {
      m_contactOutcomeUnknown = true;
      m_contactShareError = QStringLiteral("The send result could not be verified. Check this conversation before retrying.");
      emit contactShareChanged();
      emit stateChanged();
      return;
    }
    const bool accepted = status == QStringLiteral("demo_sent") ||
                          status == QStringLiteral("accepted") ||
                          status == QStringLiteral("delivered") ||
                          status == QStringLiteral("read");
    if (!accepted) {
      m_contactOutcomeUnknown = true;
      m_contactShareError = QStringLiteral("Outcome is unknown. Check this conversation before retrying.");
      emit contactShareChanged();
      emit stateChanged();
      return;
    }
    if (generation == m_generation && chat == m_id)
      m_messages.mergeTail({message.toVariantMap()});
    if (serial == m_contactFileSerial) {
      m_contactPath.clear();
      m_contactCards.clear();
      m_contactSendId.clear();
      m_contactShareError.clear();
      m_contactOutcomeUnknown = false;
    }
    refreshChats();
    emit contactShareChanged();
    emit messagesChanged();
    emit tailUpdated();
    emit stateChanged();
  });
  return true;
}

void Controller::clearContactShare() {
  if (m_contactSending)
    return;
  ++m_contactFileSerial;
  m_contactPath.clear();
  m_contactCards.clear();
  m_contactSendId.clear();
  m_contactShareError.clear();
  m_contactPreparing = false;
  m_contactOutcomeUnknown = false;
  emit contactShareChanged();
}

bool Controller::sendLocation(const QVariantMap &location) {
  if (m_locationSending || m_locationOutcomeUnknown || m_id.isEmpty() ||
      !connected() || (!m_demo && m_connectionState != QStringLiteral("connected")) ||
      !hasCapability(QStringLiteral("message.location.send")))
    return false;
  double latitude = 0, longitude = 0;
  const auto caption = location.value(QStringLiteral("caption")).toString();
  const auto name = location.value(QStringLiteral("name")).toString();
  const auto address = location.value(QStringLiteral("address")).toString();
  if (!location.value(QStringLiteral("reviewed")).toBool() ||
      !parseLocationCoordinate(location.value(QStringLiteral("latitude")), -90, 90, &latitude) ||
      !parseLocationCoordinate(location.value(QStringLiteral("longitude")), -180, 180, &longitude) ||
      !validLocationText(caption) || !validLocationText(name) || !validLocationText(address)) {
    m_locationShareError = QStringLiteral("Review a finite coordinate pair within latitude −90…90 and longitude −180…180; optional text must be bounded and contain no control characters.");
    emit locationShareChanged();
    return false;
  }
  if (m_locationSendId.isEmpty())
    m_locationSendId = QStringLiteral("WF") + QUuid::createUuid().toString(QUuid::Id128).toUpper();
  const auto clientID = m_locationSendId;
  const auto chat = m_id;
  const int generation = m_generation;
  m_locationSending = true;
  m_locationShareAccepted = false;
  m_locationShareError.clear();
  emit locationShareChanged();
  request(QStringLiteral("message.location.send"),
          {{"chat_id", chat}, {"latitude", latitude}, {"longitude", longitude},
           {"caption", caption}, {"name", name}, {"address", address},
           {"reviewed", true}, {"client_id", clientID}},
          [this, chat, generation, clientID, latitude, longitude, caption, name, address](QJsonValue value, QJsonObject error) {
    m_locationSending = false;
    if (!error.isEmpty()) {
      const auto code = error.value(QStringLiteral("code")).toString();
      m_locationOutcomeUnknown = code == QStringLiteral("unknown_outcome") ||
                                 code == QStringLiteral("timeout") ||
                                 code == QStringLiteral("disconnected") ||
                                 code == QStringLiteral("app_closing");
      m_locationShareError = m_locationOutcomeUnknown
          ? QStringLiteral("Outcome is unknown. Check this conversation; this location will not be sent again automatically.")
          : error.value(QStringLiteral("message")).toString();
      if (m_locationOutcomeUnknown && generation == m_generation && chat == m_id) {
        fetchMessages(true);
        refreshChats();
      }
      emit locationShareChanged();
      emit stateChanged();
      return;
    }
    const auto message = value.toObject().value(QStringLiteral("message")).toObject();
    const auto sentLocation = message.value(QStringLiteral("location")).toObject();
    const auto status = message.value(QStringLiteral("status")).toString();
    const bool accepted = status == QStringLiteral("demo_sent") || status == QStringLiteral("accepted") ||
                          status == QStringLiteral("delivered") || status == QStringLiteral("read");
    if (message.value(QStringLiteral("id")).toString() != clientID ||
        message.value(QStringLiteral("chat_id")).toString() != chat ||
        message.value(QStringLiteral("kind")).toString() != QStringLiteral("location") ||
        !sentLocation.value(QStringLiteral("is_live")).isBool() ||
        sentLocation.value(QStringLiteral("is_live")).toBool() ||
        sentLocation.value(QStringLiteral("latitude")).toDouble() != latitude ||
        sentLocation.value(QStringLiteral("longitude")).toDouble() != longitude ||
        sentLocation.value(QStringLiteral("caption")).toString() != caption ||
        sentLocation.value(QStringLiteral("name")).toString() != name ||
        sentLocation.value(QStringLiteral("address")).toString() != address || !accepted) {
      m_locationOutcomeUnknown = true;
      m_locationShareError = QStringLiteral("The send result could not be verified. Check this conversation before retrying.");
      emit locationShareChanged();
      emit stateChanged();
      return;
    }
    if (generation == m_generation && chat == m_id)
      m_messages.mergeTail({message.toVariantMap()});
    m_locationShareAccepted = true;
    m_locationSendId.clear();
    m_locationShareError.clear();
    refreshChats();
    emit locationShareChanged();
    emit messagesChanged();
    emit tailUpdated();
    emit stateChanged();
  });
  return true;
}

void Controller::clearLocationShare() {
  if (m_locationSending)
    return;
  if (!m_locationOutcomeUnknown) {
    m_locationSendId.clear();
    m_locationShareError.clear();
  }
  m_locationShareAccepted = false;
  emit locationShareChanged();
}

void Controller::changeAttachmentMode(const QString &mode) {
  if (m_attachmentOriginalPath.isEmpty() || m_sending) return;
  stageAttachmentAs(QUrl::fromLocalFile(m_attachmentOriginalPath), mode);
}
void Controller::searchGifs(const QString &query, const QString &provider) {
  const auto q = query.trimmed().left(50);
  if (q.isEmpty()) return;
  const int serial = ++m_gifSerial; const int gen = m_generation;
  m_gifsLoading = true; m_gifError.clear(); m_gifProvider = provider; emit gifsChanged();
  request("gif.search", {{"query", q}, {"provider", provider}, {"offset", 0}},
    [this, serial, gen](QJsonValue value, QJsonObject error) {
      if (serial != m_gifSerial || gen != m_generation) return;
      m_gifsLoading = false;
      if (!error.isEmpty()) m_gifError = error.value("message").toString();
      else { auto o=value.toObject(); m_gifItems=o.value("items").toArray().toVariantList(); m_gifProvider=o.value("provider").toString(m_gifProvider); }
      emit gifsChanged();
    });
}
void Controller::prepareGif(const QString &id, const QString &provider) {
  if (m_id.isEmpty()) return;
  const int serial = ++m_attachmentSerial; const int gen = m_generation;
  m_attachmentPath.clear(); m_attachmentOriginalPath.clear(); m_attachmentSendId.clear();
  m_attachment = {{"name", "GIF"}, {"state", "preparing"}, {"mode", "original"}};
  emit attachmentChanged();
  request("gif.prepare", {{"id", id}, {"provider", provider}},
    [this, serial, gen](QJsonValue value, QJsonObject error) {
      if (serial != m_attachmentSerial || gen != m_generation) return;
      if (!error.isEmpty()) { m_error=error.value("message").toString(); m_attachment["state"]="error"; m_attachment["error"]=m_error; emit attachmentChanged(); emit stateChanged(); return; }
      m_attachment=value.toObject().toVariantMap(); m_attachment["state"]="ready";
      m_attachmentMode="original"; m_attachmentPath=m_attachment.value("prepared_path").toString(); m_attachmentOriginalPath=m_attachmentPath;
      emit attachmentChanged();
    });
}
void Controller::finishVoiceStaging(int serial) {
  if (!m_voiceStagePending || m_voiceAttachmentSerial != serial)
    return;
  m_voiceStagePending = false;
  m_voiceAttachmentSerial = -1;
  if (m_voiceDiscardPending) {
    m_voiceDiscardPending = false;
    m_voiceRecorder.discard();
  }
}

void Controller::releaseAlbumPaths(const QStringList &paths) {
  if (!m_demo || !connected() || paths.isEmpty() || paths.size() > 10 ||
      !hasCapability(QStringLiteral("attachment.album.release")))
    return;
  const auto stageRoot = QDir(m_dataDir).filePath(QStringLiteral("media/prepared"));
  QJsonArray jsonPaths;
  QSet<QString> seen;
  for (const auto &path : paths) {
    if (seen.contains(path) || !pathWithinDirectory(stageRoot, path))
      continue;
    const QFileInfo info(path);
    if (info.exists() && info.isSymLink())
      continue;
    seen.insert(path);
    jsonPaths.append(path);
  }
  if (!jsonPaths.isEmpty())
    request(QStringLiteral("attachment.album.release"),
            {{QStringLiteral("paths"), jsonPaths}},
            [](QJsonValue, QJsonObject) {});
}

void Controller::clearAttachment() {
  if (m_sending && m_attachment.value(QStringLiteral("kind")).toString() ==
                       QStringLiteral("album"))
    return;
  if (m_voiceRecorder.recording() || m_voiceRecorder.encoding()) {
    m_voiceRecorder.cancel();
  } else if (m_voiceStagePending) {
    m_voiceDiscardPending = true;
  } else {
    m_voiceRecorder.discard();
  }
  const auto albumPaths = m_albumPreparedPaths;
  m_albumPreparedPaths.clear();
  ++m_attachmentSerial;
  m_attachment.clear();
  m_attachmentPath.clear();
  m_attachmentOriginalPath.clear();
  m_attachmentSendId.clear();
  emit attachmentChanged();
  releaseAlbumPaths(albumPaths);
}
void Controller::sendAttachment() {
  if (m_attachment.value(QStringLiteral("kind")).toString() ==
      QStringLiteral("album")) {
    sendAlbum();
    return;
  }
  if (m_sending || m_id.isEmpty() || m_attachmentPath.isEmpty() || m_attachment.value("state").toString() != "ready")
    return;
  if (m_liveMode && !isActiveProfileMediaFile(m_attachmentPath)) {
    m_error = QStringLiteral("The staged file is no longer inside the verified encrypted profile.");
    m_attachment[QStringLiteral("state")] = QStringLiteral("error");
    m_attachment[QStringLiteral("error")] = m_error;
    emit attachmentChanged();
    emit stateChanged();
    return;
  }
  if (m_draft.toUcs4().size() > 16000) {
    m_error = "Caption exceeds 16000 characters";
    emit stateChanged();
    return;
  }
  m_sending = true;
  emit stateChanged();
  int gen = m_generation, version = m_draftVersion, serial = m_attachmentSerial;
  auto text = m_draft, path = m_attachmentPath;
  if (m_attachmentSendId.isEmpty())
    m_attachmentSendId =
        "WFM" + QUuid::createUuid().toString(QUuid::Id128).toUpper();
  QJsonObject params{{"chat_id", m_id},
                     {"path", path},
                     {"text", text},
                     {"mode", m_attachmentMode},
                     {"as_document", m_attachmentMode == "document"},
                     {"client_id", m_attachmentSendId}};
  if (m_attachmentMode == "audio" || m_attachmentMode == "voice-note")
    params.insert("prepared_path", path);
  request("message.attach", params,
          [this, gen, version, serial](QJsonValue value, QJsonObject error) {
            m_sending = false;
            if (!error.isEmpty()) {
              m_error = error.value("message").toString();
              emit stateChanged();
              return;
            }
            if (gen == m_generation) {
              auto message = value.toObject().value("message").toObject();
              if (!m_history)
                m_messages.mergeTail({message.toVariantMap()});
              const auto status = message.value("status").toString();
              const bool accepted = status == "demo_sent" || status == "accepted" ||
                                    status == "delivered" || status == "read";
              if (accepted && serial == m_attachmentSerial)
                clearAttachment();
              if (accepted && version == m_draftVersion) {
                m_draft.clear();
                ++m_draftVersion;
                emit draftChanged();
                persistDraft();
              }
              if (!accepted)
                m_error = "Attachment outcome is uncertain. Check history before retrying; the draft is preserved.";
              emit messagesChanged();
              emit tailUpdated();
            }
            refreshChats();
            emit stateChanged();
          });
}

void Controller::sendAlbum() {
  const auto items = QJsonArray::fromVariantList(
      m_attachment.value(QStringLiteral("items")).toList());
  if (m_sending || !m_demo || !connected() || m_id.isEmpty() ||
      m_attachment.value(QStringLiteral("kind")).toString() !=
          QStringLiteral("album") ||
      m_attachment.value(QStringLiteral("state")).toString() !=
          QStringLiteral("ready") || items.size() < 2 || items.size() > 10 ||
      m_albumPreparedPaths.size() != items.size() ||
      !hasCapability(QStringLiteral("message.album.send")))
    return;
  if (m_draft.toUcs4().size() > 16000) {
    m_error = QStringLiteral("Caption exceeds 16000 characters");
    m_attachment[QStringLiteral("error")] = m_error;
    emit attachmentChanged();
    emit stateChanged();
    return;
  }
  const auto stageRoot = QDir(m_dataDir).filePath(QStringLiteral("media/prepared"));
  for (const auto &path : m_albumPreparedPaths) {
    if (!privateRegularFile(stageRoot, path)) {
      m_error = QStringLiteral("A reviewed album image is no longer available. Re-select the images before sending.");
      m_attachment[QStringLiteral("state")] = QStringLiteral("error");
      m_attachment[QStringLiteral("error")] = m_error;
      emit attachmentChanged();
      emit stateChanged();
      return;
    }
  }

  if (m_attachmentSendId.isEmpty())
    m_attachmentSendId = QStringLiteral("WFA") +
                         QUuid::createUuid().toString(QUuid::Id128).toUpper();
  const auto clientId = m_attachmentSendId;
  const auto chat = m_id;
  const auto text = m_draft;
  const int generation = m_generation;
  const int version = m_draftVersion;
  const int serial = m_attachmentSerial;
  const auto preparedPaths = m_albumPreparedPaths;
  m_error.clear();
  m_attachment[QStringLiteral("state")] = QStringLiteral("sending");
  m_attachment.remove(QStringLiteral("error"));
  m_sending = true;
  emit attachmentChanged();
  emit stateChanged();

  QJsonArray jsonPaths;
  for (const auto &path : preparedPaths)
    jsonPaths.append(path);
  request(QStringLiteral("message.album.send"),
          {{QStringLiteral("chat_id"), chat},
           {QStringLiteral("prepared_paths"), jsonPaths},
           {QStringLiteral("text"), text},
           {QStringLiteral("client_id"), clientId}},
          [this, chat, text, clientId, generation, version, serial, items](
              QJsonValue value, QJsonObject error) {
    m_sending = false;
    if (!error.isEmpty()) {
      const auto code = error.value(QStringLiteral("code")).toString();
      const bool unknown = code == QStringLiteral("unknown_outcome") ||
                           code == QStringLiteral("timeout") ||
                           code == QStringLiteral("disconnected") ||
                           code == QStringLiteral("app_closing");
      m_error = unknown
                    ? QStringLiteral("Album outcome is unknown. Check this conversation before retrying; the same local request ID is retained.")
                    : error.value(QStringLiteral("message")).toString();
      m_attachment[QStringLiteral("state")] = QStringLiteral("ready");
      m_attachment[QStringLiteral("error")] = m_error;
      if (unknown && generation == m_generation && chat == m_id) {
        fetchMessages(true);
        refreshChats();
      }
      emit attachmentChanged();
      emit stateChanged();
      return;
    }

    const auto message = value.toObject().value(QStringLiteral("message")).toObject();
    const auto album = message.value(QStringLiteral("album")).toArray();
    bool valid = message.value(QStringLiteral("id")).toString() == clientId &&
                 message.value(QStringLiteral("chat_id")).toString() == chat &&
                 message.value(QStringLiteral("body")).toString() == text &&
                 message.value(QStringLiteral("outgoing")).toBool() &&
                 message.value(QStringLiteral("kind")).toString() ==
                     QStringLiteral("album") &&
                 message.value(QStringLiteral("status")).toString() ==
                     QStringLiteral("demo_sent") &&
                 album.size() == items.size() && album.size() >= 2 &&
                 album.size() <= 10 &&
                 QJsonDocument(message).toJson(QJsonDocument::Compact).size() <=
                     64 * 1024;
    qint64 total = 0;
    for (int i = 0; valid && i < album.size(); ++i) {
      qint64 itemSize = 0;
      if (!validSentAlbumItem(album.at(i).toObject(), m_dataDir,
                              items.at(i).toObject(), &itemSize) ||
          total + itemSize > 50 * 1024 * 1024) {
        valid = false;
        break;
      }
      total += itemSize;
    }
    if (!valid || total != m_attachment.value(QStringLiteral("total_prepared_size")).toLongLong()) {
      m_error = QStringLiteral("Album result could not be verified. Check this conversation before retrying.");
      m_attachment[QStringLiteral("state")] = QStringLiteral("ready");
      m_attachment[QStringLiteral("error")] = m_error;
      if (generation == m_generation && chat == m_id) {
        fetchMessages(true);
        refreshChats();
      }
      emit attachmentChanged();
      emit stateChanged();
      return;
    }

    if (generation == m_generation && chat == m_id) {
      if (!m_history)
        m_messages.mergeTail({message.toVariantMap()});
      else
        fetchMessages(true);
      if (serial == m_attachmentSerial)
        clearAttachment();
      if (version == m_draftVersion && m_draft == text) {
        m_draft.clear();
        ++m_draftVersion;
        emit draftChanged();
        persistDraft();
      }
      emit messagesChanged();
      emit tailUpdated();
    }
    refreshChats();
    emit stateChanged();
  });
}
bool Controller::isActiveProfileMediaFile(const QString &path) const {
  if (m_dataDir.isEmpty() || (m_liveMode && !m_profileViewReady))
    return false;
  const auto mediaRoot = QDir(m_dataDir).filePath(QStringLiteral("media"));
  if (!privateRegularFile(mediaRoot, path))
    return false;
  const auto absolutePath = QDir::cleanPath(QFileInfo(path).absoluteFilePath());
  const auto relativePath = QDir(m_dataDir).relativeFilePath(absolutePath);
  QString current = m_dataDir;
  const auto parts = relativePath.split(QDir::separator(), Qt::SkipEmptyParts);
  if (parts.size() < 2 || parts.first() != QStringLiteral("media"))
    return false;
  for (qsizetype i = 0; i < parts.size(); ++i) {
    current = QDir(current).filePath(parts.at(i));
    const QFileInfo component(current);
    if (!component.exists() || component.isSymLink() ||
        (i + 1 < parts.size() && !component.isDir()))
      return false;
  }
  return QFileInfo(current).isFile();
}

QVariant Controller::sanitizeProfileMediaUrls(const QVariant &value) const {
  std::function<QVariant(const QVariant &)> sanitize =
      [this, &sanitize](const QVariant &input) -> QVariant {
    if (input.metaType().id() == QMetaType::QVariantMap) {
      auto map = input.toMap();
      bool removedLocalMedia = false;
      for (auto it = map.begin(); it != map.end();) {
        const bool urlField = it.key() == QStringLiteral("url") ||
                              it.key() == QStringLiteral("thumbnail_url") ||
                              it.key() == QStringLiteral("preview_url");
        const QUrl url(it.value().toString());
        if (urlField && url.isLocalFile() &&
            !isActiveProfileMediaFile(url.toLocalFile())) {
          it = map.erase(it);
          removedLocalMedia = true;
          continue;
        }
        it.value() = sanitize(it.value());
        ++it;
      }
      if (removedLocalMedia &&
          map.value(QStringLiteral("state")).toString() == QStringLiteral("ready"))
        map.insert(QStringLiteral("state"), QStringLiteral("remote"));
      return map;
    }
    if (input.metaType().id() == QMetaType::QVariantList) {
      QVariantList list;
      const auto source = input.toList();
      list.reserve(source.size());
      for (const auto &item : source)
        list.append(sanitize(item));
      return list;
    }
    return input;
  };
  return sanitize(value);
}

void Controller::copyText(const QString &text) {
  if (auto *app = qobject_cast<QGuiApplication *>(QCoreApplication::instance());
      app && QGuiApplication::clipboard())
    QGuiApplication::clipboard()->setText(text);
}
void Controller::quoteMessage(const QVariantMap &message) {
  if (message.value("chat_id").toString() != m_id)
    return;
  auto text = message.value("body").toString();
  if (text.isEmpty())
    text = message.value("attachment").toMap().value("name").toString();
  if (text.size() > 2000)
    text = text.left(2000) + QStringLiteral("…");
  setDraft("> " + text.replace("\n", "\n> ") + "\n\n" + m_draft);
}
