#pragma once
#include <QAbstractListModel>
#include <QVariantMap>
class JsonModel : public QAbstractListModel {
  Q_OBJECT
public:
  explicit JsonModel(int limit = 500, QObject *parent = nullptr)
      : QAbstractListModel(parent), m_limit(limit) {}
  int rowCount(const QModelIndex &parent = {}) const override {
    return parent.isValid() ? 0 : m_rows.size();
  }
  QVariant data(const QModelIndex &i, int role) const override;
  QHash<int, QByteArray> roleNames() const override {
    return {{Qt::UserRole + 1, "record"}};
  }
  Q_INVOKABLE QVariantMap row(int index) const {
    return index >= 0 && index < m_rows.size() ? m_rows[index].toMap()
                                               : QVariantMap{};
  }
  void replace(const QVariantList &);
  void mergeTail(const QVariantList &);
  void updateRow(const QVariantMap &);
  void prepend(const QVariantList &);
  Q_INVOKABLE int indexOf(const QString &id) const;

private:
  QVariantList m_rows;
  int m_limit;
};
