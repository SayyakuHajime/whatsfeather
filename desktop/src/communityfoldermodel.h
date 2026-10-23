#pragma once
#include <QAbstractListModel>
#include <QPointer>
#include <QVariantMap>

// A bounded presentation projection. Real chat IDs remain unchanged; folders
// never become conversations and never alter remote membership/read state.
class CommunityFolderModel : public QAbstractListModel {
  Q_OBJECT
  Q_PROPERTY(QAbstractItemModel *sourceModel READ sourceModel WRITE setSourceModel NOTIFY sourceChanged)
  Q_PROPERTY(QString query READ query WRITE setQuery NOTIFY optionsChanged)
  Q_PROPERTY(bool groupsOnly READ groupsOnly WRITE setGroupsOnly NOTIFY optionsChanged)
  Q_PROPERTY(QString profileKey READ profileKey WRITE setProfileKey NOTIFY optionsChanged)
public:
  explicit CommunityFolderModel(QObject *parent=nullptr):QAbstractListModel(parent){}
  int rowCount(const QModelIndex &p={}) const override {return p.isValid()?0:m_rows.size();}
  QVariant data(const QModelIndex &,int role) const override;
  QHash<int,QByteArray> roleNames() const override {return {{Qt::UserRole+1,"record"}};}
  Q_INVOKABLE QVariantMap row(int i) const {return i>=0&&i<m_rows.size()?m_rows[i].toMap():QVariantMap{};}
  QAbstractItemModel *sourceModel() const {return m_source;}
  void setSourceModel(QAbstractItemModel *);
  QString query() const {return m_query;}
  void setQuery(const QString &);
  bool groupsOnly() const {return m_groupsOnly;}
  void setGroupsOnly(bool);
  QString profileKey() const {return m_profile;}
  void setProfileKey(const QString &);
  Q_INVOKABLE bool expanded(const QString &) const;
  Q_INVOKABLE void toggleFolder(const QString &);
signals:
  void sourceChanged();
  void optionsChanged();
private:
  void rebuild();
  QString preferenceKey(const QString &) const;
  QPointer<QAbstractItemModel> m_source;
  QList<QMetaObject::Connection> m_connections;
  QVariantList m_rows;
  QHash<QString,bool> m_expanded;
  QString m_query,m_profile;
  bool m_groupsOnly=false;
};
