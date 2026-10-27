#include "controller.h"

#include <QFileInfo>

bool Controller::recordRecentEmoji(const QString &emoji) {
  if (m_liveMode && !m_profileViewReady) {
    m_expressionStoreError = QStringLiteral("Encrypted profile storage is not ready; the emoji was not saved.");
    emit expressionsChanged();
    return false;
  }
  QString error;
  const bool saved = m_expressionStore.recordRecentEmoji(emoji, &error);
  m_expressionStoreError = saved ? QString() : error;
  emit expressionsChanged();
  return saved;
}

bool Controller::toggleFavoriteEmoji(const QString &emoji) {
  if (m_liveMode && !m_profileViewReady) {
    m_expressionStoreError = QStringLiteral("Encrypted profile storage is not ready; the emoji was not saved.");
    emit expressionsChanged();
    return false;
  }
  QString error;
  const bool saved = m_expressionStore.toggleFavoriteEmoji(emoji, &error);
  m_expressionStoreError = saved ? QString() : error;
  emit expressionsChanged();
  return saved;
}

bool Controller::addCurrentStickerToPack(const QString &packName) {
  if (m_liveMode && !m_profileViewReady) {
    m_expressionStoreError = QStringLiteral("Encrypted profile storage is not ready; the sticker was not saved.");
    emit expressionsChanged();
    return false;
  }
  const auto meta = m_attachment;
  if (meta.value(QStringLiteral("state")).toString() != QStringLiteral("ready") ||
      meta.value(QStringLiteral("kind")).toString() != QStringLiteral("sticker") ||
      meta.value(QStringLiteral("mime")).toString() != QStringLiteral("image/webp") ||
      meta.value(QStringLiteral("width")).toInt() != 512 ||
      meta.value(QStringLiteral("height")).toInt() != 512 ||
      meta.value(QStringLiteral("size")).toLongLong() <= 0 ||
      meta.value(QStringLiteral("size")).toLongLong() > 100000 ||
      m_attachmentMode != QStringLiteral("sticker") || m_attachmentPath.isEmpty()) {
    m_expressionStoreError = QStringLiteral("Review a ready static WebP sticker before saving it to a local pack.");
    emit expressionsChanged();
    return false;
  }

  const auto displayName = meta.value(QStringLiteral("display_name")).toString().isEmpty()
                               ? QFileInfo(m_attachmentOriginalPath).fileName()
                               : meta.value(QStringLiteral("display_name")).toString();
  QString error;
  const bool saved = m_expressionStore.addStickerToPack(
      packName, m_attachmentPath, &error, displayName);
  m_expressionStoreError = saved ? QString() : error;
  emit expressionsChanged();
  return saved;
}

bool Controller::stageLocalSticker(const QString &packId,
                                   const QString &stickerId) {
  const auto features = m_capabilities.value(QStringLiteral("features")).toMap();
  const auto stickers = features.value(QStringLiteral("stickers")).toMap();
  if ((m_liveMode && !m_profileViewReady) || !connected() || m_id.isEmpty() || !m_capabilitiesReady ||
      stickers.value(QStringLiteral("available")).toBool() != true ||
      !hasCapability(QStringLiteral("attachment.prepare"))) {
    m_expressionStoreError = QStringLiteral("Local sticker review is unavailable until a conversation and static-sticker support are ready.");
    emit expressionsChanged();
    return false;
  }

  QString displayName;
  const auto path =
      m_expressionStore.resolveStickerPath(packId, stickerId, &displayName);
  if (path.isEmpty()) {
    m_expressionStoreError = QStringLiteral("That local sticker is no longer available in this profile.");
    emit expressionsChanged();
    return false;
  }
  m_expressionStoreError.clear();
  emit expressionsChanged();
  stageAttachmentAsWithDisplayName(QUrl::fromLocalFile(path),
                                   QStringLiteral("sticker"), displayName);
  return true;
}
