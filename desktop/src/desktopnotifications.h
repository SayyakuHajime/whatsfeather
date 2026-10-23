#pragma once

#include <QObject>
#include <QQueue>
#include <QSet>
#include <QSettings>
#include <QString>
#include <QTimer>
#include <QVariantMap>

#include <functional>
#include <memory>

class DesktopNotificationDelivery {
public:
  virtual ~DesktopNotificationDelivery() = default;
  virtual bool trayAvailable() const = 0;
  virtual bool notificationsAvailable() const = 0;
  virtual void setTrayVisible(bool visible) = 0;
  virtual void setToolTip(const QString &toolTip) = 0;
  virtual void showNotification(const QString &title, const QString &body,
                                int timeoutMs) = 0;
  virtual void setOpenHandler(std::function<void()> handler) = 0;
  virtual void setQuitHandler(std::function<void()> handler) = 0;
  virtual void setNotificationClickHandler(std::function<void()> handler) = 0;
};

class DesktopNotifications final : public QObject {
  Q_OBJECT
  Q_PROPERTY(bool notificationsEnabled READ notificationsEnabled WRITE
                 setNotificationsEnabled NOTIFY preferencesChanged)
  Q_PROPERTY(bool messagePreviewsEnabled READ messagePreviewsEnabled WRITE
                 setMessagePreviewsEnabled NOTIFY preferencesChanged)
  Q_PROPERTY(bool trayAvailable READ trayAvailable NOTIFY availabilityChanged)
  Q_PROPERTY(bool notificationsAvailable READ notificationsAvailable NOTIFY
                 availabilityChanged)
  Q_PROPERTY(QString trayStatus READ trayStatus NOTIFY availabilityChanged)

public:
  static constexpr int MaximumRememberedMessages = 512;
  static constexpr int MaximumPreviewCharacters = 160;
  static constexpr int NotificationTimeoutMs = 5000;

  struct IncomingMessage {
    QString chatId;
    QString messageId;
    QString chatTitle;
    QString body;
    // Callers must positively classify the event before it can be delivered.
    bool outgoing = true;
    bool demo = true;
    bool historical = true;
    bool activeVisibleChat = true;
  };

  explicit DesktopNotifications(QObject *parent = nullptr);
  DesktopNotifications(QSettings *settings,
                       std::unique_ptr<DesktopNotificationDelivery> delivery,
                       QObject *parent = nullptr);
  ~DesktopNotifications() override;

  bool notificationsEnabled() const { return m_notificationsEnabled; }
  bool messagePreviewsEnabled() const { return m_messagePreviewsEnabled; }
  bool trayAvailable() const { return m_trayAvailable; }
  bool notificationsAvailable() const { return m_notificationsAvailable; }
  QString trayStatus() const;

  void setNotificationsEnabled(bool enabled);
  void setMessagePreviewsEnabled(bool enabled);
  bool notifyIncomingMessage(const IncomingMessage &message);
  bool notifyLiveInboundEvent(const QVariantMap &event, bool demo,
                              bool activeVisibleChat);
  Q_INVOKABLE void refreshAvailability();

signals:
  void preferencesChanged();
  void availabilityChanged();
  void showWindowRequested();
  void quitRequested();

private:
  void initialize();
  static QString safeNotificationText(const QString &text,
                                      int maximumCharacters);

  std::unique_ptr<QSettings> m_ownedSettings;
  QSettings *m_settings = nullptr;
  std::unique_ptr<DesktopNotificationDelivery> m_delivery;
  QTimer m_availabilityTimer;
  QQueue<QString> m_seenMessageOrder;
  QSet<QString> m_seenMessageKeys;
  bool m_notificationsEnabled = false;
  bool m_messagePreviewsEnabled = false;
  bool m_trayAvailable = false;
  bool m_notificationsAvailable = false;
};
