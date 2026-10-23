#include "localexpressionstore.h"

#include <QCryptographicHash>
#include <QDir>
#include <QFile>
#include <QFileDevice>
#include <QFileInfo>
#include <QImageReader>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QRegularExpression>
#include <QSaveFile>
#include <QSet>
#include <QUrl>
#include <QUuid>

namespace {
constexpr int MaxRecentEmojis = 16;
constexpr int MaxFavoriteEmojis = 64;
constexpr int MaxEmojiCodeUnits = 16;
constexpr int MaxPacks = 16;
constexpr int MaxStickersPerPack = 32;
constexpr int MaxTotalStickers = 128;
constexpr qint64 MaxStickerBytes = 100000;
constexpr qint64 MaxTotalStickerBytes = 10 * 1024 * 1024;
constexpr qint64 MaxManifestBytes = 128 * 1024;
constexpr auto LocalDirectory = "local-expressions";
constexpr auto StickerDirectory = "sticker-packs";
constexpr auto PreferencesFile = "preferences.json";

QFileDevice::Permissions ownerDirectoryPermissions() {
  return QFileDevice::ReadOwner | QFileDevice::WriteOwner |
         QFileDevice::ExeOwner;
}

QFileDevice::Permissions ownerFilePermissions() {
  return QFileDevice::ReadOwner | QFileDevice::WriteOwner;
}

bool containsControl(const QString &text) {
  for (const auto character : text)
    if (character.category() == QChar::Other_Control)
      return true;
  return false;
}

bool validId(const QString &id, int length) {
  static const QRegularExpression expression(QStringLiteral("^[a-f0-9]+$"));
  return id.size() == length && expression.match(id).hasMatch();
}

bool isSafeDirectory(const QString &path) {
  const QFileInfo info(path);
  return info.exists() && info.isDir() && !info.isSymLink();
}
} // namespace

LocalExpressionStore::LocalExpressionStore(const QString &profileRoot) {
  setProfileRoot(profileRoot);
}

void LocalExpressionStore::setProfileRoot(const QString &profileRoot) {
  m_profileRoot = profileRoot.isEmpty()
                      ? QString()
                      : QDir::cleanPath(QFileInfo(profileRoot).absoluteFilePath());
  m_recentEmojis.clear();
  m_favoriteEmojis.clear();
  m_packs.clear();
  m_healthy = true;
}

void LocalExpressionStore::setError(QString *error, const QString &message) {
  if (error)
    *error = message;
}

bool LocalExpressionStore::validEmojiToken(const QString &emoji) {
  return !emoji.isEmpty() && emoji.size() <= MaxEmojiCodeUnits &&
         !emoji.trimmed().isEmpty() && !containsControl(emoji);
}

bool LocalExpressionStore::validPackName(const QString &name) {
  const auto normalized = name.trimmed();
  return !normalized.isEmpty() && normalized.size() <= 32 &&
         !containsControl(normalized) && !normalized.contains('/') &&
         !normalized.contains('\\');
}

QString LocalExpressionStore::preferencesPath() const {
  return QDir(m_profileRoot).filePath(QStringLiteral("%1/%2")
                                          .arg(QString::fromLatin1(LocalDirectory),
                                               QString::fromLatin1(PreferencesFile)));
}

QString LocalExpressionStore::stickerPath(const QString &packId,
                                          const QString &stickerId) const {
  return QDir(m_profileRoot)
      .filePath(QStringLiteral("%1/%2/%3/%4.webp")
                    .arg(QString::fromLatin1(LocalDirectory),
                         QString::fromLatin1(StickerDirectory), packId,
                         stickerId));
}

bool LocalExpressionStore::validateStickerFile(const QString &path,
                                               const QString &expectedId,
                                               bool decode,
                                               QString *error) const {
  const QFileInfo info(path);
  if (!info.isAbsolute() || !info.isFile() || info.isSymLink() ||
      info.size() <= 0 || info.size() > MaxStickerBytes) {
    setError(error, QStringLiteral("Sticker must be a regular local file up to 100,000 bytes."));
    return false;
  }

  QFile file(path);
  if (!file.open(QIODevice::ReadOnly)) {
    setError(error, QStringLiteral("Could not read the local sticker file."));
    return false;
  }
  const auto bytes = file.read(MaxStickerBytes + 1);
  if (bytes.isEmpty() || bytes.size() > MaxStickerBytes ||
      bytes.size() != info.size()) {
    setError(error, QStringLiteral("Sticker changed while it was being checked."));
    return false;
  }
  const auto digest = QCryptographicHash::hash(bytes, QCryptographicHash::Sha256)
                          .toHex();
  if (!expectedId.isEmpty() && digest != expectedId.toLatin1()) {
    setError(error, QStringLiteral("The saved sticker file no longer matches its local manifest."));
    return false;
  }

  QImageReader reader(path);
  if (!reader.canRead() || reader.format().toLower() != QByteArrayLiteral("webp") ||
      reader.size() != QSize(512, 512) || reader.imageCount() != 1) {
    setError(error, QStringLiteral("Only a single-frame 512 × 512 WebP sticker is supported."));
    return false;
  }
  if (decode && reader.read().isNull()) {
    setError(error, QStringLiteral("The local WebP sticker could not be decoded."));
    return false;
  }
  return true;
}

bool LocalExpressionStore::load(QString *error) {
  m_recentEmojis.clear();
  m_favoriteEmojis.clear();
  m_packs.clear();
  m_healthy = false;
  if (m_profileRoot.isEmpty()) {
    setError(error, QStringLiteral("No local profile is available for expression preferences."));
    return false;
  }

  const QFileInfo profileInfo(m_profileRoot);
  if (!profileInfo.exists()) {
    m_healthy = true;
    return true;
  }
  if (!profileInfo.isDir() || profileInfo.isSymLink()) {
    setError(error, QStringLiteral("The local profile path is not a regular directory."));
    return false;
  }

  const auto localDir = QDir(m_profileRoot).filePath(QString::fromLatin1(LocalDirectory));
  const QFileInfo localInfo(localDir);
  if (!localInfo.exists()) {
    m_healthy = true;
    return true;
  }
  if (!localInfo.isDir() || localInfo.isSymLink()) {
    setError(error, QStringLiteral("The local expression store directory is unsafe."));
    return false;
  }

  const auto manifestPath = preferencesPath();
  const QFileInfo manifestInfo(manifestPath);
  if (!manifestInfo.exists()) {
    m_healthy = true;
    return true;
  }
  if (!manifestInfo.isFile() || manifestInfo.isSymLink() ||
      manifestInfo.size() <= 0 || manifestInfo.size() > MaxManifestBytes) {
    setError(error, QStringLiteral("The local expression manifest is invalid or too large."));
    return false;
  }

  QFile manifest(manifestPath);
  if (!manifest.open(QIODevice::ReadOnly)) {
    setError(error, QStringLiteral("Could not read the local expression manifest."));
    return false;
  }
  QJsonParseError parseError;
  const auto document = QJsonDocument::fromJson(manifest.readAll(), &parseError);
  if (parseError.error != QJsonParseError::NoError || !document.isObject()) {
    setError(error, QStringLiteral("The local expression manifest is malformed."));
    return false;
  }
  const auto root = document.object();
  if (root.value(QStringLiteral("version")).toInt(-1) != 1) {
    setError(error, QStringLiteral("The local expression manifest version is unsupported."));
    return false;
  }

  auto parseEmojiList = [&](const QString &key, int maximum,
                            QStringList *target) {
    const auto value = root.value(key);
    if (!value.isArray() || value.toArray().size() > maximum)
      return false;
    QSet<QString> seen;
    for (const auto entry : value.toArray()) {
      if (!entry.isString())
        return false;
      const auto emoji = entry.toString();
      if (!validEmojiToken(emoji) || seen.contains(emoji))
        return false;
      seen.insert(emoji);
      target->append(emoji);
    }
    return true;
  };

  if (!parseEmojiList(QStringLiteral("recent_emojis"), MaxRecentEmojis,
                      &m_recentEmojis) ||
      !parseEmojiList(QStringLiteral("favorite_emojis"), MaxFavoriteEmojis,
                      &m_favoriteEmojis)) {
    m_recentEmojis.clear();
    m_favoriteEmojis.clear();
    setError(error, QStringLiteral("Stored emoji preferences failed validation."));
    return false;
  }

  const auto packsValue = root.value(QStringLiteral("sticker_packs"));
  if (!packsValue.isArray() || packsValue.toArray().size() > MaxPacks) {
    m_recentEmojis.clear();
    m_favoriteEmojis.clear();
    setError(error, QStringLiteral("Stored sticker pack list exceeds its limit."));
    return false;
  }

  const auto stickerRootPath =
      QDir(localDir).filePath(QString::fromLatin1(StickerDirectory));
  const QFileInfo stickerRootInfo(stickerRootPath);
  if (((stickerRootInfo.exists() || stickerRootInfo.isSymLink()) &&
       (!stickerRootInfo.isDir() || stickerRootInfo.isSymLink())) ||
      (!packsValue.toArray().isEmpty() && !isSafeDirectory(stickerRootPath))) {
    setError(error, QStringLiteral("Stored sticker pack root directory is missing or unsafe."));
    m_recentEmojis.clear();
    m_favoriteEmojis.clear();
    m_packs.clear();
    return false;
  }

  QSet<QString> packIds;
  QSet<QString> packNames;
  int totalStickers = 0;
  qint64 totalBytes = 0;
  for (const auto packValue : packsValue.toArray()) {
    if (!packValue.isObject()) {
      setError(error, QStringLiteral("A stored sticker pack is malformed."));
      m_recentEmojis.clear();
      m_favoriteEmojis.clear();
      m_packs.clear();
      return false;
    }
    const auto packObject = packValue.toObject();
    Pack pack;
    pack.id = packObject.value(QStringLiteral("id")).toString();
    pack.name = packObject.value(QStringLiteral("name")).toString();
    const auto stickersValue = packObject.value(QStringLiteral("stickers"));
    const auto foldedName = pack.name.toCaseFolded();
    if (!validId(pack.id, 32) || packIds.contains(pack.id) ||
        !validPackName(pack.name) || packNames.contains(foldedName) ||
        !stickersValue.isArray() ||
        stickersValue.toArray().size() > MaxStickersPerPack) {
      setError(error, QStringLiteral("Stored sticker pack metadata failed validation."));
      m_recentEmojis.clear();
      m_favoriteEmojis.clear();
      m_packs.clear();
      return false;
    }
    const auto packDirPath = QDir(stickerRootPath).filePath(pack.id);
    if (!isSafeDirectory(packDirPath)) {
      setError(error, QStringLiteral("Stored sticker pack directory is missing or unsafe."));
      m_recentEmojis.clear();
      m_favoriteEmojis.clear();
      m_packs.clear();
      return false;
    }
    packIds.insert(pack.id);
    packNames.insert(foldedName);

    QSet<QString> stickerIds;
    for (const auto stickerValue : stickersValue.toArray()) {
      if (!stickerValue.isObject()) {
        setError(error, QStringLiteral("Stored sticker metadata is malformed."));
        m_recentEmojis.clear();
        m_favoriteEmojis.clear();
        m_packs.clear();
        return false;
      }
      const auto stickerObject = stickerValue.toObject();
      Sticker sticker{stickerObject.value(QStringLiteral("id")).toString(),
                      stickerObject.value(QStringLiteral("name")).toString()};
      if (!validId(sticker.id, 64) || stickerIds.contains(sticker.id) ||
          sticker.name.isEmpty() || sticker.name.size() > 96 ||
          containsControl(sticker.name) || ++totalStickers > MaxTotalStickers) {
        setError(error, QStringLiteral("Stored sticker metadata failed validation."));
        m_recentEmojis.clear();
        m_favoriteEmojis.clear();
        m_packs.clear();
        return false;
      }
      const auto path = stickerPath(pack.id, sticker.id);
      if (!validateStickerFile(path, sticker.id, false, error)) {
        m_recentEmojis.clear();
        m_favoriteEmojis.clear();
        m_packs.clear();
        return false;
      }
      totalBytes += QFileInfo(path).size();
      if (totalBytes > MaxTotalStickerBytes) {
        setError(error, QStringLiteral("Stored sticker packs exceed the local size limit."));
        m_recentEmojis.clear();
        m_favoriteEmojis.clear();
        m_packs.clear();
        return false;
      }
      stickerIds.insert(sticker.id);
      pack.stickers.append(sticker);
    }
    m_packs.append(pack);
  }

  m_healthy = true;
  return true;
}

bool LocalExpressionStore::ensureLayout(QString *error) const {
  if (m_profileRoot.isEmpty()) {
    setError(error, QStringLiteral("No local profile is available for expression preferences."));
    return false;
  }
  QFileInfo profileInfo(m_profileRoot);
  if (!profileInfo.exists() && !QDir().mkpath(m_profileRoot)) {
    setError(error, QStringLiteral("Could not create the local profile directory."));
    return false;
  }
  profileInfo.setFile(m_profileRoot);
  if (!profileInfo.isDir() || profileInfo.isSymLink()) {
    setError(error, QStringLiteral("The local profile path is not a regular directory."));
    return false;
  }

  const auto localDir = QDir(m_profileRoot).filePath(QString::fromLatin1(LocalDirectory));
  QFileInfo localInfo(localDir);
  if (!localInfo.exists() && !QDir().mkpath(localDir)) {
    setError(error, QStringLiteral("Could not create the local expression store directory."));
    return false;
  }
  localInfo.setFile(localDir);
  if (!localInfo.isDir() || localInfo.isSymLink() ||
      !QFile::setPermissions(localDir, ownerDirectoryPermissions())) {
    setError(error, QStringLiteral("The local expression store directory is unsafe."));
    return false;
  }

  const auto stickerDir = QDir(localDir).filePath(QString::fromLatin1(StickerDirectory));
  QFileInfo stickerInfo(stickerDir);
  if (!stickerInfo.exists() && !QDir().mkpath(stickerDir)) {
    setError(error, QStringLiteral("Could not create the local sticker pack directory."));
    return false;
  }
  stickerInfo.setFile(stickerDir);
  if (!stickerInfo.isDir() || stickerInfo.isSymLink() ||
      !QFile::setPermissions(stickerDir, ownerDirectoryPermissions())) {
    setError(error, QStringLiteral("The local sticker pack directory is unsafe."));
    return false;
  }
  return true;
}

bool LocalExpressionStore::persist(QString *error) {
  if (!m_healthy) {
    setError(error, QStringLiteral("The local expression store is read-only after a validation error."));
    return false;
  }
  if (!ensureLayout(error))
    return false;

  QJsonArray recent;
  for (const auto &emoji : m_recentEmojis)
    recent.append(emoji);
  QJsonArray favorites;
  for (const auto &emoji : m_favoriteEmojis)
    favorites.append(emoji);
  QJsonArray packs;
  for (const auto &pack : m_packs) {
    QJsonArray stickers;
    for (const auto &sticker : pack.stickers)
      stickers.append(QJsonObject{{QStringLiteral("id"), sticker.id},
                                  {QStringLiteral("name"), sticker.name}});
    packs.append(QJsonObject{{QStringLiteral("id"), pack.id},
                             {QStringLiteral("name"), pack.name},
                             {QStringLiteral("stickers"), stickers}});
  }
  const auto bytes = QJsonDocument(QJsonObject{
      {QStringLiteral("version"), 1},
      {QStringLiteral("recent_emojis"), recent},
      {QStringLiteral("favorite_emojis"), favorites},
      {QStringLiteral("sticker_packs"), packs}})
                         .toJson(QJsonDocument::Compact);
  if (bytes.size() > MaxManifestBytes) {
    setError(error, QStringLiteral("The local expression manifest exceeds its size limit."));
    return false;
  }

  const auto path = preferencesPath();
  const QFileInfo existing(path);
  if (existing.exists() && (!existing.isFile() || existing.isSymLink())) {
    setError(error, QStringLiteral("The local expression manifest path is unsafe."));
    return false;
  }
  QSaveFile file(path);
  file.setDirectWriteFallback(false);
  if (!file.open(QIODevice::WriteOnly) || file.write(bytes) != bytes.size() ||
      !file.commit() || !QFile::setPermissions(path, ownerFilePermissions())) {
    setError(error, QStringLiteral("Could not safely save local expression preferences."));
    return false;
  }
  return true;
}

bool LocalExpressionStore::recordRecentEmoji(const QString &emoji,
                                            QString *error) {
  if (!m_healthy || !validEmojiToken(emoji)) {
    setError(error, QStringLiteral("Choose one supported emoji to update local recents."));
    return false;
  }
  const auto previous = m_recentEmojis;
  m_recentEmojis.removeAll(emoji);
  m_recentEmojis.prepend(emoji);
  while (m_recentEmojis.size() > MaxRecentEmojis)
    m_recentEmojis.removeLast();
  if (m_recentEmojis == previous)
    return true;
  if (persist(error))
    return true;
  m_recentEmojis = previous;
  return false;
}

bool LocalExpressionStore::toggleFavoriteEmoji(const QString &emoji,
                                               QString *error) {
  if (!m_healthy || !validEmojiToken(emoji)) {
    setError(error, QStringLiteral("Choose one supported emoji to update local favorites."));
    return false;
  }
  const auto previous = m_favoriteEmojis;
  if (m_favoriteEmojis.contains(emoji)) {
    m_favoriteEmojis.removeAll(emoji);
  } else {
    if (m_favoriteEmojis.size() >= MaxFavoriteEmojis) {
      setError(error, QStringLiteral("The local favorite emoji limit has been reached."));
      return false;
    }
    m_favoriteEmojis.append(emoji);
  }
  if (persist(error))
    return true;
  m_favoriteEmojis = previous;
  return false;
}

QVariantList LocalExpressionStore::stickerPacks() const {
  QVariantList result;
  for (const auto &pack : m_packs) {
    QVariantList stickers;
    for (const auto &sticker : pack.stickers) {
      const auto path = resolveStickerPath(pack.id, sticker.id);
      if (path.isEmpty())
        continue;
      stickers.append(QVariantMap{{QStringLiteral("id"), sticker.id},
                                  {QStringLiteral("name"), sticker.name},
                                  {QStringLiteral("url"), QUrl::fromLocalFile(path).toString()}});
    }
    result.append(QVariantMap{{QStringLiteral("id"), pack.id},
                              {QStringLiteral("name"), pack.name},
                              {QStringLiteral("count"), stickers.size()},
                              {QStringLiteral("stickers"), stickers}});
  }
  return result;
}

QString LocalExpressionStore::resolveStickerPath(const QString &packId,
                                                 const QString &stickerId,
                                                 QString *displayName) const {
  if (displayName)
    displayName->clear();
  if (!validId(packId, 32) || !validId(stickerId, 64))
    return {};
  const auto localDirPath =
      QDir(m_profileRoot).filePath(QString::fromLatin1(LocalDirectory));
  const auto stickerRootPath =
      QDir(localDirPath).filePath(QString::fromLatin1(StickerDirectory));
  if (!isSafeDirectory(m_profileRoot) || !isSafeDirectory(localDirPath) ||
      !isSafeDirectory(stickerRootPath))
    return {};
  for (const auto &pack : m_packs) {
    if (pack.id != packId)
      continue;
    if (!isSafeDirectory(QDir(stickerRootPath).filePath(pack.id)))
      return {};
    for (const auto &sticker : pack.stickers) {
      if (sticker.id != stickerId)
        continue;
      const auto path = stickerPath(packId, stickerId);
      const QFileInfo info(path);
      if (!info.isFile() || info.isSymLink() || info.size() <= 0 ||
          info.size() > MaxStickerBytes)
        return {};
      if (displayName)
        *displayName = sticker.name;
      return info.absoluteFilePath();
    }
  }
  return {};
}

bool LocalExpressionStore::addStickerToPack(const QString &packName,
                                            const QString &stagedPath,
                                            QString *error,
                                            const QString &displayName) {
  const auto name = packName.trimmed();
  if (!m_healthy || !validPackName(name)) {
    setError(error, QStringLiteral("Enter a pack name up to 32 characters without path separators."));
    return false;
  }
  if (!validateStickerFile(stagedPath, QString(), true, error))
    return false;

  QFile source(stagedPath);
  if (!source.open(QIODevice::ReadOnly)) {
    setError(error, QStringLiteral("Could not read the staged sticker."));
    return false;
  }
  const auto bytes = source.read(MaxStickerBytes + 1);
  if (bytes.isEmpty() || bytes.size() > MaxStickerBytes ||
      bytes.size() != QFileInfo(stagedPath).size()) {
    setError(error, QStringLiteral("The staged sticker changed while it was copied."));
    return false;
  }
  const auto stickerId = QString::fromLatin1(
      QCryptographicHash::hash(bytes, QCryptographicHash::Sha256).toHex());

  int packIndex = -1;
  for (int index = 0; index < m_packs.size(); ++index) {
    if (m_packs.at(index).name.compare(name, Qt::CaseInsensitive) == 0) {
      packIndex = index;
      break;
    }
  }
  const bool createPack = packIndex < 0;
  if (createPack && m_packs.size() >= MaxPacks) {
    setError(error, QStringLiteral("The local sticker pack limit has been reached."));
    return false;
  }
  if (!createPack && m_packs.at(packIndex).stickers.size() >= MaxStickersPerPack) {
    setError(error, QStringLiteral("This local sticker pack is full."));
    return false;
  }
  if (!createPack) {
    for (const auto &existing : m_packs.at(packIndex).stickers)
      if (existing.id == stickerId)
        return true;
  }

  int totalStickers = 0;
  qint64 totalBytes = 0;
  for (const auto &pack : m_packs) {
    for (const auto &sticker : pack.stickers) {
      ++totalStickers;
      totalBytes += QFileInfo(stickerPath(pack.id, sticker.id)).size();
    }
  }
  if (totalStickers >= MaxTotalStickers ||
      totalBytes + bytes.size() > MaxTotalStickerBytes) {
    setError(error, QStringLiteral("Local sticker packs have reached their storage limit."));
    return false;
  }

  Pack pack;
  if (createPack) {
    pack.id = QUuid::createUuid().toString(QUuid::Id128);
    pack.name = name;
  } else {
    pack = m_packs.at(packIndex);
  }
  if (!ensureLayout(error))
    return false;
  const auto stickerRootPath =
      QDir(m_profileRoot)
          .filePath(QStringLiteral("%1/%2")
                       .arg(QString::fromLatin1(LocalDirectory),
                            QString::fromLatin1(StickerDirectory)));
  if (!isSafeDirectory(stickerRootPath)) {
    setError(error, QStringLiteral("The local sticker pack directory is unsafe."));
    return false;
  }
  const auto packDir = QDir(stickerRootPath).filePath(pack.id);
  QFileInfo packDirInfo(packDir);
  if (packDirInfo.isSymLink() ||
      (packDirInfo.exists() && !packDirInfo.isDir()) ||
      (!packDirInfo.exists() && !QDir().mkpath(packDir))) {
    setError(error, QStringLiteral("Could not safely create the local sticker pack directory."));
    return false;
  }
  packDirInfo.setFile(packDir);
  if (!isSafeDirectory(packDir) ||
      !QFile::setPermissions(packDir, ownerDirectoryPermissions())) {
    setError(error, QStringLiteral("Could not safely create the local sticker pack directory."));
    return false;
  }

  const auto targetPath = stickerPath(pack.id, stickerId);
  bool createdCopy = false;
  const QFileInfo targetInfo(targetPath);
  if (targetInfo.exists() || targetInfo.isSymLink()) {
    if (!validateStickerFile(targetPath, stickerId, false, error))
      return false;
  } else {
    QSaveFile target(targetPath);
    target.setDirectWriteFallback(false);
    if (!target.open(QIODevice::WriteOnly) || target.write(bytes) != bytes.size() ||
        !target.commit() || !QFile::setPermissions(targetPath, ownerFilePermissions())) {
      setError(error, QStringLiteral("Could not safely copy the sticker into its local pack."));
      return false;
    }
    createdCopy = true;
  }

  auto safeDisplayName = displayName.isEmpty()
                             ? QFileInfo(stagedPath).fileName()
                             : displayName;
  if (safeDisplayName.isEmpty() || safeDisplayName.size() > 96 ||
      containsControl(safeDisplayName))
    safeDisplayName = QStringLiteral("Sticker");
  pack.stickers.append(Sticker{stickerId, safeDisplayName});
  const auto previousPacks = m_packs;
  if (createPack)
    m_packs.append(pack);
  else
    m_packs[packIndex] = pack;
  if (persist(error))
    return true;

  m_packs = previousPacks;
  if (createdCopy)
    QFile::remove(targetPath);
  return false;
}
