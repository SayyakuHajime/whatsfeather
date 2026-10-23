#include "communityfoldermodel.h"
#include <QCryptographicHash>
#include <QSettings>
#include <QFile>
#include <QSet>
#include <algorithm>
#include <limits>

QVariant CommunityFolderModel::data(const QModelIndex &i,int role) const {
  return role==Qt::UserRole+1&&i.isValid()&&i.row()>=0&&i.row()<m_rows.size()?m_rows[i.row()]:QVariant{};
}
void CommunityFolderModel::setSourceModel(QAbstractItemModel *source) {
  if(source==this||source==m_source)return;
  for(const auto &c:m_connections)disconnect(c);
  m_connections.clear();m_source=source;
  if(source){
    m_connections << connect(source,&QAbstractItemModel::modelReset,this,&CommunityFolderModel::rebuild)
      << connect(source,&QAbstractItemModel::rowsInserted,this,&CommunityFolderModel::rebuild)
      << connect(source,&QAbstractItemModel::rowsRemoved,this,&CommunityFolderModel::rebuild)
      << connect(source,&QAbstractItemModel::rowsMoved,this,&CommunityFolderModel::rebuild)
      << connect(source,&QAbstractItemModel::dataChanged,this,&CommunityFolderModel::rebuild)
      << connect(source,&QAbstractItemModel::layoutChanged,this,&CommunityFolderModel::rebuild)
      << connect(source,&QObject::destroyed,this,[this]{m_source=nullptr;rebuild();emit sourceChanged();});
  }
  rebuild();emit sourceChanged();
}
void CommunityFolderModel::setQuery(const QString &value){if(m_query==value.trimmed())return;m_query=value.trimmed();rebuild();emit optionsChanged();}
void CommunityFolderModel::setGroupsOnly(bool value){if(m_groupsOnly==value)return;m_groupsOnly=value;rebuild();emit optionsChanged();}
void CommunityFolderModel::setProfileKey(const QString &value){if(m_profile==value)return;m_profile=value;m_expanded.clear();rebuild();emit optionsChanged();}
QString CommunityFolderModel::preferenceKey(const QString &id) const {
  auto hash=[](const QString &v){return QString::fromLatin1(QCryptographicHash::hash(v.toUtf8(),QCryptographicHash::Sha256).toHex());};
  return hash(m_profile)+"/"+hash(id);
}
bool CommunityFolderModel::expanded(const QString &id) const {
  if(m_expanded.contains(id))return m_expanded.value(id);
  if(m_profile.isEmpty())return false;
  QSettings prefs(QSettings::IniFormat,QSettings::UserScope,"WhatsFeather","CommunityFolders");
  return prefs.value(preferenceKey(id),false).toBool();
}
void CommunityFolderModel::toggleFolder(const QString &id){
  // Searches reveal descendants without changing the user's saved layout.
  if(id.isEmpty()||!m_query.isEmpty())return;
  bool exists=false;for(const auto &v:m_rows){const auto r=v.toMap();if(r.value("row_type")=="community"&&r.value("community_id")==id)exists=true;}
  if(!exists)return;
  const bool value=!expanded(id);m_expanded[id]=value;
  if(!m_profile.isEmpty()){
    QSettings prefs(QSettings::IniFormat,QSettings::UserScope,"WhatsFeather","CommunityFolders");
    prefs.setValue(preferenceKey(id),value);prefs.sync();
    QFile::setPermissions(prefs.fileName(),QFileDevice::ReadOwner|QFileDevice::WriteOwner);
  }
  rebuild();
}
void CommunityFolderModel::rebuild(){
  QVariantList rows;
  if(m_source){
    const int role=m_source->roleNames().key("record",Qt::DisplayRole);
    for(int i=0;i<std::min(1000,m_source->rowCount());++i)rows<<m_source->data(m_source->index(i,0),role);
  }
  QHash<QString,QVariantList> groups;
  QVariantList unique;
  QSet<QString> seen;
  auto community=[](const QVariantMap &r){
    return r.value("is_group").toBool()&&!r.value("community_title").toString().trimmed().isEmpty()
      ?r.value("community_id").toString():QString{};
  };
  for(const auto &v:rows){
    auto r=v.toMap();const auto id=r.value("id").toString();
    if(id.isEmpty()||seen.contains(id))continue;
    seen.insert(id);if(m_groupsOnly&&!r.value("is_group").toBool())continue;unique<<r;
    const auto key=community(r);if(!key.isEmpty())groups[key]<<r;
  }
  auto matches=[this](const QVariantMap &r){return m_query.isEmpty()||r.value("title").toString().contains(m_query,Qt::CaseInsensitive);};
  QVariantList visible;QSet<QString> emitted;
  for(const auto &v:unique){
    auto r=v.toMap();const auto key=community(r);
    if(key.isEmpty()){if(matches(r)){r["row_type"]="chat";r["depth"]=0;visible<<r;}continue;}
    if(emitted.contains(key))continue;emitted.insert(key);
    QVariantList children;const bool parentMatches=m_query.isEmpty()||r.value("community_title").toString().contains(m_query,Qt::CaseInsensitive);
    for(const auto &child:groups[key])if(parentMatches||matches(child.toMap()))children<<child;
    if(children.isEmpty())continue;
    qint64 unread=0;bool manual=false;
    for(const auto &child:groups[key]){auto c=child.toMap();unread+=std::max(0,c.value("unread").toInt());manual|=c.value("marked_unread").toBool();}
    visible<<QVariantMap{{"id","community:"+key},{"row_type","community"},{"community_id",key},
      {"title",r.value("community_title")},{"child_count",groups[key].size()},
      {"unread",int(std::min<qint64>(unread,std::numeric_limits<int>::max()))},{"marked_unread",manual},
      {"expanded",expanded(key)||!m_query.isEmpty()},{"search_expanded",!m_query.isEmpty()},{"depth",0}};
    if(expanded(key)||!m_query.isEmpty())for(const auto &child:children){auto c=child.toMap();c["row_type"]="chat";c["depth"]=1;visible<<c;}
  }
  beginResetModel();m_rows=visible;endResetModel();
}
