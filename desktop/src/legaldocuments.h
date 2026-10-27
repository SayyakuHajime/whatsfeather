#pragma once

#include <QObject>
#include <QVariantMap>

class LegalDocuments final : public QObject {
  Q_OBJECT

public:
  explicit LegalDocuments(QObject *parent = nullptr);

  Q_INVOKABLE QVariantMap load(const QString &documentId) const;
};
