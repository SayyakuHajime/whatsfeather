#include "jsonmodel.h"
#include <algorithm>

namespace {
QVariantList chronological(QVariantList rows) {
  bool sortable = !rows.isEmpty();
  for (const auto &value : rows) {
    // Chat rows also carry timestamps, but their backend order is the
    // conversation-list order rather than a timeline. Only message-shaped
    // rows are normalized here.
    if (value.toMap().value("chat_id").toString().isEmpty() ||
        value.toMap().value("timestamp").toLongLong() <= 0) {
      sortable = false;
      break;
    }
  }
  if (!sortable)
    return rows;
  std::stable_sort(rows.begin(), rows.end(), [](const QVariant &a, const QVariant &b) {
    const auto left = a.toMap();
    const auto right = b.toMap();
    const auto lt = left.value("timestamp").toLongLong();
    const auto rt = right.value("timestamp").toLongLong();
    if (lt != rt)
      return lt < rt;
    return left.value("id").toString() < right.value("id").toString();
  });
  return rows;
}
} // namespace

QVariant JsonModel::data(const QModelIndex &i, int role) const {
  if (role != Qt::UserRole + 1 || !i.isValid() || i.row() < 0 ||
      i.row() >= m_rows.size())
    return {};
  return m_rows[i.row()];
}
int JsonModel::indexOf(const QString &id) const {
  for (int i = 0; i < m_rows.size(); ++i)
    if (row(i).value("id").toString() == id)
      return i;
  return -1;
}
void JsonModel::updateRow(const QVariantMap &record) {
  const auto id = record.value("id").toString();
  if (id.isEmpty())
    return;
  const int rowIndex = indexOf(id);
  if (rowIndex < 0)
    return;
  m_rows[rowIndex] = record;
  const auto modelIndex = index(rowIndex, 0);
  emit dataChanged(modelIndex, modelIndex, {Qt::UserRole + 1});
}
void JsonModel::replace(const QVariantList &rows) {
  beginResetModel();
  m_rows = chronological(rows);
  if (m_rows.size() > m_limit)
    m_rows = m_rows.mid(m_rows.size() - m_limit);
  endResetModel();
}
void JsonModel::mergeTail(const QVariantList &rows) {
  QVariantList merged = m_rows;
  for (const auto &v : rows) {
    const auto map = v.toMap();
    const int found = indexOf(map.value("id").toString());
    if (found >= 0)
      merged[found] = v;
    else
      merged.append(v);
  }
  auto sorted = chronological(merged);
  if (sorted.size() > m_limit)
    sorted = sorted.mid(sorted.size() - m_limit);
  if (sorted != m_rows) {
    beginResetModel();
    m_rows = sorted;
    endResetModel();
  }
}
void JsonModel::prepend(const QVariantList &rows) {
  QVariantList combined;
  for (const auto &v : rows)
    if (indexOf(v.toMap().value("id").toString()) < 0)
      combined.append(v);
  combined.append(m_rows);
  combined = chronological(combined);
  if (combined.size() > m_limit)
    combined = combined.mid(0, m_limit);
  beginResetModel();
  m_rows = combined;
  endResetModel();
}
