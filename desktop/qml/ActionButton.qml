import QtQuick
import QtQuick.Controls

Button {
    id: action
    required property var theme
    property bool primary: false
    implicitHeight: 40
    implicitWidth: Math.max(96, implicitContentWidth + 32)
    padding: 12
    activeFocusOnTab: true
    Accessible.name: text
    contentItem: Text {
        text: action.text
        textFormat: Text.PlainText
        font: action.font
        color: action.primary ? (action.theme.dark ? "#062e26" : "white") : action.theme.ink
        horizontalAlignment: Text.AlignHCenter
        verticalAlignment: Text.AlignVCenter
        elide: Text.ElideRight
    }
    background: Rectangle {
        radius: 10
        color: action.primary ? action.theme.accent : (action.hovered ? action.theme.line : action.theme.active)
        border.width: action.activeFocus ? 2 : 0
        border.color: action.theme.accent
        opacity: action.enabled ? 1 : 0.4
    }
}
