#pragma once
#include "jsonmodel.h"
#include "localexpressionstore.h"
#include "voicenoterecorder.h"
#include <QJsonObject>
#include <QLocalSocket>
#include <QProcess>
#include <QSet>
#include <QStringList>
#include <QTimer>
#include <QUrl>
#include <functional>
class Controller : public QObject {
  Q_OBJECT
  Q_PROPERTY(JsonModel *chats READ chats CONSTANT)
  Q_PROPERTY(JsonModel *messages READ messages CONSTANT)
  Q_PROPERTY(JsonModel *searchResults READ searchResults CONSTANT)
  Q_PROPERTY(bool searching READ searching NOTIFY findChanged)
  Q_PROPERTY(QString findQuery READ findQuery NOTIFY findChanged)
  Q_PROPERTY(QVariantMap attachment READ attachment NOTIFY attachmentChanged)
  Q_PROPERTY(QVariantList contactCards READ contactCards NOTIFY contactShareChanged)
  Q_PROPERTY(QString contactShareError READ contactShareError NOTIFY contactShareChanged)
  Q_PROPERTY(bool contactPreparing READ contactPreparing NOTIFY contactShareChanged)
  Q_PROPERTY(bool contactSending READ contactSending NOTIFY contactShareChanged)
  Q_PROPERTY(bool contactOutcomeUnknown READ contactOutcomeUnknown NOTIFY contactShareChanged)
  Q_PROPERTY(bool locationSending READ locationSending NOTIFY locationShareChanged)
  Q_PROPERTY(QString locationShareError READ locationShareError NOTIFY locationShareChanged)
  Q_PROPERTY(bool locationOutcomeUnknown READ locationOutcomeUnknown NOTIFY locationShareChanged)
  Q_PROPERTY(bool locationShareAccepted READ locationShareAccepted NOTIFY locationShareChanged)
  Q_PROPERTY(QVariantList gifItems READ gifItems NOTIFY gifsChanged)
  Q_PROPERTY(bool gifsLoading READ gifsLoading NOTIFY gifsChanged)
  Q_PROPERTY(QString gifError READ gifError NOTIFY gifsChanged)
  Q_PROPERTY(QString gifProvider READ gifProvider NOTIFY gifsChanged)
  Q_PROPERTY(QStringList recentEmojis READ recentEmojis NOTIFY expressionsChanged)
  Q_PROPERTY(QStringList favoriteEmojis READ favoriteEmojis NOTIFY expressionsChanged)
  Q_PROPERTY(QVariantList localStickerPacks READ localStickerPacks NOTIFY expressionsChanged)
  Q_PROPERTY(QString expressionStoreError READ expressionStoreError NOTIFY expressionsChanged)
  Q_PROPERTY(QVariantMap account READ account NOTIFY accountChanged)
  Q_PROPERTY(QVariantMap capabilities READ capabilities NOTIFY capabilitiesChanged)
  Q_PROPERTY(bool capabilitiesReady READ capabilitiesReady NOTIFY capabilitiesChanged)
  Q_PROPERTY(JsonModel *contacts READ contacts CONSTANT)
  Q_PROPERTY(bool contactsLoading READ contactsLoading NOTIFY contactsChanged)
  Q_PROPERTY(bool contactsHasMore READ contactsHasMore NOTIFY contactsChanged)
  Q_PROPERTY(QString contactsError READ contactsError NOTIFY contactsChanged)
  Q_PROPERTY(QVariantMap routeData READ routeData NOTIFY routeChanged)
  Q_PROPERTY(QString routeError READ routeError NOTIFY routeChanged)
  Q_PROPERTY(QString routeMethod READ routeMethod NOTIFY routeChanged)
  Q_PROPERTY(bool routeLoading READ routeLoading NOTIFY routeChanged)
  Q_PROPERTY(QString selectedId READ selectedId NOTIFY selectionChanged)
  Q_PROPERTY(QString selectedTitle READ selectedTitle NOTIFY selectionChanged)
  Q_PROPERTY(
      QString selectedInitials READ selectedInitials NOTIFY selectionChanged)
  Q_PROPERTY(QString draft READ draft WRITE setDraft NOTIFY draftChanged)
  Q_PROPERTY(QString status READ status NOTIFY stateChanged)
  Q_PROPERTY(QString connectionState READ connectionState NOTIFY stateChanged)
  Q_PROPERTY(QString error READ error NOTIFY stateChanged)
  Q_PROPERTY(QString notice READ notice NOTIFY stateChanged)
  Q_PROPERTY(bool demo READ demo NOTIFY stateChanged)
  Q_PROPERTY(bool connected READ connected NOTIFY stateChanged)
  Q_PROPERTY(bool sending READ sending NOTIFY stateChanged)
  Q_PROPERTY(QStringList pollVoteBlockedTargets READ pollVoteBlockedTargets NOTIFY pollVotesChanged)
  Q_PROPERTY(QStringList listReplyBlockedTargets READ listReplyBlockedTargets NOTIFY listRepliesChanged)
  Q_PROPERTY(QStringList buttonReplyBlockedTargets READ buttonReplyBlockedTargets NOTIFY buttonRepliesChanged)
  Q_PROPERTY(bool voiceRecording READ voiceRecording NOTIFY stateChanged)
  Q_PROPERTY(bool voiceEncoding READ voiceEncoding NOTIFY stateChanged)
  Q_PROPERTY(int voiceRecordingSeconds READ voiceRecordingSeconds NOTIFY stateChanged)
  Q_PROPERTY(QString voiceRecordingError READ voiceRecordingError NOTIFY stateChanged)
  Q_PROPERTY(bool canQuit READ canQuit NOTIFY stateChanged)
  Q_PROPERTY(bool hasMore READ hasMore NOTIFY messagesChanged)
  Q_PROPERTY(bool loading READ loading NOTIFY messagesChanged)
  Q_PROPERTY(bool historyWindow READ historyWindow NOTIFY messagesChanged)
  Q_PROPERTY(int messageCount READ messageCount NOTIFY messagesChanged)
public:
  using ProfileMountVerifier = std::function<bool(
      const QString &ciphertextRoot, const QString &runtimeDir,
      const QString &profileRoot, const QString &mountId, QString *error)>;
  explicit Controller(QObject *parent = nullptr,
                      VoiceNoteRecorder::CaptureFactory voiceCaptureFactory = {},
                      ProfileMountVerifier profileMountVerifier = {});
  ~Controller();
  JsonModel *chats() { return &m_chats; }
  JsonModel *messages() { return &m_messages; }
  JsonModel *searchResults() { return &m_searchResults; }
  bool searching() const { return m_searching; }
  QString findQuery() const { return m_findQuery; }
  QVariantMap attachment() const { return m_attachment; }
  QVariantList contactCards() const { return m_contactCards; }
  QString contactShareError() const { return m_contactShareError; }
  bool contactPreparing() const { return m_contactPreparing; }
  bool contactSending() const { return m_contactSending; }
  bool contactOutcomeUnknown() const { return m_contactOutcomeUnknown; }
  bool locationSending() const { return m_locationSending; }
  QString locationShareError() const { return m_locationShareError; }
  bool locationOutcomeUnknown() const { return m_locationOutcomeUnknown; }
  bool locationShareAccepted() const { return m_locationShareAccepted; }
  QVariantList gifItems() const { return m_gifItems; }
  bool gifsLoading() const { return m_gifsLoading; }
  QString gifError() const { return m_gifError; }
  QString gifProvider() const { return m_gifProvider; }
  QStringList recentEmojis() const { return m_expressionStore.recentEmojis(); }
  QStringList favoriteEmojis() const { return m_expressionStore.favoriteEmojis(); }
  QVariantList localStickerPacks() const { return m_expressionStore.stickerPacks(); }
  QString expressionStoreError() const { return m_expressionStoreError; }
  bool encryptedProfileViewReady() const { return m_profileViewReady; }
  QVariantMap account() const { return m_account; }
  QVariantMap capabilities() const { return m_capabilities; }
  bool capabilitiesReady() const { return m_capabilitiesReady; }
  JsonModel *contacts() { return &m_contacts; }
  bool contactsLoading() const { return m_contactsLoading; }
  bool contactsHasMore() const { return m_contactsHasMore; }
  QString contactsError() const { return m_contactsError; }
  QVariantMap routeData() const { return m_routeData; }
  QString routeError() const { return m_routeError; }
  QString routeMethod() const { return m_routeMethod; }
  bool routeLoading() const { return m_routeLoading; }
  Q_INVOKABLE void setNotificationSubscription(bool enabled, bool previews);
  Q_INVOKABLE bool hasCapability(const QString &method) const;
  Q_INVOKABLE QString capabilityReason(const QString &method) const;
  Q_INVOKABLE bool performMessageAction(const QString &method,
                                        const QVariantMap &message,
                                        const QVariantMap &options = {});
  Q_INVOKABLE void searchContacts(const QString &query);
  Q_INVOKABLE void loadMoreContacts();
  Q_INVOKABLE void openChatRecipient(const QString &recipient);
  Q_INVOKABLE bool updateChat(const QString &chatId,
                              const QVariantMap &changes);
  Q_PROPERTY(bool communitiesRefreshing READ communitiesRefreshing NOTIFY stateChanged)
  bool communitiesRefreshing() const { return m_communitiesRefreshing; }
  Q_INVOKABLE bool refreshCommunities();
  Q_INVOKABLE bool loadHub(const QString &method,
                           const QVariantMap &params = {});
  Q_INVOKABLE bool performCapabilityAction(const QString &method,
                                           const QVariantMap &params = {});
  Q_INVOKABLE void findMessages(const QString &query);
  Q_INVOKABLE void revealMessage(const QString &id);
  Q_INVOKABLE bool revealMediaLibraryMessage(const QString &chatId,
                                             const QString &messageId);
  Q_INVOKABLE void stageAttachment(const QUrl &file);
  Q_INVOKABLE void stageAttachmentAs(const QUrl &file, const QString &mode);
  Q_INVOKABLE void stageAttachmentAlbum(const QVariantList &files);
  Q_INVOKABLE void stageContactFile(const QUrl &file);
  Q_INVOKABLE bool sendContactCards();
  Q_INVOKABLE void clearContactShare();
  Q_INVOKABLE bool sendLocation(const QVariantMap &location);
  Q_INVOKABLE void clearLocationShare();
  Q_INVOKABLE bool startVoiceNoteRecording();
  Q_INVOKABLE void stopVoiceNoteRecording();
  Q_INVOKABLE void cancelVoiceNoteRecording();
  Q_INVOKABLE void changeAttachmentMode(const QString &mode);
  Q_INVOKABLE void searchGifs(const QString &query, const QString &provider = QStringLiteral("commons"));
  Q_INVOKABLE void prepareGif(const QString &id, const QString &provider);
  Q_INVOKABLE bool recordRecentEmoji(const QString &emoji);
  Q_INVOKABLE bool toggleFavoriteEmoji(const QString &emoji);
  Q_INVOKABLE bool addCurrentStickerToPack(const QString &packName);
  Q_INVOKABLE bool stageLocalSticker(const QString &packId,
                                     const QString &stickerId);
  Q_INVOKABLE void clearAttachment();
  Q_INVOKABLE void copyText(const QString &text);
  Q_INVOKABLE void quoteMessage(const QVariantMap &message);
  QString selectedId() const { return m_id; }
  QString selectedTitle() const { return m_title; }
  QString selectedInitials() const { return m_initials; }
  QString draft() const { return m_draft; }
  QString status() const { return m_status; }
  QString connectionState() const { return m_connectionState; }
  QString error() const { return m_error; }
  QString notice() const { return m_notice; }
  bool demo() const { return m_demo; }
  bool connected() const {
    return m_socket.state() == QLocalSocket::ConnectedState;
  }
  bool sending() const { return m_sending; }
  QStringList pollVoteBlockedTargets() const { return m_pollVoteLocks.value(m_id).values(); }
  QStringList listReplyBlockedTargets() const { return m_listReplyLocks.value(m_id).values(); }
  QStringList buttonReplyBlockedTargets() const { return m_buttonReplyLocks.value(m_id).values(); }
  bool voiceRecording() const { return m_voiceRecorder.recording(); }
  bool voiceEncoding() const { return m_voiceRecorder.encoding(); }
  int voiceRecordingSeconds() const { return m_voiceRecorder.elapsedSeconds(); }
  QString voiceRecordingError() const { return m_voiceRecorder.error(); }
  bool canQuit() const { return m_canQuit; }
  bool hasMore() const { return m_hasMore; }
  bool loading() const { return m_loading; }
  bool historyWindow() const { return m_history; }
  int messageCount() const { return m_messages.rowCount(); }
  bool ownsBackend() const { return m_process.state() != QProcess::NotRunning; }
  void start(const QString &socket, const QString &dataDir, bool live,
             const QString &liveTestScopePath = {});
  Q_INVOKABLE void selectChat(const QString &id);
  Q_INVOKABLE void openSelfChat();
  Q_INVOKABLE void refreshHistory();
  Q_INVOKABLE void setDraft(const QString &text);
  Q_INVOKABLE void search(const QString &query);
  Q_INVOKABLE void send();
  Q_INVOKABLE void loadOlder();
  Q_INVOKABLE void latest();
  Q_INVOKABLE void clearError();
  Q_INVOKABLE void requestQuit();
  Q_INVOKABLE QString timeLabel(qint64 value) const;
  Q_INVOKABLE QString messageStatusLabel(const QString &status) const;
  Q_INVOKABLE QString messageStatusDescription(const QString &status) const;
  Q_INVOKABLE void saveTheme(bool dark);
  Q_INVOKABLE void downloadMessage(const QString &id);
  Q_INVOKABLE void openAttachment(const QString &id);
  Q_INVOKABLE void openExternalLink(const QString &link);
  Q_INVOKABLE bool messageBusy(const QString &id) const { return m_mediaBusy.contains(id); }
  Q_INVOKABLE QString messageError(const QString &id) const { return m_mediaErrors.value(id); }
  Q_INVOKABLE void connectLive();
  Q_INVOKABLE void disconnectLive();
  QString qr() const { return m_qr; }
  Q_PROPERTY(QString qr READ qr NOTIFY stateChanged)
signals:
  void rpcRequestSent(const QString &method);
  void incomingNotification(const QVariantMap &event);
  void selectionChanged();
  void draftChanged();
  void stateChanged();
  void pollVotesChanged();
  void listRepliesChanged();
  void buttonRepliesChanged();
  void messagesChanged();
  void olderLoaded();
  void tailUpdated();
  void findChanged();
  void attachmentChanged();
  void contactShareChanged();
  void locationShareChanged();
  void gifsChanged();
  void expressionsChanged();
  void accountChanged();
  void capabilitiesChanged();
  void contactsChanged();
  void routeChanged();
  void messageRevealed(const QString &id);

private:
  using Callback = std::function<void(QJsonValue, QJsonObject)>;
  void request(const QString &, const QJsonObject &, Callback);
  void read();
  void statusPoll();
  void pollNotifications();
  QString m_notificationCursor;
  bool m_notificationsEnabled = false, m_notificationPreviews = false,
       m_notificationPending = false;
  int m_notificationSerial = 0;
  void refreshAccount(const QVariantMap &fromStatus = {});
  void refreshChats();
  void refreshCapabilities();
  void fetchMessages(bool reset);
  void finishPendingMediaReveal(int generation);
  void persistDraft();
  bool applyEncryptedProfileView(const QVariantMap &view, QString *error);
  void invalidateEncryptedProfileView(const QString &reason);
  bool isActiveProfileMediaFile(const QString &path) const;
  QVariant sanitizeProfileMediaUrls(const QVariant &value) const;
  void runFind();
  void sendAttachment();
  void sendAlbum();
  void releaseAlbumPaths(const QStringList &paths);
  void stageAttachmentAsWithDisplayName(const QUrl &file, const QString &mode,
                                        const QString &displayName);
  void finishVoiceStaging(int serial);
  JsonModel m_chats{1000, this}, m_messages{500, this},
      m_searchResults{100, this}, m_contacts{200, this};
  QTimer m_findTimer;
  QString m_findQuery, m_attachmentPath, m_attachmentOriginalPath, m_attachmentMode = "photo", m_attachmentSendId;
  QString m_contactPath, m_contactShareError, m_contactSendId;
  QString m_locationShareError, m_locationSendId;
  QVariantList m_contactCards;
  LocalExpressionStore m_expressionStore;
  QString m_expressionStoreError;
  bool m_contactPreparing = false, m_contactSending = false,
       m_contactOutcomeUnknown = false;
  bool m_locationSending = false, m_locationOutcomeUnknown = false,
       m_locationShareAccepted = false;
  QVariantList m_gifItems;
  QString m_gifError, m_gifProvider = "commons";
  bool m_gifsLoading = false;
  bool m_searching = false;
  QVariantMap m_attachment;
  QStringList m_albumPreparedPaths;
  int m_attachmentSerial = 0;
  QLocalSocket m_socket;
  QProcess m_process;
  QTimer m_poll, m_retry, m_timeout, m_draftTimer, m_searchTimer;
  struct Pending {
    Callback callback;
    qint64 started;
  };
  QHash<QString, Pending> m_pending;
  quint64 m_next = 0;
  QByteArray m_buffer;
  QString m_id, m_title, m_initials, m_draft,
      m_status = "Starting local backend…", m_connectionState = "starting", m_error, m_query, m_path, m_dataDir, m_ciphertextRoot, m_qr;
  QString m_storageViewId, m_profileMountId;
  QString m_pendingMediaChat, m_pendingMediaMessage;
  QString m_notice;
  bool m_syncPending = false;
  bool m_canQuit = false;
  bool m_demo = true, m_sending = false, m_hasMore = false, m_loading = false,
       m_history = false, m_statusPending = false, m_accountPending = false;
  bool m_liveMode = false, m_profileViewReady = false;
  quint64 m_backendGeneration = 0;
  quint64 m_profileViewGeneration = 0, m_voiceCaptureViewGeneration = 0;
  ProfileMountVerifier m_profileMountVerifier;
  QVariantMap m_account;
  QVariantMap m_capabilities, m_routeData;
  QString m_routeError, m_routeMethod, m_contactQuery, m_contactCursor,
      m_contactsError;
  QTimer m_contactTimer;
  QStringList m_capabilityMethods;
  bool m_capabilitiesReady = false, m_capabilitiesPending = false;
  bool m_communitiesRefreshing = false;
  bool m_routeLoading = false, m_contactsLoading = false,
       m_contactsHasMore = false;
  // A dispatched poll vote stays locked if its outcome is ambiguous. Changing
  // chat selection or reconnecting must not turn an unknown result into retry.
  QHash<QString, QSet<QString>> m_pollVoteLocks;
  // An unresolved list reply remains blocked across chat changes until a fresh
  // backend projection is loaded; the backend remains the restart authority.
  QHash<QString, QSet<QString>> m_listReplyLocks;
  // A ButtonsMessage response stays blocked after an ambiguous dispatch; only
  // a safe not-dispatched result releases the per-target retry lock.
  QHash<QString, QSet<QString>> m_buttonReplyLocks;
  QHash<QString, bool> m_mediaBusy;
  QHash<QString, QString> m_mediaErrors;
  int m_generation = 0, m_draftVersion = 0, m_retries = 0, m_gifSerial = 0,
      m_routeSerial = 0, m_contactSerial = 0, m_contactFileSerial = 0;
  double m_revision = -1;
  double m_accountRevision = -2;
  VoiceNoteRecorder m_voiceRecorder;
  int m_voiceAttachmentSerial = -1;
  bool m_voiceStagePending = false;
  bool m_voiceDiscardPending = false;
};
