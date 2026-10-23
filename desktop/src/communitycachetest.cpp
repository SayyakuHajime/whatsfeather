#include "controller.h"
#include "communityfoldermodel.h"
#include <QGuiApplication>
#include <QQuickWindow>
#include <QQuickItem>
#include <QDir>
#include <QTimer>
#include <memory>

namespace {
QQuickItem *cacheItem(QQuickItem *root,const QString &name){
  if(root->objectName()==name)return root;
  for(auto *child:root->childItems())if(auto *item=cacheItem(child,name))return item;
  return nullptr;
}
}
// Unlike the layout fixture probe, this consumes actual demo-daemon cache data.
void runCommunityCacheTest(QGuiApplication &app,QQuickWindow *window,Controller &c,CommunityFolderModel &tree){
  struct State{int stage=0,ticks=0,before=0,children=0;QString id;};auto s=std::make_shared<State>();
  auto *timer=new QTimer(&app);timer->setInterval(100);
  auto fail=[&app,timer](const QString &why){qCritical()<<"Community daemon/UI FAIL:"<<why;timer->stop();app.exit(1);};
  QObject::connect(timer,&QTimer::timeout,&app,[&app,&c,&tree,window,s,timer,fail]{
    if(++s->ticks>120){fail("metadata/selection timeout");return;}
    if(s->stage==0){
      if(!c.capabilitiesReady()||!c.chats()->rowCount())return;
      if(!c.demo()){fail("refused outside demo");return;}
      if(!c.hasCapability("community.list")){fail("cache route not advertised");return;}
      int folderIndex=-1;
      for(int i=0;i<tree.rowCount();++i)if(tree.row(i).value("row_type")=="community"){folderIndex=i;break;}
      if(folderIndex<0){fail("actual daemon community fields did not produce a folder");return;}
      const auto folder=tree.row(folderIndex);s->before=tree.rowCount();s->children=folder.value("child_count").toInt();
      if(!folder.value("community_id").toString().startsWith("demo:")||s->children<1){fail("missing explicit demo provenance or members");return;}
      auto *item=cacheItem(window->contentItem(),"communityFolderRow");if(!item)return;
      QMetaObject::invokeMethod(item,"clicked");s->stage=1;
    }else if(s->stage==1){
      if(tree.rowCount()!=s->before+s->children){fail("cached children duplicated or omitted");return;}
      auto *child=cacheItem(window->contentItem(),"communityChildRow");if(!child)return;
      for(int i=0;i<tree.rowCount();++i)if(tree.row(i).value("depth").toInt()==1){s->id=tree.row(i).value("id").toString();break;}
      if(s->id.isEmpty()||c.chats()->indexOf(s->id)<0){fail("child lost real cached chat identity");return;}
      QMetaObject::invokeMethod(child,"clicked");s->stage=2;
    }else if(s->stage==2){
      if(c.selectedId()!=s->id)return;
      if(!c.error().isEmpty()){fail(c.error());return;}
      qInfo()<<"Community daemon/UI PASS: actual synthetic persisted metadata -> folder -> nested group -> selected stored chat";
      timer->stop();app.quit();
    }
  });timer->start();
}
