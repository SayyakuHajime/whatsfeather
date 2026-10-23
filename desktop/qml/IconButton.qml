import QtQuick
import QtQuick.Controls

Button {
    id: control

    property string iconName: ""
    property string label: ""
    property color hoverColor: "#29343b"
    property color tint: "#aebac1"

    implicitWidth: 40
    implicitHeight: 40
    Accessible.name: label
    ToolTip.visible: hovered
    ToolTip.text: label
    ToolTip.delay: 400
    activeFocusOnTab: true
    display: AbstractButton.IconOnly
    icon.source: "qrc:/icons/" + iconName + ".svg"
    icon.color: tint
    icon.width: 21
    icon.height: 21

    background: Rectangle {
        radius: 20
        color: control.hovered ? control.hoverColor : "transparent"
        border.width: control.activeFocus ? 2 : 0
        border.color: "#7acda8"
    }

}
