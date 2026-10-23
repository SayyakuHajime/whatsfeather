#include "controller.h"
#include <QJsonArray>

void Controller::setNotificationSubscription(bool enabled, bool previews) {
  if (m_notificationsEnabled == enabled && m_notificationPreviews == previews)
    return;
  m_notificationsEnabled = enabled;
  m_notificationPreviews = previews;
  ++m_notificationSerial;
  m_notificationCursor.clear();
  // An empty cursor subscribes at the daemon's current high-water mark.
}

void Controller::pollNotifications() {
  if (!m_notificationsEnabled || m_notificationPending || m_demo ||
      !connected() || !hasCapability(QStringLiteral("notification.poll")))
    return;
  if (m_connectionState != QStringLiteral("connected")) {
    m_notificationCursor.clear();
    return;
  }
  const int serial = m_notificationSerial;
  const bool previews = m_notificationPreviews;
  m_notificationPending = true;
  request(QStringLiteral("notification.poll"),
          {{"cursor", m_notificationCursor}, {"include_preview", previews}},
          [this, serial, previews](QJsonValue value, QJsonObject error) {
    m_notificationPending = false;
    if (serial != m_notificationSerial || !m_notificationsEnabled || m_demo)
      return;
    if (!error.isEmpty()) {
      m_notificationCursor.clear();
      return; // Notification failure must not disrupt chat or expose payloads.
    }
    const auto result = value.toObject();
    const auto cursor = result.value("cursor").toString();
    const auto events = result.value("events");
    if (cursor.isEmpty() || cursor.size() > 128 || !events.isArray() ||
        events.toArray().size() > 32) {
      m_notificationCursor.clear();
      return;
    }
    m_notificationCursor = cursor;
    if (result.value("reset").toBool())
      return;
    for (const auto &entry : events.toArray()) {
      const auto record = entry.toObject();
      const auto chat = record.value("chat_id").toString();
      const auto id = record.value("message_id").toString();
      if (chat.isEmpty() || chat.size() > 512 || id.isEmpty() || id.size() > 128)
        continue;
      QVariantMap event{{"chat_id", chat}, {"message_id", id},
                        {"title", record.value("title").toString().left(160)}};
      if (previews)
        event.insert("body", record.value("body").toString().left(320));
      emit incomingNotification(event);
    }
  });
}
