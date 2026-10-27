#include "legaldocuments.h"

#include <QFile>
#include <QString>

namespace {
constexpr qint64 kMaximumDocumentBytes = 1024 * 1024;

struct DocumentEntry {
  QString id;
  QString title;
  QString resource;
};

const DocumentEntry kDocuments[] = {
    {QStringLiteral("project-license"),
     QStringLiteral("Project license — GPL-3.0-only"),
     QStringLiteral(":/legal/LICENSE")},
    {QStringLiteral("asset-licenses"),
     QStringLiteral("Asset license and provenance"),
     QStringLiteral(":/legal/ASSET-LICENSES.md")},
    {QStringLiteral("third-party-notices"),
     QStringLiteral("Third-party notices"),
     QStringLiteral(":/legal/THIRD-PARTY-NOTICES.md")},
};

QVariantMap unavailable(const QString &message) {
  return {{QStringLiteral("ok"), false},
          {QStringLiteral("title"), QStringLiteral("Legal information")},
          {QStringLiteral("text"), QString()},
          {QStringLiteral("error"), message},
          {QStringLiteral("byteCount"), 0}};
}
} // namespace

LegalDocuments::LegalDocuments(QObject *parent) : QObject(parent) {}

QVariantMap LegalDocuments::load(const QString &documentId) const {
  const DocumentEntry *entry = nullptr;
  for (const auto &candidate : kDocuments) {
    if (documentId == candidate.id) {
      entry = &candidate;
      break;
    }
  }
  if (!entry)
    return unavailable(QStringLiteral("This legal document is not available."));

  QFile resource(entry->resource);
  if (!resource.open(QIODevice::ReadOnly))
    return unavailable(QStringLiteral(
        "The legal document could not be loaded from the application."));

  const QByteArray bytes = resource.read(kMaximumDocumentBytes + 1);
  if (resource.error() != QFileDevice::NoError || bytes.isEmpty() ||
      bytes.size() > kMaximumDocumentBytes)
    return unavailable(QStringLiteral(
        "The legal document could not be loaded from the application."));

  const QString text = QString::fromUtf8(bytes.constData(), bytes.size());
  if (text.toUtf8() != bytes)
    return unavailable(QStringLiteral(
        "The legal document could not be loaded from the application."));

  return {{QStringLiteral("ok"), true},
          {QStringLiteral("id"), documentId},
          {QStringLiteral("title"), entry->title},
          {QStringLiteral("text"), text},
          {QStringLiteral("error"), QString()},
          {QStringLiteral("byteCount"), bytes.size()}};
}
