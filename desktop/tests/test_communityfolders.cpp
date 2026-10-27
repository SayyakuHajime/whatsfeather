#include <QtTest>
#include <QTemporaryDir>
#include <QSettings>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include "jsonmodel.h"
#include "communityfoldermodel.h"
#include "controller.h"
#include <QLocalServer>
#include <QLocalSocket>
#include <QJsonDocument>
#include <QJsonArray>

class CommunityFoldersTest : public QObject {
  Q_OBJECT
private slots:
  void refreshRequiresExplicitActionAndReloadsAuthoritativeChats() {
    QTemporaryDir tmp;
    const auto runtime = tmp.filePath("runtime");
    const auto ciphertext = tmp.filePath("ciphertext");
    const auto profileRoot = QDir(runtime).filePath("wf-pv-synthetic");
    for (const auto &path : {runtime, ciphertext, profileRoot}) {
      QVERIFY(QDir().mkpath(path));
      QVERIFY(QFile::setPermissions(path, QFileDevice::ReadOwner |
                                            QFileDevice::WriteOwner |
                                            QFileDevice::ExeOwner));
    }
    QLocalServer server;
    QVERIFY(server.listen(QDir(runtime).filePath("f.sock")));
    int refreshes=0,chatReads=0;bool fixtureDemo=false;
    connect(&server,&QLocalServer::newConnection,&server,[&]{
      auto *peer=server.nextPendingConnection();
      connect(peer,&QLocalSocket::readyRead,peer,[&,peer]{
        while(peer->canReadLine()){
          const auto req=QJsonDocument::fromJson(peer->readLine()).object();
          const auto method=req.value("method").toString();QJsonObject result;
          if(method=="app.status") result={{"mode",fixtureDemo?"demo":"live"},{"state","connected"},{"revision",1},{"account",QJsonObject{{"available",false}}},{"storage_view",QJsonObject{{"version",1},{"mode","encrypted"},{"ciphertext_root",ciphertext},{"runtime_dir",runtime},{"profile_root",profileRoot},{"view_id","0123456789abcdef0123456789abcdef"},{"mount_id","42"}}}};
          else if(method=="app.capabilities")result={{"version",1},{"methods",QJsonArray{"community.refresh","chat.list"}}};
          else if(method=="community.refresh"){++refreshes;result={{"refreshed",true},{"source","live_cache"}};}
          else if(method=="chat.list"){
            ++chatReads;QJsonArray chats;
            if(refreshes)chats.append(QJsonObject{{"id","fixture-group"},{"title","Study"},{"is_group",true},{"community_id","fixture-community"},{"community_title","Campus"}});
            result={{"chats",chats}};
          }
          peer->write(QJsonDocument(QJsonObject{{"v",1},{"id",req.value("id")},{"result",result}}).toJson(QJsonDocument::Compact)+"\n");peer->flush();
        }
      });
    });
    Controller controller(nullptr, {}, [](const QString &,
                                          const QString &runtimeDir,
                                          const QString &mountedRoot,
                                          const QString &mountId,
                                          QString *) {
      return mountId == QStringLiteral("42") &&
             QFileInfo(mountedRoot).absolutePath() == runtimeDir;
    });
    CommunityFolderModel tree;tree.setSourceModel(controller.chats());
    controller.start(server.fullServerName(),ciphertext,true);
    QTRY_VERIFY(controller.capabilitiesReady());QTRY_COMPARE(controller.connectionState(),QString("connected"));
    QCOMPARE(refreshes,0);QCOMPARE(tree.rowCount(),0);const int readsBefore=chatReads;
    bool accepted=false;
    QVERIFY2(QMetaObject::invokeMethod(&controller,"refreshCommunities",Qt::DirectConnection,Q_RETURN_ARG(bool,accepted)),"Explicit community refresh method missing");
    QVERIFY(accepted);QVERIFY(controller.property("communitiesRefreshing").toBool());
    bool duplicate=true;QVERIFY(QMetaObject::invokeMethod(&controller,"refreshCommunities",Qt::DirectConnection,Q_RETURN_ARG(bool,duplicate)));QVERIFY(!duplicate);
    QTRY_COMPARE(refreshes,1);QTRY_VERIFY(!controller.property("communitiesRefreshing").toBool());
    QTRY_VERIFY(chatReads>readsBefore);QTRY_COMPARE(tree.rowCount(),1);
    QCOMPARE(tree.row(0).value("title").toString(),QString("Campus"));
    fixtureDemo=true;
    QTRY_VERIFY(!controller.demo());
    QTRY_VERIFY(!controller.encryptedProfileViewReady());
    accepted=true;
    QVERIFY(QMetaObject::invokeMethod(&controller,"refreshCommunities",Qt::DirectConnection,Q_RETURN_ARG(bool,accepted)));
    QVERIFY(!accepted);QCOMPARE(refreshes,1);
  }
  void unknownParentsAndDuplicateRowsDoNotHideChats() {
    JsonModel source;
    const QVariantMap known{{"id","g"},{"title","Group"},{"is_group",true},{"community_id","parent"},{"community_title","Verified community"},{"marked_unread",true}};
    source.replace({known,known,QVariantMap{{"id","orphan"},{"is_group",true},{"community_id","unknown"}},QVariantMap{{"id","dm"},{"community_id","parent"},{"community_title","Must not group a DM"}}});
    CommunityFolderModel model;model.setSourceModel(&source);
    QCOMPARE(model.rowCount(),3);QCOMPARE(model.row(0).value("child_count").toInt(),1);
    QCOMPARE(model.row(0).value("unread").toInt(),0);QVERIFY(model.row(0).value("marked_unread").toBool());
    QCOMPARE(model.row(1).value("id").toString(),QString("orphan"));
    QCOMPARE(model.row(2).value("id").toString(),QString("dm"));
    source.replace({QVariantMap{{"id","g"},{"title","Group"},{"is_group",true}}});
    QCOMPARE(model.rowCount(),1);QCOMPARE(model.row(0).value("row_type").toString(),QString("chat"));
    model.setSourceModel(nullptr);QCOMPARE(model.rowCount(),0);
  }
  void searchRevealsChildrenWithoutChangingSavedCollapse() {
    JsonModel source;source.replace({
      QVariantMap{{"id","g1"},{"title","Study"},{"is_group",true},{"community_id","c"},{"community_title","Campus"}},
      QVariantMap{{"id","dm"},{"title","Friend"}},
      QVariantMap{{"id","g2"},{"title","Projects"},{"is_group",true},{"community_id","c"},{"community_title","Campus"}}});
    CommunityFolderModel model;model.setSourceModel(&source);model.setQuery("projects");
    QCOMPARE(model.rowCount(),2);
    QCOMPARE(model.row(0).value("title").toString(),QString("Campus"));
    QCOMPARE(model.row(1).value("id").toString(),QString("g2"));
    QVERIFY(model.row(0).value("search_expanded").toBool());
    QVERIFY(!model.expanded("c"));
    model.setQuery("CAMPUS");QCOMPARE(model.rowCount(),3);
    model.setQuery("");QCOMPARE(model.rowCount(),2);
    model.setGroupsOnly(true);QCOMPARE(model.rowCount(),1);
  }
  void expansionPreservesChatIdsAndPersistsPerProfile() {
    QTemporaryDir prefs;
    QSettings::setPath(QSettings::IniFormat,QSettings::UserScope,prefs.path());
    JsonModel source;
    source.replace({QVariantMap{{"id","announcement"},{"is_group",true},{"community_id","announcement"},{"community_title","Campus"}},
                    QVariantMap{{"id","g2"},{"is_group",true},{"community_id","announcement"},{"community_title","Campus"}}});
    CommunityFolderModel model;
    model.setProfileKey("fixture-account-a");model.setSourceModel(&source);
    model.toggleFolder("announcement");
    QCOMPARE(model.rowCount(),3);
    QCOMPARE(model.row(1).value("id").toString(),QString("announcement"));
    QCOMPARE(model.row(1).value("depth").toInt(),1);
    QCOMPARE(model.row(2).value("id").toString(),QString("g2"));
    CommunityFolderModel restored;restored.setProfileKey("fixture-account-a");restored.setSourceModel(&source);
    QCOMPARE(restored.rowCount(),3);
    restored.setProfileKey("fixture-account-b");QCOMPARE(restored.rowCount(),1);
    model.toggleFolder("announcement");QCOMPARE(model.rowCount(),1);
  }
  void groupsBecomeOneCollapsedFolder() {
    JsonModel source;
    source.replace({QVariantMap{{"id","g1"},{"title","Study"},{"is_group",true},{"community_id","c1"},{"community_title","Campus"},{"unread",2}},
                    QVariantMap{{"id","dm"},{"title","Friend"}},
                    QVariantMap{{"id","g2"},{"title","Projects"},{"is_group",true},{"community_id","c1"},{"community_title","Campus"},{"unread",3}},
                    QVariantMap{{"id","loose"},{"title","Loose group"},{"is_group",true}}});
    CommunityFolderModel model;
    model.setSourceModel(&source);
    QCOMPARE(model.rowCount(),3);
    auto row=model.row(0);
    QCOMPARE(row.value("row_type").toString(),QString("community"));
    QCOMPARE(row.value("title").toString(),QString("Campus"));
    QCOMPARE(row.value("child_count").toInt(),2);
    QCOMPARE(row.value("unread").toInt(),5);
    QCOMPARE(model.row(1).value("id").toString(),QString("dm"));
    QCOMPARE(model.row(2).value("id").toString(),QString("loose"));
  }
};
QTEST_GUILESS_MAIN(CommunityFoldersTest)
#include "test_communityfolders.moc"
