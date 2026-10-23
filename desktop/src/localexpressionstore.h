#pragma once

#include <QList>
#include <QStringList>
#include <QVariantList>

class LocalExpressionStore {
public:
  explicit LocalExpressionStore(const QString &profileRoot = {});

  void setProfileRoot(const QString &profileRoot);
  bool load(QString *error = nullptr);

  QStringList recentEmojis() const { return m_recentEmojis; }
  QStringList favoriteEmojis() const { return m_favoriteEmojis; }
  QVariantList stickerPacks() const;

  bool recordRecentEmoji(const QString &emoji, QString *error = nullptr);
  bool toggleFavoriteEmoji(const QString &emoji, QString *error = nullptr);
  bool addStickerToPack(const QString &packName, const QString &stagedPath,
                        QString *error = nullptr,
                        const QString &displayName = {});
  QString resolveStickerPath(const QString &packId, const QString &stickerId,
                             QString *displayName = nullptr) const;

private:
  struct Sticker {
    QString id;
    QString name;
  };
  struct Pack {
    QString id;
    QString name;
    QList<Sticker> stickers;
  };

  bool persist(QString *error);
  bool ensureLayout(QString *error) const;
  QString preferencesPath() const;
  QString stickerPath(const QString &packId, const QString &stickerId) const;
  bool validateStickerFile(const QString &path, const QString &expectedId,
                            bool decode, QString *error) const;
  static bool validEmojiToken(const QString &emoji);
  static bool validPackName(const QString &name);
  static void setError(QString *error, const QString &message);

  QString m_profileRoot;
  QStringList m_recentEmojis;
  QStringList m_favoriteEmojis;
  QList<Pack> m_packs;
  bool m_healthy = true;
};
