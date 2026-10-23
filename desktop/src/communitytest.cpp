#include "controller.h"
#include "communityfoldermodel.h"
#include <QGuiApplication>
#include <QQuickWindow>
#include <QQuickItem>
#include <QTimer>
#include <QDir>
#include <QKeyEvent>
#include <QDebug>
#include <memory>

namespace {
QQuickItem *communityItem(QQuickItem *item,const QString &name){
  if(item->objectName()==name)return item;
  for(auto *child:item->childItems())if(auto *found=communityItem(child,name))return found;
  return nullptr;
}
}
void runCommunityTest(QGuiApplication &app,QQuickWindow *window,Controller &controller,CommunityFolderModel &model,const QString &dir){
  struct State{int stage=0,ticks=0;QString child,directChat,directTitle;bool confirmationChecked=false;};auto s=std::make_shared<State>();
  auto *timer=new QTimer(&app);timer->setInterval(100);
  auto fail=[&app,timer](const QString &reason){qCritical()<<"Community folders FAIL:"<<reason;timer->stop();app.exit(1);};
  auto capture=[window,dir](const QString &name){if(!dir.isEmpty()){QDir().mkpath(dir);window->grabWindow().save(QDir(dir).filePath(name+".png"));}};
  QObject::connect(timer,&QTimer::timeout,&app,[&,window,s,timer,fail,capture]{
    if(++s->ticks>160){fail("timed out");return;}
    if(s->stage==0){
      if(controller.chats()->rowCount()<3)return;
      if(!controller.demo()){fail("fixture refused outside demo");return;}
      auto a=controller.chats()->row(1),b=controller.chats()->row(2);
      a["title"]="Study group";a["initials"]="SG";a["is_group"]=true;
      b["title"]="Project group";b["initials"]="PG";b["is_group"]=true;
      a["community_id"]="synthetic-campus";b["community_id"]="synthetic-campus";
      a["community_title"]="Campus community";b["community_title"]="Campus community";
      a["unread"]=2;b["unread"]=3;s->child=a.value("id").toString();
      controller.chats()->replace({a,controller.chats()->row(0),b});s->stage=1;
    }else if(s->stage==1){
      auto *confirmation=window->findChild<QObject *>("communityRefreshDialog");
      if(!confirmation){fail("community refresh confirmation missing");return;}
      if(!s->confirmationChecked){QMetaObject::invokeMethod(confirmation,"open");s->stage=10;return;}
      auto *folder=communityItem(window->contentItem(),"communityFolderRow");
      if(!folder){fail("community folder delegate missing");return;}
      if(model.rowCount()!=2){fail("collapsed membership duplicated");return;}
      capture("collapsed");QMetaObject::invokeMethod(folder,"clicked");s->stage=2;
    }else if(s->stage==10){
      auto *confirmation=window->findChild<QObject *>("communityRefreshDialog");
      if(!confirmation->property("opened").toBool())return;
      capture("refresh-confirmation");QMetaObject::invokeMethod(confirmation,"reject");s->stage=11;
    }else if(s->stage==11){
      auto *confirmation=window->findChild<QObject *>("communityRefreshDialog");
      if(confirmation->property("visible").toBool())return;
      if(controller.communitiesRefreshing()){fail("cancel started a refresh");return;}
      s->confirmationChecked=true;s->stage=1;
    }else if(s->stage==2){
      if(model.rowCount()!=4){fail("folder did not expand");return;}
      auto *child=communityItem(window->contentItem(),"communityChildRow");
      auto *folder=communityItem(window->contentItem(),"communityFolderRow");
      if(!child||!folder||child->property("leftPadding").toReal()<=folder->property("leftPadding").toReal()){fail("missing child indentation");return;}
      capture("expanded");QMetaObject::invokeMethod(child,"clicked");s->stage=3;
    }else if(s->stage==3){
      if(controller.selectedId()!=s->child)return;
      window->setProperty("chatFilter", "communities");
      QVariant dmMatches;
      QMetaObject::invokeMethod(window,"chatMatches",Q_RETURN_ARG(QVariant,dmMatches),Q_ARG(QVariant,controller.chats()->row(1)));
      if(dmMatches.toBool()){fail("community-only filter kept a direct chat");return;}
      window->setProperty("chatFilter", "all");
      const auto direct=model.row(model.rowCount()-1);
      s->directChat=direct.value("id").toString();s->directTitle=direct.value("title").toString();
      auto *directRow=communityItem(window->contentItem(),"chatRow");
      if(s->directChat.isEmpty()||s->directTitle.isEmpty()||!directRow){fail("top-level direct chat row missing");return;}
      QMetaObject::invokeMethod(directRow,"clicked");s->stage=7;
    }else if(s->stage==4){
      if(model.rowCount()!=2||model.row(1).value("title").toString()!="Project group"){fail("search lost nested group");return;}
      capture("search");model.setQuery("");controller.selectChat("");window->resize(720,600);s->stage=5;
    }else if(s->stage==7){
      if(controller.selectedId()!=s->directChat||controller.loading())return;
      auto *title=window->findChild<QObject *>("conversationTitle");
      auto *directRow=communityItem(window->contentItem(),"chatRow");
      if(controller.selectedTitle()!=s->directTitle||!title||title->property("text").toString()!=s->directTitle||
         !directRow||directRow->property("selectedId").toString()!=s->directChat){
        fail(QString("direct chat selection mismatch: selectedTitle=%1 expectedTitle=%2 titleItem=%3 titleText=%4 row=%5 rowSelection=%6 expectedId=%7")
                 .arg(controller.selectedTitle(),s->directTitle)
                 .arg(title!=nullptr)
                 .arg(title?title->property("text").toString():QStringLiteral("<missing>"))
                 .arg(directRow!=nullptr)
                 .arg(directRow?directRow->property("selectedId").toString():QStringLiteral("<missing>"),s->directChat));
        return;
      }
      capture("selected-chat");model.setQuery("Project");s->stage=4;
    }else if(s->stage==5){
      capture("compact");auto *folder=communityItem(window->contentItem(),"communityFolderRow");
      if(!folder){fail("compact folder missing");return;}
      folder->forceActiveFocus();QKeyEvent press(QEvent::KeyPress,Qt::Key_Left,Qt::NoModifier);QCoreApplication::sendEvent(window,&press);s->stage=6;
    }else if(s->stage==6){
      if(model.rowCount()!=2||model.expanded("synthetic-campus")){fail("keyboard collapse failed");return;}
      if(controller.chats()->rowCount()!=3||!controller.selectedId().isEmpty()){fail("folder altered chat or selection");return;}
      qInfo()<<"Native community folders PASS: collapse, expand, indentation, child/direct chat selection, search, compact, keyboard; synthetic projection only";
      timer->stop();app.quit();
    }
  });timer->start();
}
