import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

ItemDelegate {
    id: control
    required property var record
    required property var theme
    property string selectedId: ""
    property bool matchesFilter: true
    property bool canManageChat: false
    property var timeFormatter: function(timestamp) { return ""; }
    readonly property bool isFolder: record.row_type === "community"
    readonly property bool nested: Number(record.depth || 0) === 1
    signal chatActivated(string chatId)
    signal folderToggled(string communityId)
    signal manageChatRequested(var chat)
    objectName: isFolder ? "communityFolderRow" : nested ? "communityChildRow" : "chatRow"
    height: !matchesFilter ? 0 : isFolder ? 52 : nested ? 64 : 78
    visible: matchesFilter
    leftPadding: nested ? 38 : 14
    rightPadding: 16
    activeFocusOnTab: true
    enabled: !isFolder || !record.search_expanded
    Accessible.name: isFolder ? record.title + ", " + record.child_count + " loaded groups, " + (record.expanded ? "expanded" : "collapsed") + (record.unread ? ", " + record.unread + " unread messages" : "") : record.title
    Accessible.description: isFolder ? (record.search_expanded ? "Search results expand automatically. Clear search to collapse." : "Expand or collapse community groups. This does not mark messages read.") : "Open conversation"
    ToolTip.visible: hovered && isFolder
    ToolTip.text: record.search_expanded ? "Clear search to collapse" : record.child_count + " loaded groups"
    ToolTip.delay: 500
    onClicked: {
        if (isFolder) folderToggled(record.community_id);
        else chatActivated(record.id);
    }
    Keys.onRightPressed: function(event) {
        if (isFolder && !record.expanded) folderToggled(record.community_id);
        event.accepted = isFolder;
    }
    Keys.onLeftPressed: function(event) {
        if (isFolder && record.expanded) folderToggled(record.community_id);
        event.accepted = isFolder;
    }
    background: Rectangle {
        anchors.fill: parent
        anchors.leftMargin: 8
        anchors.rightMargin: 8
        radius: 8
        color: !control.isFolder && control.selectedId === control.record.id ? control.theme.active : control.hovered ? control.theme.chrome : control.theme.surface
        border.width: control.activeFocus ? 2 : 0
        border.color: control.theme.ink
    }
    contentItem: RowLayout {
        spacing: 12
        Canvas {
            visible: control.isFolder
            Layout.preferredWidth: 24
            Layout.preferredHeight: 24
            property color strokeColor: control.theme.ink
            onStrokeColorChanged: requestPaint()
            onPaint: {
                let c = getContext("2d");
                c.clearRect(0, 0, width, height); c.beginPath();
                c.strokeStyle = strokeColor; c.lineWidth = 1.5; c.lineJoin = "round";
                c.moveTo(2, 7); c.lineTo(2, 4); c.lineTo(9, 4); c.lineTo(12, 7); c.lineTo(21, 7);
                c.lineTo(18, 20); c.lineTo(3, 20); c.lineTo(1, 9); c.lineTo(21, 9); c.stroke();
            }
        }
        Rectangle {
            visible: !control.isFolder
            Layout.preferredWidth: control.nested ? 32 : 46
            Layout.preferredHeight: control.nested ? 32 : 46
            radius: width / 2
            color: control.record.color || control.theme.chrome
            Label {
                anchors.centerIn: parent
                text: control.record.initials || ""
                textFormat: Text.PlainText
                color: control.theme.dark || control.record.color ? "white" : control.theme.ink
                font.pixelSize: control.nested ? 12 : 15
            }
        }
        ColumnLayout {
            Layout.fillWidth: true
            spacing: 4
            Label {
                Layout.fillWidth: true
                text: control.record.title || ""
                textFormat: Text.PlainText
                color: control.theme.ink
                font.pixelSize: control.isFolder || control.nested ? 13 : 15
                font.weight: control.isFolder ? Font.DemiBold : Font.Normal
                elide: Text.ElideRight
            }
            Label {
                Layout.fillWidth: true
                visible: !control.isFolder
                text: control.record.preview || "No local messages"
                textFormat: Text.PlainText
                color: control.theme.muted
                font.pixelSize: 12
                elide: Text.ElideRight
            }
        }
        Label {
            visible: control.isFolder
            text: control.record.child_count + " groups"
            textFormat: Text.PlainText
            color: control.theme.muted
            font.pixelSize: 11
        }
        Label {
            visible: !control.isFolder && Number(control.record.timestamp || 0) > 0
            text: control.timeFormatter(control.record.timestamp || 0)
            textFormat: Text.PlainText
            color: control.theme.muted
            font.pixelSize: 10
        }
        Label {
            objectName: "chatUnreadCountBadge_" + control.record.id
            visible: Number(control.record.unread || control.record.unread_count || 0) > 0
            text: Number(control.record.unread || control.record.unread_count || 0) > 99 ? "99+" : String(Number(control.record.unread || control.record.unread_count || 0))
            textFormat: Text.PlainText
            Accessible.name: "Unread message count " + text
            color: control.theme.dark ? "#062e26" : "white"
            font.pixelSize: 10
            font.weight: Font.DemiBold
            leftPadding: 7
            rightPadding: 7
            topPadding: 3
            bottomPadding: 3
            background: Rectangle { radius: 10; color: control.theme.accent }
        }
        Label {
            objectName: "chatManualUnreadBadge_" + control.record.id
            visible: !!control.record.marked_unread
            text: "Unread"
            textFormat: Text.PlainText
            Accessible.name: "Manually marked unread"
            color: control.theme.ink
            font.pixelSize: 10
            leftPadding: 7
            rightPadding: 7
            topPadding: 3
            bottomPadding: 3
            background: Rectangle { radius: 10; color: control.theme.chrome; border.color: control.theme.line }
        }
        IconButton {
            objectName: "chatActionsButton"
            visible: !control.isFolder && control.canManageChat && control.hovered
            iconName: "more"
            label: "Manage chat " + control.record.title
            tint: control.theme.muted
            hoverColor: control.theme.active
            implicitWidth: 32
            implicitHeight: 32
            onClicked: control.manageChatRequested(control.record)
        }
        Canvas {
            visible: control.isFolder
            Layout.preferredWidth: 12
            Layout.preferredHeight: 12
            property bool expanded: !!control.record.expanded
            property color strokeColor: control.theme.muted
            onExpandedChanged: requestPaint()
            onStrokeColorChanged: requestPaint()
            onPaint: {
                let c=getContext("2d");c.clearRect(0,0,width,height);c.beginPath();c.strokeStyle=strokeColor;c.lineWidth=1.5;c.lineCap="round";
                if (expanded) { c.moveTo(2,4);c.lineTo(6,8);c.lineTo(10,4); }
                else { c.moveTo(4,2);c.lineTo(8,6);c.lineTo(4,10); }
                c.stroke();
            }
        }
    }
}
