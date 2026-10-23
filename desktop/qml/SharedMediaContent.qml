import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

ColumnLayout {
    id: root
    required property var theme
    required property var controller
    property bool olderPage: false
    property string scopeMode: "all"
    readonly property bool globalScope: scopeMode === "all"
    readonly property var page: globalScope
        ? (controller.routeMethod === "media.library" ? controller.routeData : ({}))
        : (controller.routeMethod === "media.list" && controller.routeData.chat_id === controller.selectedId ? controller.routeData : ({}))
    readonly property var items: globalScope ? (page.entries || []) : (page.messages || [])
    signal showMessageRequested(var message)
    spacing: 12

    function refresh(before) {
        if (!globalScope && !controller.selectedId)
            return
        const method = globalScope ? "media.library" : "media.list"
        let params = {limit: 50}
        if (!globalScope)
            params.chat_id = controller.selectedId
        const kind = mediaKind.model[mediaKind.currentIndex].kind
        if (kind.length > 0)
            params.kind = kind
        if (before && before.id)
            params.before = before
        if (controller.loadHub(method, params))
            olderPage = !!(before && before.id)
    }

    function begin() {
        scopeMode = controller.selectedId ? "chat" : "all"
        mediaScope.currentIndex = globalScope ? 0 : 1
        mediaKind.currentIndex = 0
        refresh(null)
    }

    Label {
        objectName: "sharedMediaScope"
        text: root.globalScope ? "All chats · local cache only" : "This conversation · local cache only"
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    ComboBox {
        id: mediaScope
        objectName: "sharedMediaScopeSelector"
        Layout.fillWidth: true
        textRole: "label"
        model: [{label: "All chats", scope: "all"}, {label: "This conversation", scope: "chat"}]
        enabled: !root.controller.routeLoading
        Accessible.name: "Choose which conversations to include in cached media"
        onActivated: {
            const nextScope = model[currentIndex].scope
            if (nextScope === "chat" && !root.controller.selectedId) {
                currentIndex = 0
                return
            }
            root.scopeMode = nextScope
            root.refresh(null)
        }
    }
    ComboBox {
        id: mediaKind
        objectName: "sharedMediaKind"
        Layout.fillWidth: true
        textRole: "label"
        model: [{label: "All media", kind: ""}, {label: "Photos", kind: "image"}, {label: "Videos", kind: "video"}, {label: "Audio and voice notes", kind: "audio"}, {label: "Documents", kind: "document"}, {label: "GIFs", kind: "gif"}, {label: "Stickers", kind: "sticker"}]
        enabled: !root.controller.routeLoading
        Accessible.name: "Filter cached media by type"
        onActivated: root.refresh(null)
    }
    Label {
        visible: root.controller.routeLoading || root.controller.routeError.length > 0
        text: root.controller.routeLoading ? "Loading local media…" : root.controller.routeError
        textFormat: Text.PlainText
        color: root.controller.routeError.length > 0 ? root.theme.danger : root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    ListView {
        objectName: "mediaLibraryItems"
        Layout.fillWidth: true
        Layout.fillHeight: true
        clip: true
        spacing: 4
        model: root.items
        ScrollBar.vertical: ScrollBar {}
        delegate: ItemDelegate {
            required property var modelData
            readonly property var record: modelData
            objectName: "sharedMediaRow"
            width: ListView.view.width
            enabled: !!record.chat_id && !!record.id &&
                     (root.globalScope || record.chat_id === root.controller.selectedId)
            onClicked: root.showMessageRequested(record)
            Accessible.name: (root.globalScope ? String(record.chat_title || "Unknown conversation") + ": " : "") +
                             String((record.attachment || {}).name || record.kind || "media") +
                             (record.body ? ", " + String(record.body) : "") + ", show in conversation"
            contentItem: ColumnLayout {
                spacing: 4
                Label {
                    objectName: "sharedMediaEntryName"
                    text: String((record.attachment || {}).name || record.kind || "Media")
                    textFormat: Text.PlainText
                    color: root.theme.ink
                    Layout.fillWidth: true
                    elide: Text.ElideMiddle
                }
                Label {
                    objectName: "sharedMediaChatTitle"
                    visible: root.globalScope
                    text: String(record.chat_title || "Unknown conversation")
                    textFormat: Text.PlainText
                    color: root.theme.muted
                    font.pixelSize: 11
                    Layout.fillWidth: true
                    elide: Text.ElideRight
                }
                Label {
                    objectName: "sharedMediaCaption"
                    visible: String(record.body || "").length > 0
                    text: String(record.body || "")
                    textFormat: Text.PlainText
                    color: root.theme.ink
                    Layout.fillWidth: true
                    wrapMode: Text.Wrap
                    maximumLineCount: 2
                    elide: Text.ElideRight
                }
                Label {
                    text: String(record.kind || "Media") + " · " + root.controller.timeLabel(record.timestamp) + " · Show in conversation"
                    textFormat: Text.PlainText
                    color: root.theme.muted
                    font.pixelSize: 11
                    Layout.fillWidth: true
                    elide: Text.ElideRight
                }
            }
            background: Rectangle {
                color: parent.hovered ? root.theme.chrome : root.theme.surface
                border.color: root.theme.line
                radius: 8
            }
        }
    }
    Label {
        visible: !root.controller.routeLoading && root.controller.routeError.length === 0 && root.items.length === 0
        text: root.globalScope
              ? "No matching media is retained in the local cache. Link-only messages and unavailable protected, view-once or disappearing media are omitted."
              : "No matching media is retained in this conversation. Link-only messages and unavailable protected, view-once or disappearing media are omitted."
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    RowLayout {
        Layout.fillWidth: true
        ActionButton {
            objectName: "sharedMediaNewest"
            theme: root.theme
            text: "Newest"
            enabled: !root.controller.routeLoading && root.olderPage
            onClicked: root.refresh(null)
        }
        Item { Layout.fillWidth: true }
        ActionButton {
            objectName: "sharedMediaOlder"
            theme: root.theme
            text: "Load older"
            enabled: !root.controller.routeLoading && !!root.page.has_more && !!root.page.next_cursor
            onClicked: root.refresh(root.page.next_cursor)
        }
    }
    Label {
        text: root.globalScope && root.page.disclosure
              ? String(root.page.disclosure)
              : "Only retained local media is shown; this is not a complete history. Link-only messages and unavailable protected, view-once or disappearing media are omitted. Nothing is downloaded or opened automatically."
        textFormat: Text.PlainText
        color: root.theme.muted
        font.pixelSize: 11
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
}
