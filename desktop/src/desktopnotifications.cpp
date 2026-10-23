#include "desktopnotifications.h"

#include <QAction>
#include <QChar>
#include <QGuiApplication>
#include <QIcon>
#include <QMenu>
#include <QSystemTrayIcon>

#include <utility>

namespace {
class QtSystemTrayNotificationDelivery final
    : public DesktopNotificationDelivery {
public:
  QtSystemTrayNotificationDelivery()
      : m_menu(), m_trayIcon(QGuiApplication::windowIcon()) {
    if (m_trayIcon.icon().isNull())
      m_trayIcon.setIcon(QIcon::fromTheme(QStringLiteral("whatsfeather")));
    m_trayIcon.setToolTip(QStringLiteral("WhatsFeather"));

    auto *openAction = m_menu.addAction(QStringLiteral("Open WhatsFeather"));
    QObject::connect(openAction, &QAction::triggered, &m_menu, [this] {
      if (m_openHandler)
        m_openHandler();
    });
    m_menu.addSeparator();
    auto *quitAction = m_menu.addAction(QStringLiteral("Quit WhatsFeather"));
    QObject::connect(quitAction, &QAction::triggered, &m_menu, [this] {
      if (m_quitHandler)
        m_quitHandler();
    });
    m_trayIcon.setContextMenu(&m_menu);
    QObject::connect(&m_trayIcon, &QSystemTrayIcon::messageClicked, &m_trayIcon,
                     [this] {
                       if (m_notificationClickHandler)
                         m_notificationClickHandler();
                     });
    QObject::connect(&m_trayIcon, &QSystemTrayIcon::activated, &m_trayIcon,
                     [this](QSystemTrayIcon::ActivationReason reason) {
                       if ((reason == QSystemTrayIcon::Trigger ||
                            reason == QSystemTrayIcon::DoubleClick) &&
                           m_openHandler)
                         m_openHandler();
                     });
  }

  bool trayAvailable() const override {
    return QSystemTrayIcon::isSystemTrayAvailable();
  }

  bool notificationsAvailable() const override {
    return QSystemTrayIcon::supportsMessages();
  }

  void setTrayVisible(bool visible) override {
    m_trayIcon.setVisible(visible && trayAvailable());
  }

  void setToolTip(const QString &toolTip) override {
    m_trayIcon.setToolTip(toolTip);
  }

  void showNotification(const QString &title, const QString &body,
                        int timeoutMs) override {
    if (trayAvailable() && notificationsAvailable())
      m_trayIcon.showMessage(title, body, QSystemTrayIcon::Information,
                             timeoutMs);
  }

  void setOpenHandler(std::function<void()> handler) override {
    m_openHandler = std::move(handler);
  }

  void setQuitHandler(std::function<void()> handler) override {
    m_quitHandler = std::move(handler);
  }

  void setNotificationClickHandler(std::function<void()> handler) override {
    m_notificationClickHandler = std::move(handler);
  }

private:
  // The tray icon is destroyed before its non-owning context-menu pointer.
  QMenu m_menu;
  QSystemTrayIcon m_trayIcon;
  std::function<void()> m_openHandler;
  std::function<void()> m_quitHandler;
  std::function<void()> m_notificationClickHandler;
};

constexpr auto NotificationsEnabledKey = "notifications/enabled";
constexpr auto MessagePreviewsEnabledKey = "notifications/messagePreviews";
constexpr int MaximumInputCodeUnits = 4096;
constexpr int AvailabilityPollIntervalMs = 2500;
} // namespace

DesktopNotifications::DesktopNotifications(QObject *parent)
    : QObject(parent), m_ownedSettings(std::make_unique<QSettings>()),
      m_settings(m_ownedSettings.get()),
      m_delivery(std::make_unique<QtSystemTrayNotificationDelivery>()) {
  initialize();
}

DesktopNotifications::DesktopNotifications(
    QSettings *settings, std::unique_ptr<DesktopNotificationDelivery> delivery,
    QObject *parent)
    : QObject(parent), m_settings(settings), m_delivery(std::move(delivery)) {
  if (!m_settings) {
    m_ownedSettings = std::make_unique<QSettings>();
    m_settings = m_ownedSettings.get();
  }
  if (!m_delivery)
    m_delivery = std::make_unique<QtSystemTrayNotificationDelivery>();
  initialize();
}

DesktopNotifications::~DesktopNotifications() {
  if (m_delivery) {
    m_delivery->setOpenHandler({});
    m_delivery->setQuitHandler({});
    m_delivery->setNotificationClickHandler({});
  }
}

void DesktopNotifications::initialize() {
  m_notificationsEnabled =
      m_settings->value(NotificationsEnabledKey, false).toBool();
  m_messagePreviewsEnabled =
      m_settings->value(MessagePreviewsEnabledKey, false).toBool();
  m_delivery->setToolTip(QStringLiteral("WhatsFeather"));
  m_delivery->setOpenHandler([this] { emit showWindowRequested(); });
  m_delivery->setQuitHandler([this] { emit quitRequested(); });
  m_delivery->setNotificationClickHandler(
      [this] { emit showWindowRequested(); });

  connect(&m_availabilityTimer, &QTimer::timeout, this,
          &DesktopNotifications::refreshAvailability);
  m_availabilityTimer.setInterval(AvailabilityPollIntervalMs);
  m_availabilityTimer.start();
  refreshAvailability();
}

QString DesktopNotifications::trayStatus() const {
  if (!m_trayAvailable)
    return QStringLiteral("System tray unavailable");
  if (!m_notificationsAvailable)
    return QStringLiteral(
        "System tray available; desktop notifications unavailable");
  return QStringLiteral("System tray available");
}

void DesktopNotifications::setNotificationsEnabled(bool enabled) {
  if (m_notificationsEnabled == enabled)
    return;
  m_notificationsEnabled = enabled;
  m_settings->setValue(NotificationsEnabledKey, enabled);
  emit preferencesChanged();
}

void DesktopNotifications::setMessagePreviewsEnabled(bool enabled) {
  if (m_messagePreviewsEnabled == enabled)
    return;
  m_messagePreviewsEnabled = enabled;
  m_settings->setValue(MessagePreviewsEnabledKey, enabled);
  emit preferencesChanged();
}

void DesktopNotifications::refreshAvailability() {
  const bool trayAvailable = m_delivery->trayAvailable();
  const bool notificationsAvailable =
      trayAvailable && m_delivery->notificationsAvailable();
  const bool changed = trayAvailable != m_trayAvailable ||
                       notificationsAvailable != m_notificationsAvailable;
  m_trayAvailable = trayAvailable;
  m_notificationsAvailable = notificationsAvailable;
  m_delivery->setTrayVisible(trayAvailable);
  if (changed)
    emit availabilityChanged();
}

bool DesktopNotifications::notifyLiveInboundEvent(const QVariantMap &event,
                                                  bool demo,
                                                  bool activeVisibleChat) {
  IncomingMessage message;
  message.chatId = event.value(QStringLiteral("chat_id")).toString();
  message.messageId = event.value(QStringLiteral("message_id")).toString();
  message.chatTitle = event.value(QStringLiteral("title")).toString();
  if (m_messagePreviewsEnabled)
    message.body = event.value(QStringLiteral("body")).toString();
  message.outgoing = false;
  message.demo = demo;
  message.historical = false;
  message.activeVisibleChat = activeVisibleChat;
  return notifyIncomingMessage(message);
}

bool DesktopNotifications::notifyIncomingMessage(
    const IncomingMessage &message) {
  if (!m_notificationsEnabled || message.outgoing || message.demo ||
      message.historical || message.activeVisibleChat || !m_trayAvailable ||
      !m_notificationsAvailable)
    return false;
  if (message.chatId.trimmed().isEmpty() ||
      message.messageId.trimmed().isEmpty() || message.chatId.size() > 512 ||
      message.messageId.size() > 512)
    return false;

  const QString key = QString::number(message.chatId.size()) +
                      QLatin1Char(':') + message.chatId + message.messageId;
  if (m_seenMessageKeys.contains(key))
    return false;

  QString title = safeNotificationText(message.chatTitle, 80);
  if (title.isEmpty())
    title = QStringLiteral("WhatsFeather");
  QString body =
      m_messagePreviewsEnabled
          ? safeNotificationText(message.body, MaximumPreviewCharacters)
          : QStringLiteral("New message");
  if (body.isEmpty())
    body = QStringLiteral("New message");

  m_seenMessageKeys.insert(key);
  m_seenMessageOrder.enqueue(key);
  if (m_seenMessageOrder.size() > MaximumRememberedMessages)
    m_seenMessageKeys.remove(m_seenMessageOrder.dequeue());

  m_delivery->showNotification(title, body, NotificationTimeoutMs);
  return true;
}

QString DesktopNotifications::safeNotificationText(const QString &text,
                                                   int maximumCharacters) {
  const QString input = text.left(MaximumInputCodeUnits);
  QString safe;
  safe.reserve(qMin(input.size(), maximumCharacters + 16));
  int characters = 0;
  for (qsizetype index = 0;
       index < input.size() && characters < maximumCharacters; ++characters) {
    const QChar current = input.at(index);
    if (current.isHighSurrogate() && index + 1 < input.size() &&
        input.at(index + 1).isLowSurrogate()) {
      safe.append(current);
      safe.append(input.at(index + 1));
      index += 2;
      continue;
    }
    if (current.category() == QChar::Other_Control) {
      safe.append(QLatin1Char(' '));
    } else if (current.category() != QChar::Other_Surrogate) {
      safe.append(current);
    }
    ++index;
  }
  return safe.simplified();
}
