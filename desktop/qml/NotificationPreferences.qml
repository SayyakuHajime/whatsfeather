import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

Item {
    id: root

    property bool notificationsEnabled: false
    property bool messagePreviewsEnabled: false
    property bool trayAvailable: false
    property bool notificationsAvailable: false
    property color ink: "#172126"
    property color muted: "#52616b"
    property color danger: "#b42318"

    signal notificationsEnabledRequested(bool enabled)
    signal messagePreviewsEnabledRequested(bool enabled)

    implicitHeight: settingsColumn.implicitHeight
    implicitWidth: settingsColumn.implicitWidth

    ColumnLayout {
        id: settingsColumn
        anchors.fill: parent
        spacing: 8

        Label {
            text: "Notifications"
            color: root.ink
            font.weight: Font.DemiBold
            Layout.fillWidth: true
        }

        CheckBox {
            objectName: "desktopNotificationsEnabled"
            text: "Enable desktop notifications"
            checked: root.notificationsEnabled
            onToggled: root.notificationsEnabledRequested(checked)
            Accessible.name: text
            Layout.fillWidth: true
        }

        CheckBox {
            objectName: "desktopMessagePreviewsEnabled"
            text: "Show message text in notifications"
            checked: root.messagePreviewsEnabled
            enabled: root.notificationsEnabled
            onToggled: root.messagePreviewsEnabledRequested(checked)
            Accessible.name: text
            Accessible.description: "Off by default. When enabled, message text can appear on screen outside WhatsFeather."
            Layout.fillWidth: true
        }

        Label {
            objectName: "notificationTrayAvailabilityStatus"
            text: !root.trayAvailable
                  ? "System tray unavailable. Notifications cannot be delivered on this desktop yet."
                  : !root.notificationsAvailable
                        ? "System tray available, but desktop notifications are unavailable on this desktop."
                        : "System tray and desktop notifications are available."
            color: root.trayAvailable && root.notificationsAvailable
                   ? root.muted : root.danger
            wrapMode: Text.WordWrap
            Layout.fillWidth: true
        }

        Label {
            text: "Message text stays hidden unless you explicitly enable previews."
            color: root.muted
            wrapMode: Text.WordWrap
            font.pixelSize: 12
            Layout.fillWidth: true
        }
    }
}
