import QtQuick
import QtQuick.Controls
import QtQuick.Dialogs
import QtQuick.Layouts

ApplicationWindow {
    id: app

    property bool dark: initialDark
    property bool groupsOnly: false
    onGroupsOnlyChanged: communityFolders.groupsOnly = groupsOnly
    property string chatFilter: "all"
    property var menuMessage: ({})
    property var menuChat: ({})
    property string replyText: ""
    property string editText: ""
    property string deleteScope: "local"
    property string forwardTargetChatId: ""
    property string selectedHubMethod: ""
    property string groupActionError: ""
    property bool selectionMode: false
    property var selectedMessageIds: []
    property string highlightedMessage: ""
    readonly property bool compact: width < 900
    readonly property color canvas: dark ? "#161717" : "#efeae2"
    readonly property color surface: dark ? "#161717" : "#ffffff"
    readonly property color chrome: dark ? "#242626" : "#f0f2f5"
    readonly property color rail: dark ? "#1d1f1f" : "#f0f2f5"
    readonly property color ink: dark ? "#fafafa" : "#172126"
    readonly property color muted: dark ? "#a7a8a8" : "#52616b"
    readonly property color line: dark ? "#343636" : "#d8dfe3"
    readonly property color active: dark ? "#2d2e2e" : "#e9edef"
    readonly property color outgoing: dark ? "#0b3b2e" : "#d9fdd3"
    readonly property color accent: dark ? "#00a884" : "#008069"
    readonly property color danger: dark ? "#ff9b8f" : "#b42318"
    readonly property var accountData: bridge.account || ({})
    property var demoProfileOverride: ({})
    property var pendingMediaNavigation: ({})

    function accountName() {
        if (bridge.demo && Object.prototype.hasOwnProperty.call(demoProfileOverride, "display_name"))
            return demoProfileOverride.display_name;
        return accountData.display_name || (accountData.available === false ? "Account unavailable" : "WhatsFeather account");
    }

    function accountAbout() {
        if (bridge.demo && Object.prototype.hasOwnProperty.call(demoProfileOverride, "about"))
            return demoProfileOverride.about;
        return accountData.about || "";
    }

    function accountInitials() {
        let name = accountName().trim();
        let parts = name.split(/\\s+/);
        return parts.length > 1 ? (parts[0][0] + parts[parts.length - 1][0]).toUpperCase() : (name[0] || "?").toUpperCase();
    }

    function featureAvailable(id) {
        let features = bridge.capabilities.features || ({});
        return !!features[id] && !!features[id].available;
    }

    function chatMatches(record) {
        if (groupsOnly && !record.is_group)
            return false;
        if (chatFilter === "unread")
            return !!record.marked_unread || Number(record.unread || record.unread_count || 0) > 0;
        if (chatFilter === "favorites")
            return !!record.favorite;
        if (chatFilter === "communities")
            return !!record.is_group && !!record.community_id && !!record.community_title;
        return true;
    }

    function communityRowMatches(record) {
        if (record.row_type !== "community")
            return chatMatches(record);
        if (chatFilter === "all")
            return true;
        for (let i = 0; i < bridge.chats.rowCount(); ++i) {
            let child = bridge.chats.row(i);
            if (child.community_id === record.community_id && chatMatches(child))
                return true;
        }
        return false;
    }

    function capabilityAvailable(method) {
        // QML bindings must depend on capability readiness as well as the
        // invokable lookup, otherwise they can stay stale after startup.
        const ready = bridge.capabilitiesReady;
        const snapshot = bridge.capabilities;
        return ready && !!snapshot && bridge.hasCapability(method);
    }

    function openMediaLibrary() {
        const method = bridge.selectedId ? "media.list" : "media.library"
        if (!app.capabilityAvailable(method))
            return;
        mediaLibrary.open();
    }

    function openSyntheticGroupCreator() {
        if (!bridge.demo || !app.capabilityAvailable("group.create"))
            return;
        app.groupActionError = "";
        createSyntheticGroupDialog.resetForm();
        createSyntheticGroupDialog.open();
    }

    function openHub(method) {
        if (!app.capabilityAvailable(method))
            return;
        selectedHubMethod = method;
        bridge.loadHub(method, {});
        hubDialog.open();
    }

    function selectMessage(message) {
        if (message.chat_id !== bridge.selectedId)
            return;
        selectionMode = true;
        if (selectedMessageIds.indexOf(message.id) < 0)
            selectedMessageIds = selectedMessageIds.concat([message.id]);
        messageMenu.close();
    }

    function copySelectedMessages() {
        let selected = [];
        for (let i = 0; i < bridge.messages.rowCount(); ++i) {
            let record = bridge.messages.row(i);
            if (selectedMessageIds.indexOf(record.id) >= 0)
                selected.push(record.body || record.attachment?.name || "");
        }
        bridge.copyText(selected.join("\\n"));
    }

    function openFind() {
        if (!bridge.selectedId)
            return ;

        findDrawer.open();
        messageSearch.forceActiveFocus();
    }

    function openMessageMenu(message, anchor) {
        if (message.chat_id !== bridge.selectedId || message.view_once || message.ephemeral || message.protected)
            return ;

        menuMessage = message;
        let p = anchor.mapToItem(app.contentItem, 0, anchor.height);
        messageMenu.x = Math.max(8, Math.min(app.width - messageMenu.width - 8, p.x));
        messageMenu.y = Math.max(8, Math.min(app.height - messageMenu.implicitHeight - 8, p.y));
        messageMenu.open();
    }

    property string attachmentMode: "photo"
    function contactValues(values) {
        if (!values || typeof values.length !== "number")
            return "";
        let output = [];
        for (let i = 0; i < values.length && i < 5; ++i)
            if (typeof values[i] === "string")
                output.push(values[i]);
        return output.join(", ");
    }
    function chooseAttachment(kind) {
        if (kind === "photo-album") {
            if (!bridge.demo || !app.featureAvailable("multi-attachment-albums") ||
                    !app.capabilityAvailable("attachment.album.prepare") ||
                    !app.capabilityAvailable("message.album.send") ||
                    !app.capabilityAvailable("attachment.album.release"))
                return;
            albumFilePicker.open();
            return;
        }
        if (kind === "location") {
            locationEntryDialog.beginEntry();
            locationEntryDialog.open();
            return;
        }
        if (kind === "contact-card") {
            contactCardFilePicker.open();
            return;
        }
        if (kind === "sticker") {
            attachmentMode = "sticker";
            filePicker.title = "Choose a static WebP sticker";
            filePicker.nameFilters = ["Static WebP stickers (*.webp)"];
        } else if (kind === "video") {
            attachmentMode = "video";
            filePicker.title = "Choose an MP4 video";
            filePicker.nameFilters = ["MP4 video (*.mp4)"];
        } else if (kind === "audio" || kind === "voice-note") {
            attachmentMode = kind;
            filePicker.title = kind === "voice-note" ? "Choose an OGG/Opus voice note" : "Choose an OGG/Opus audio file";
            filePicker.nameFilters = ["OGG/Opus audio (*.ogg)"];
        } else {
            attachmentMode = kind === "document" ? "document" : kind === "gif" ? "original" : "photo";
            filePicker.title = "Choose an attachment";
            filePicker.nameFilters = kind === "gif" ? ["GIF images (*.gif)"] : kind === "image" ? ["Photos and GIFs (*.png *.jpg *.jpeg *.gif)"] : ["All files (*)"];
        }
        filePicker.open();
    }
    function openMedia(record) {
        if (record.chat_id !== bridge.selectedId || !record.attachment || record.attachment.state !== "ready" || String(record.attachment.url || "").indexOf("file:///") !== 0) return;
        mediaViewer.record = record;
        mediaViewer.open();
    }

    objectName: "appWindow"
    width: 1180
    height: 780
    minimumWidth: 640
    minimumHeight: 480
    visible: true
    title: "WhatsFeather" + (bridge.demo ? " — Demo" : "")
    color: surface
    font.family: systemFont
    font.pixelSize: 14
    palette.window: surface
    palette.text: ink
    palette.windowText: ink
    palette.base: chrome
    palette.button: chrome
    palette.buttonText: ink
    palette.highlight: accent
    palette.highlightedText: "white"
    onDarkChanged: bridge.saveTheme(dark)
    onClosing: function(close) {
        if (!bridge.canQuit) {
            close.accepted = false;
            bridge.requestQuit();
        }
    }

    Shortcut {
        sequence: "Ctrl+K"
        onActivated: {
            if (app.compact)
                bridge.selectChat("");

            search.forceActiveFocus();
            search.selectAll();
        }
    }

    Shortcut {
        sequence: "Ctrl+F"
        onActivated: app.openFind()
    }

    Shortcut {
        sequence: "Escape"
        onActivated: {
            if (pollVoteDialog.opened) { pollVoteDialog.close(); return; }
            if (mediaViewer.opened) { mediaViewer.close(); return; }
            if (attachmentDialog.opened) { bridge.clearAttachment(); attachmentDialog.close(); return; }
            if (floating.dismissPopups())
                return ;

            if (settings.opened)
                settings.close();
            else if (profile.opened)
                profile.close();
            else if (findDrawer.opened)
                findDrawer.close();
            else if (app.compact)
                bridge.selectChat("");
            else
                search.clear();
        }
    }

    Connections {
        function onAttachmentChanged() {
            if (!bridge.attachment.name) attachmentDialog.close();
            else attachmentDialog.open();
        }
        function onRouteChanged() {
            const method = String(bridge.routeMethod || "");
            if (!method.startsWith("group.") || bridge.routeLoading)
                return;
            if (bridge.routeError.length > 0) {
                app.groupActionError = bridge.routeError;
                if (method !== "group.create" && bridge.selectedId.startsWith("demo:group:"))
                    bridge.loadHub("chat.info", {"chat_id": bridge.selectedId});
                return;
            }
            app.groupActionError = "";
            if (method === "group.create") {
                const createdChat = bridge.routeData.chat || ({});
                const createdId = String(createdChat.id || "");
                if (!createdId)
                    return;
                createSyntheticGroupDialog.close();
                search.clear();
                bridge.selectChat(createdId);
                conversationInfo.open();
                return;
            }
            if (bridge.selectedId.startsWith("demo:group:"))
                bridge.loadHub("chat.info", {"chat_id": bridge.selectedId});
        }
        function onSelectionChanged() {
            mediaLibrary.close();
            conversationInfo.close();
            pollVoteDialog.close();
            messageMenu.close();
            findDrawer.close();
            messageSearch.clear();
            app.highlightedMessage = "";
            mediaViewer.close();
        }

        target: bridge
    }

    RowLayout {
        anchors.fill: parent
        spacing: 0

        Rectangle {
            objectName: "navigationRail"
            visible: !app.compact
            Layout.preferredWidth: 64
            Layout.fillHeight: true
            color: app.rail

            ColumnLayout {
                anchors.fill: parent
                anchors.topMargin: 12
                anchors.bottomMargin: 12
                spacing: 12

                IconButton {
                    objectName: "navChats"
                    Layout.alignment: Qt.AlignHCenter
                    iconName: "chat"
                    label: "Chats"
                    tint: app.ink
                    hoverColor: app.active
                    onClicked: {
                        findDrawer.close();
                        search.clear();
                        search.forceActiveFocus();
                    }

                    background: Rectangle {
                        radius: 20
                        color: app.active
                    }

                }

                IconButton {
                    objectName: "navFind"
                    Layout.alignment: Qt.AlignHCenter
                    iconName: "search"
                    label: "Find in conversation"
                    enabled: bridge.selectedId !== ""
                    tint: app.muted
                    hoverColor: app.active
                    onClicked: app.openFind()
                }

                Item {
                    Layout.fillHeight: true
                }

                IconButton {
                    objectName: "mediaLibraryButton"
                    Layout.alignment: Qt.AlignHCenter
                    iconName: "file"
                    label: app.capabilityAvailable(bridge.selectedId ? "media.list" : "media.library")
                           ? "Shared media"
                           : "Shared media unavailable — " + bridge.capabilityReason(bridge.selectedId ? "media.list" : "media.library")
                    enabled: app.capabilityAvailable(bridge.selectedId ? "media.list" : "media.library")
                    tint: enabled ? app.accent : app.muted
                    hoverColor: app.active
                    onClicked: app.openMediaLibrary()
                }

                IconButton {
                    objectName: "settingsButton"
                    Layout.alignment: Qt.AlignHCenter
                    iconName: "settings"
                    label: "Settings"
                    tint: app.muted
                    hoverColor: app.active
                    onClicked: settings.open()
                }

                Button {
                    objectName: "profileButton"
                    Layout.alignment: Qt.AlignHCenter
                    implicitWidth: 40
                    implicitHeight: 40
                    Accessible.name: "Profile and account"
                    activeFocusOnTab: true
                    contentItem: Item { AccountAvatar { anchors.centerIn: parent; diameter: 34 } }
                    background: Rectangle { radius: 20; color: "transparent"; border.width: parent.activeFocus ? 2 : 0; border.color: app.accent }
                    ToolTip.visible: hovered
                    ToolTip.text: "Profile and account"
                    onClicked: profile.open()
                }

                IconButton {
                    objectName: "aboutButton"
                    Layout.alignment: Qt.AlignHCenter
                    iconName: "feather"
                    label: "About WhatsFeather"
                    tint: app.muted
                    hoverColor: app.active
                    onClicked: about.open()
                }

            }

        }

        Rectangle {
            id: sidebar

            objectName: "chatSidebar"
            visible: !app.compact || bridge.selectedId === ""
            Layout.preferredWidth: app.compact ? app.width : 330
            Layout.fillWidth: app.compact
            Layout.fillHeight: true
            color: app.surface

            ColumnLayout {
                anchors.fill: parent
                spacing: 0

                RowLayout {
                    Layout.fillWidth: true
                    Layout.preferredHeight: 66
                    Layout.leftMargin: 20
                    Layout.rightMargin: 12

                    Label {
                        textFormat: Text.PlainText
                        text: "WhatsFeather"
                        font.pixelSize: 22
                        font.weight: Font.DemiBold
                        color: app.ink
                        Layout.fillWidth: true
                    }

                    IconButton {
                        objectName: "newChatButton"
                        iconName: "chat"
                        label: app.capabilityAvailable("chat.open") ? "New chat" : "New chat unavailable — " + bridge.capabilityReason("chat.open")
                        enabled: app.capabilityAvailable("chat.open")
                        tint: enabled ? app.accent : app.muted
                        hoverColor: app.chrome
                        onClicked: { newChatDialog.open(); newChatRecipient.forceActiveFocus() }
                    }

                    IconButton {
                        objectName: "chatListOptionsButton"
                        iconName: "more"
                        label: "Chat list options"
                        tint: app.muted
                        hoverColor: app.chrome
                        onClicked: sidebarMenu.popup()
                    }

                }

                Rectangle {
                    Layout.fillWidth: true
                    Layout.preferredHeight: 40
                    Layout.leftMargin: 16
                    Layout.rightMargin: 16
                    color: app.chrome
                    radius: 22

                    RowLayout {
                        anchors.fill: parent
                        anchors.leftMargin: 10
                        anchors.rightMargin: 10
                        spacing: 8

                        IconButton {
                            iconName: "search"
                            label: "Focus chat search"
                            implicitWidth: 26
                            implicitHeight: 30
                            tint: app.muted
                            hoverColor: "transparent"
                            onClicked: search.forceActiveFocus()
                        }

                        TextField {
                            id: search

                            objectName: "chatSearch"
                            Layout.fillWidth: true
                            placeholderText: "Search conversations"
                            placeholderTextColor: app.muted
                            color: app.ink
                            selectByMouse: true
                            background: null
                            font.pixelSize: 13
                            onTextChanged: { communityFolders.query = text; bridge.search(text); }
                        }

                    }

                }

                RowLayout {
                    Layout.leftMargin: 16
                    Layout.topMargin: 10
                    Layout.bottomMargin: 10
                    spacing: 8

                    Repeater {
                        model: ["All", "Groups"]

                        delegate: Button {
                            required property int index
                            required property string modelData

                            objectName: index === 0 ? "chatFilterAllButton" : "chatFilterGroupsButton"
                            text: modelData
                            implicitHeight: 30
                            implicitWidth: index === 0 ? 48 : 76
                            onClicked: app.groupsOnly = index === 1

                            contentItem: Label {
                                textFormat: Text.PlainText
                                text: parent.text
                                color: ((index === 1) === app.groupsOnly) ? (app.dark ? "#b9edda" : "#005c4b") : app.muted
                                horizontalAlignment: Text.AlignHCenter
                                verticalAlignment: Text.AlignVCenter
                                font.pixelSize: 12
                            }

                            background: Rectangle {
                                radius: 15
                                color: ((index === 1) === app.groupsOnly) ? (app.dark ? "#123d34" : "#d9fdd3") : app.chrome
                            }

                        }

                    }

                    Item {
                        Layout.fillWidth: true
                    }

                }

                RowLayout {
                    objectName: "unreadFavoritesFilters"
                    visible: app.capabilityAvailable("chat.update")
                    Layout.fillWidth: true
                    Layout.leftMargin: 16
                    Layout.rightMargin: 16
                    spacing: 8
                    Repeater {
                        model: ["Unread", "Favorites"]
                        delegate: Button {
                            required property string modelData
                            required property int index
                            objectName: index === 0 ? "unreadChatFilterButton" : "favoriteChatFilterButton"
                            text: modelData
                            implicitHeight: 30
                            implicitWidth: 82
                            Accessible.name: "Filter " + modelData + " chats"
                            onClicked: app.chatFilter = app.chatFilter === (index === 0 ? "unread" : "favorites") ? "all" : (index === 0 ? "unread" : "favorites")
                            contentItem: Label {
                                textFormat: Text.PlainText
                                text: parent.text
                                color: app.chatFilter === (index === 0 ? "unread" : "favorites") ? app.ink : app.muted
                                horizontalAlignment: Text.AlignHCenter
                                verticalAlignment: Text.AlignVCenter
                                font.pixelSize: 12
                            }
                            background: Rectangle {
                                radius: 15
                                color: app.chatFilter === (index === 0 ? "unread" : "favorites") ? app.active : app.chrome
                            }
                        }
                    }
                }

                ListView {
                    id: chatList

                    objectName: "chatList"
                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    clip: true
                    model: communityFolders
                    reuseItems: true

                    Label {
                        objectName: "chatListEmptyState"
                        textFormat: Text.PlainText
                        anchors.centerIn: parent
                        text: bridge.connected ? (app.chatFilter === "all" && !app.groupsOnly ? "No conversations found" : "No chats match this filter") : "Starting backend…"
                        color: app.muted
                        visible: chatList.count === 0
                    }

                    ScrollBar.vertical: ScrollBar {
                        policy: ScrollBar.AsNeeded
                    }

                    delegate: CommunityFolderRow {
                        id: chatDelegate
                        width: ListView.view.width
                        theme: app
                        selectedId: bridge.selectedId
                        matchesFilter: app.communityRowMatches(record)
                        canManageChat: app.capabilityAvailable("chat.update")
                        timeFormatter: function(timestamp) { return bridge.timeLabel(timestamp); }
                        onChatActivated: function(chatId) { bridge.selectChat(chatId); }
                        onFolderToggled: function(communityId) { communityFolders.toggleFolder(communityId); }
                        onManageChatRequested: function(chat) { app.menuChat = chat; chatMenu.popup(); }
                    }

                }

                Label {
                    textFormat: Text.PlainText
                    Layout.fillWidth: true
                    Layout.margins: 14
                    text: bridge.demo ? "DEMO · synthetic data · local only" : bridge.status
                    color: app.muted
                    font.pixelSize: 10
                    elide: Text.ElideRight
                }

            }

        }

        Rectangle {
            visible: !app.compact
            Layout.preferredWidth: 1
            Layout.fillHeight: true
            color: app.line
        }

        Item {
            objectName: "conversationPane"
            visible: !app.compact || bridge.selectedId !== ""
            Layout.rightMargin: !app.compact && app.width >= 1080 ? findDrawer.width * findDrawer.position : 0
            Layout.fillWidth: true
            Layout.fillHeight: true

            Rectangle {
                anchors.fill: parent
                color: app.canvas
            }

            Rectangle {
                id: header

                anchors.left: parent.left
                anchors.right: parent.right
                anchors.top: parent.top
                height: 66
                color: app.surface

                RowLayout {
                    anchors.fill: parent
                    anchors.leftMargin: 16
                    anchors.rightMargin: 12
                    spacing: 12

                    IconButton {
                        visible: app.compact
                        iconName: "back"
                        label: "Back to conversations"
                        tint: app.muted
                        hoverColor: app.chrome
                        onClicked: bridge.selectChat("")
                    }

                    Rectangle {
                        visible: bridge.selectedId !== ""
                        width: 40
                        height: 40
                        radius: 20
                        color: "#526a63"

                        Label {
                            textFormat: Text.PlainText
                            anchors.centerIn: parent
                            text: bridge.selectedInitials
                            color: "white"
                            font.pixelSize: 14
                        }

                    }

                    ColumnLayout {
                        Layout.fillWidth: true
                        spacing: 4

                        Label {
                            objectName: "conversationTitle"
                            textFormat: Text.PlainText
                            text: bridge.selectedTitle || "WhatsFeather"
                            color: app.ink
                            font.pixelSize: 15
                            font.weight: Font.Medium
                            elide: Text.ElideRight
                            Layout.fillWidth: true
                        }

                        Label {
                            textFormat: Text.PlainText
                            text: bridge.selectedId ? (bridge.demo ? "Demo conversation" : bridge.status) : "Native messaging for Linux"
                            color: app.muted
                            font.pixelSize: 11
                            elide: Text.ElideRight
                            Layout.fillWidth: true
                        }

                    }

                    IconButton {
                        iconName: "video"
                        label: "Video calls are not supported yet"
                        enabled: false
                        tint: app.muted
                    }

                    IconButton {
                        iconName: "phone"
                        label: "Voice calls are not supported yet"
                        enabled: false
                        tint: app.muted
                    }

                    IconButton {
                        objectName: "findMessagesButton"
                        iconName: "search"
                        label: "Find messages in this conversation"
                        enabled: bridge.selectedId !== ""
                        tint: app.muted
                        hoverColor: app.chrome
                        onClicked: app.openFind()
                    }

                    IconButton {
                        objectName: "conversationMenuButton"
                        iconName: "more"
                        label: "Conversation options"
                        enabled: bridge.selectedId !== ""
                        tint: app.muted
                        hoverColor: app.chrome
                        onClicked: conversationMenu.popup()
                    }

                }

            }

            Rectangle {
                id: errorBanner

                anchors.top: header.bottom
                anchors.left: parent.left
                anchors.right: parent.right
                height: visible ? 56 : 0
                visible: bridge.error !== "" || bridge.notice !== ""
                color: app.dark ? "#4b3323" : "#fff0d4"

                RowLayout {
                    anchors.fill: parent
                    anchors.margins: 10

                    Label {
                        textFormat: Text.PlainText
                        text: bridge.error || bridge.notice
                        color: app.dark ? "#ffddb0" : "#613b0b"
                        wrapMode: Text.WordWrap
                        font.pixelSize: 12
                        Layout.fillWidth: true
                    }

                    Button {
                        text: "Dismiss"
                        onClicked: bridge.clearError()
                    }

                }

            }

            ChatWallpaper {
                anchors.top: errorBanner.bottom
                anchors.bottom: parent.bottom
                anchors.left: parent.left
                anchors.right: parent.right
                dark: app.dark
                visible: bridge.selectedId !== ""
            }

            Item {
                anchors.top: errorBanner.bottom
                anchors.bottom: parent.bottom
                anchors.left: parent.left
                anchors.right: parent.right
                visible: bridge.selectedId === ""

                ColumnLayout {
                    anchors.centerIn: parent
                    width: Math.min(parent.width - 64, 380)
                    spacing: 16

                    Image {
                        source: "qrc:/icons/feather.svg"
                        sourceSize.width: 60
                        sourceSize.height: 60
                        Layout.alignment: Qt.AlignHCenter
                    }

                    Label {
                        textFormat: Text.PlainText
                        text: "WhatsFeather"
                        font.pixelSize: 30
                        color: app.ink
                        Layout.alignment: Qt.AlignHCenter
                    }

                    Label {
                        textFormat: Text.PlainText
                        text: bridge.demo ? "Choose a synthetic conversation. Nothing is sent to WhatsApp." : "Open Settings to connect explicitly with a spare account."
                        color: app.muted
                        wrapMode: Text.WordWrap
                        horizontalAlignment: Text.AlignHCenter
                        Layout.fillWidth: true
                    }

                }

            }

            Rectangle {
                id: selectionToolbar
                objectName: "messageSelectionToolbar"
                anchors.top: errorBanner.bottom
                anchors.left: parent.left
                anchors.right: parent.right
                height: app.selectionMode ? 48 : 0
                visible: app.selectionMode
                color: app.surface
                RowLayout {
                    anchors.fill: parent; anchors.leftMargin: 16; anchors.rightMargin: 16; spacing: 10
                    Label { textFormat: Text.PlainText; text: app.selectedMessageIds.length + " selected"; color: app.ink; Layout.fillWidth: true }
                    PanelAction { objectName: "copySelectedMessagesButton"; text: "Copy"; enabled: app.selectedMessageIds.length > 0; onClicked: app.copySelectedMessages() }
                    PanelAction { objectName: "cancelMessageSelectionButton"; text: "Cancel"; onClicked: { app.selectionMode = false; app.selectedMessageIds = [] } }
                }
            }

            ListView {
                id: timeline

                property bool follow: true
                property string anchorId: ""
                property real anchorOffset: 0

                function rememberAnchor() {
                    let idx = indexAt(10, contentY + 10);
                    let item = itemAtIndex(idx);
                    if (item) {
                        anchorId = item.record.id;
                        anchorOffset = item.y - contentY;
                    }
                }

                function restoreAnchor() {
                    let idx = bridge.messages.indexOf(anchorId);
                    if (idx >= 0) {
                        forceLayout();
                        positionViewAtIndex(idx, ListView.Beginning);
                        contentY -= anchorOffset;
                    }
                }

                objectName: "messageTimeline"
                visible: bridge.selectedId !== ""
                anchors.top: selectionToolbar.visible ? selectionToolbar.bottom : errorBanner.bottom
                anchors.bottom: floating.top
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.margins: 16
                clip: true
                spacing: 8
                model: bridge.messages
                reuseItems: true
                cacheBuffer: Math.max(0, height)
                onMovementEnded: follow = atYEnd

                Connections {
                    function onSelectionChanged() {
                        timeline.follow = true;
                    }

                    function onTailUpdated() {
                        if (timeline.follow)
                            Qt.callLater(function() {
                            timeline.forceLayout();
                            timeline.positionViewAtEnd();
                        });

                    }

                    function onOlderLoaded() {
                        Qt.callLater(function() {
                            timeline.restoreAnchor();
                        });
                    }

                    function onMessageRevealed(id) {
                        timeline.follow = false;
                        app.highlightedMessage = id;
                        Qt.callLater(function() {
                            timeline.forceLayout();
                            let index = bridge.messages.indexOf(id);
                            if (index >= 0)
                                timeline.positionViewAtIndex(index, ListView.Center);

                        });
                    }

                    target: bridge
                }

                header: Item {
                    width: timeline.width
                    height: 52

                    Button {
                        anchors.centerIn: parent
                        flat: true
                        text: bridge.loading ? "Loading…" : bridge.hasMore ? "Load earlier messages" : "Start of available local history"
                        background: Rectangle { color: app.chrome; radius: 8; border.color: app.line }
                        enabled: bridge.hasMore && !bridge.loading
                        onClicked: {
                            timeline.follow = false;
                            timeline.rememberAnchor();
                            bridge.loadOlder();
                        }

                        contentItem: Label {
                            textFormat: Text.PlainText
                            text: parent.text
                            color: app.muted
                            horizontalAlignment: Text.AlignHCenter
                            font.pixelSize: 11
                        }

                    }

                }

                delegate: MessageBubble {
                    required property var model

                    record: model.record
                    theme: app
                    controller: bridge
                    highlighted: record.id === app.highlightedMessage
                    selected: app.selectedMessageIds.indexOf(record.id) >= 0
                    onMenuRequested: function(message, anchor) {
                        app.openMessageMenu(message, anchor);
                    }
                    onMediaRequested: function(message) { app.openMedia(message) }
                    onPollVoteRequested: function(message) {
                        if (message.chat_id !== bridge.selectedId || !app.capabilityAvailable("poll.vote")) return;
                        pollVoteDialog.targetRecord = message;
                        pollVoteDialog.selectedIndices = [];
                        pollVoteDialog.reviewing = false;
                        pollVoteDialog.open();
                    }
                }

                ScrollBar.vertical: ScrollBar {
                    policy: ScrollBar.AsNeeded
                }

            }

            Button {
                objectName: "jumpToLatestButton"
                anchors.right: parent.right
                anchors.bottom: floating.top
                anchors.margins: 20
                text: "Jump to latest"
                visible: bridge.selectedId !== "" && (!timeline.follow || bridge.historyWindow)
                onClicked: {
                    timeline.follow = true;
                    app.highlightedMessage = "";
                    bridge.latest();
                }
            }

            FloatingComposer {
                id: floating

                objectName: "floatingComposer"
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.bottom: parent.bottom
                height: implicitHeight
                visible: bridge.selectedId !== ""
                theme: app
                controller: bridge
                onBeforeSend: {
                    timeline.follow = true;
                    if (bridge.historyWindow)
                        bridge.latest();

                }
                onAttachRequested: function(kind) {
                    app.chooseAttachment(kind);
                }
            }

        }

    }

    Dialog {
        id: communityRefreshDialog
        objectName: "communityRefreshDialog"
        anchors.centerIn: parent
        width: Math.min(440, app.width - 32)
        title: "Refresh community folders?"
        modal: true
        padding: 20
        standardButtons: Dialog.NoButton
        background: Rectangle { color: app.surface; radius: 12; border.color: app.line }
        onAccepted: bridge.refreshCommunities()
        contentItem: Label {
            textFormat: Text.PlainText
            text: "Read community and group names and their membership links from WhatsApp. No messages will be downloaded, marked read or sent. No groups will be joined or changed. Your existing folders remain if the refresh fails."
            color: app.ink
            wrapMode: Text.WordWrap
        }
        footer: Item {
            implicitHeight: 68
            RowLayout {
                anchors.right: parent.right
                anchors.rightMargin: 16
                anchors.verticalCenter: parent.verticalCenter
                spacing: 8
                PanelAction { objectName: "cancelCommunityRefresh"; text: "Cancel"; onClicked: communityRefreshDialog.reject() }
                PanelAction {
                    objectName: "confirmCommunityRefresh"
                    text: "Refresh"
                    primary: true
                    enabled: !bridge.demo && bridge.connectionState === "connected" && app.capabilityAvailable("community.refresh") && !bridge.communitiesRefreshing
                    onClicked: communityRefreshDialog.accept()
                }
            }
        }
    }

    Menu {
        id: sidebarMenu

        MenuItem {
            text: "Message yourself"
            enabled: !!bridge.account.available && (bridge.demo || bridge.connectionState === "connected")
            onTriggered: bridge.openSelfChat()
        }

        MenuItem {
            objectName: "createSyntheticGroupAction"
            text: "Create synthetic demo group"
            visible: bridge.demo && app.capabilityAvailable("group.create")
            enabled: visible && !bridge.routeLoading
            onTriggered: app.openSyntheticGroupCreator()
        }

        MenuItem { text: "Groups"; visible: app.capabilityAvailable("group.list"); enabled: app.capabilityAvailable("group.list"); onTriggered: app.openHub("group.list") }
        MenuItem { text: "Status"; visible: app.capabilityAvailable("status.list"); enabled: app.capabilityAvailable("status.list"); onTriggered: app.openHub("status.list") }
        MenuItem { text: "Channels"; visible: app.capabilityAvailable("channel.list"); enabled: app.capabilityAvailable("channel.list"); onTriggered: app.openHub("channel.list") }
        MenuItem {
            text: "Community folders"
            visible: app.capabilityAvailable("community.list")
            checkable: true
            checked: app.chatFilter === "communities"
            onTriggered: {
                app.chatFilter = "communities";
                app.groupsOnly = false;
                search.clear();
                if (app.compact) bridge.selectChat("");
                chatList.positionViewAtBeginning();
            }
        }
        MenuItem {
            objectName: "refreshCommunityFoldersMenu"
            text: bridge.communitiesRefreshing ? "Refreshing community folders…" : "Refresh community folders"
            visible: app.capabilityAvailable("community.refresh")
            enabled: !bridge.demo && bridge.connectionState === "connected" && !bridge.communitiesRefreshing
            onTriggered: communityRefreshDialog.open()
        }

        MenuItem {
            text: "Profile and account"
            onTriggered: profile.open()
        }

        MenuItem {
            text: "Settings"
            onTriggered: settings.open()
        }

        MenuItem {
            text: "About WhatsFeather"
            onTriggered: about.open()
        }

    }

    Menu {
        id: chatMenu
        objectName: "chatManagementMenu"
        MenuItem { objectName: "archiveChatAction"; text: menuChat.archived ? "Unarchive chat" : "Archive chat"; onTriggered: bridge.updateChat(menuChat.id, {"archived": !menuChat.archived}) }
        MenuItem { objectName: "pinChatAction"; text: menuChat.pinned ? "Unpin chat" : "Pin chat"; onTriggered: bridge.updateChat(menuChat.id, {"pinned": !menuChat.pinned}) }
        MenuItem { objectName: "muteChatAction"; text: menuChat.muted ? "Unmute chat" : "Mute chat"; onTriggered: bridge.updateChat(menuChat.id, {"muted": !menuChat.muted}) }
        MenuItem { objectName: "markUnreadChatAction"; text: menuChat.marked_unread ? "Clear unread mark" : "Mark unread"; onTriggered: bridge.updateChat(menuChat.id, {"unread": !menuChat.marked_unread}) }
        MenuItem { objectName: "favoriteChatAction"; text: menuChat.favorite ? "Remove from Favorites" : "Add to Favorites"; onTriggered: bridge.updateChat(menuChat.id, {"favorite": !menuChat.favorite}) }
    }

    Menu {
        id: conversationMenu

        objectName: "conversationMenu"

        MenuItem {
            text: "Find messages"
            onTriggered: app.openFind()
        }

        MenuItem {
            objectName: "requestOlderHistoryAction"
            text: "Request up to 50 older messages from phone"
            enabled: !bridge.demo && bridge.connectionState === "connected" && bridge.selectedId !== ""
            onTriggered: bridge.refreshHistory()
        }

        MenuItem {
            text: app.capabilityAvailable("chat.info") ? "Conversation info" : "Conversation info unavailable"
            enabled: app.capabilityAvailable("chat.info")
            onTriggered: conversationInfo.open()
        }

        MenuSeparator {
        }

        MenuItem {
            text: "Settings"
            onTriggered: settings.open()
        }

    }

    Menu {
        id: messageMenu

        objectName: "messageMenu"

        MenuItem {
            objectName: "protocolReplyAction"
            text: "Reply"
            visible: app.capabilityAvailable("message.reply")
            onTriggered: { app.replyText = ""; replyDialog.open(); messageMenu.close() }
        }

        Menu {
            title: "React"
            objectName: "messageReactionMenu"
            enabled: app.capabilityAvailable("message.react")
            MenuItem { objectName: "reactThumbsUpAction"; text: "👍"; Accessible.name: "React with thumbs up"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": "👍"}) }
            MenuItem { text: "❤️"; Accessible.name: "React with heart"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": "❤️"}) }
            MenuItem { text: "😂"; Accessible.name: "React with laughter"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": "😂"}) }
            MenuItem { text: "😮"; Accessible.name: "React with surprise"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": "😮"}) }
            MenuItem { text: "🙏"; Accessible.name: "React with thanks"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": "🙏"}) }
            MenuItem { text: "Remove reaction"; onTriggered: bridge.performMessageAction("message.react", app.menuMessage, {"reaction": ""}) }
        }

        MenuItem {
            objectName: "editMessageAction"
            text: "Edit message"
            visible: app.capabilityAvailable("message.edit") && !!app.menuMessage.outgoing
            onTriggered: { app.editText = app.menuMessage.body || ""; editDialog.open(); messageMenu.close() }
        }

        MenuItem {
            objectName: "forwardMessageAction"
            text: "Forward"
            visible: app.capabilityAvailable("message.forward")
            onTriggered: { app.forwardTargetChatId = ""; forwardDialog.open(); messageMenu.close() }
        }

        MenuItem {
            objectName: "starMessageAction"
            text: app.menuMessage.starred ? "Unstar message" : "Star message"
            visible: app.capabilityAvailable("message.star")
            onTriggered: bridge.performMessageAction("message.star", app.menuMessage, {"starred": !app.menuMessage.starred})
        }

        MenuItem {
            objectName: "pinMessageAction"
            text: app.menuMessage.pinned ? "Unpin message" : "Pin message"
            visible: app.capabilityAvailable("message.pin")
            onTriggered: bridge.performMessageAction("message.pin", app.menuMessage, {"pinned": !app.menuMessage.pinned, "duration_seconds": 0})
        }

        MenuItem {
            objectName: "deleteLocalMessageAction"
            text: "Delete for me…"
            visible: app.capabilityAvailable("message.delete")
            onTriggered: { app.deleteScope = "local"; deleteMessageDialog.open(); messageMenu.close() }
        }

        MenuItem {
            objectName: "deleteEveryoneMessageAction"
            text: "Delete for everyone…"
            visible: app.capabilityAvailable("message.delete") && !!app.menuMessage.outgoing
            onTriggered: { app.deleteScope = "everyone"; deleteMessageDialog.open(); messageMenu.close() }
        }

        MenuItem {
            objectName: "selectMessageAction"
            text: "Select message"
            onTriggered: app.selectMessage(app.menuMessage)
        }

        MenuItem {
            objectName: "copySelectedMessagesAction"
            text: "Copy selected messages"
            visible: app.selectionMode && app.selectedMessageIds.length > 0
            onTriggered: app.copySelectedMessages()
        }

        MenuSeparator { visible: app.capabilityAvailable("message.reply") || app.capabilityAvailable("message.react") }

        MenuItem {
            objectName: "copyMessageAction"
            text: "Copy"
            onTriggered: bridge.copyText(app.menuMessage.body || (app.menuMessage.attachment ? app.menuMessage.attachment.name : ""))
        }

        MenuItem {
            objectName: "quoteMessageAction"
            text: "Quote in draft"
            onTriggered: {
                bridge.quoteMessage(app.menuMessage);
                floating.focusEditor();
            }
        }

        MenuSeparator {
        }

        MenuItem {
            objectName: "messageInfoAction"
            text: "Message info"
            onTriggered: messageInfo.open()
        }

    }

    Drawer {
        id: findDrawer

        objectName: "findDrawer"
        edge: Qt.RightEdge
        width: Math.min(340, app.width - 64)
        height: app.height
        y: 0
        modal: false
        dim: false
        interactive: false

        background: Rectangle {
            color: app.surface
            border.color: app.line
        }

        contentItem: ColumnLayout {
            spacing: 12

            RowLayout {
                Layout.fillWidth: true
                Layout.margins: 12

                IconButton {
                    iconName: "close"
                    label: "Close message search"
                    tint: app.muted
                    hoverColor: app.chrome
                    onClicked: findDrawer.close()
                }

                Label {
                    textFormat: Text.PlainText
                    text: "Search messages"
                    color: app.ink
                    font.weight: Font.DemiBold
                    Layout.fillWidth: true
                }

            }

            TextField {
                id: messageSearch

                objectName: "messageSearch"
                Layout.fillWidth: true
                Layout.leftMargin: 16
                Layout.rightMargin: 16
                placeholderText: "Find in this conversation"
                color: app.ink
                placeholderTextColor: app.muted
                selectByMouse: true
                onTextChanged: bridge.findMessages(text)

                background: Rectangle {
                    color: app.chrome
                    radius: 20
                }

            }

            Label {
                textFormat: Text.PlainText
                Layout.leftMargin: 16
                Layout.rightMargin: 16
                text: bridge.searching ? "Searching local history…" : bridge.searchResults.rowCount() > 0 ? "Select a result to jump to it" : "Search messages stored on this device"
                color: app.muted
                font.pixelSize: 11
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }

            ListView {
                objectName: "messageSearchResults"
                Layout.fillWidth: true
                Layout.fillHeight: true
                clip: true
                model: bridge.searchResults

                delegate: ItemDelegate {
                    required property var record

                    width: ListView.view.width
                    implicitHeight: 92
                    onClicked: bridge.revealMessage(record.id)

                    background: Rectangle {
                        color: parent.hovered ? app.chrome : app.surface
                    }

                    contentItem: ColumnLayout {
                        spacing: 6

                        Label {
                            textFormat: Text.PlainText
                            text: new Date(record.timestamp).toLocaleDateString(Qt.locale(), "dd MMM yyyy") + " · " + bridge.timeLabel(record.timestamp)
                            color: app.muted
                            font.pixelSize: 10
                        }

                        Label {
                            textFormat: Text.PlainText
                            text: record.body
                            color: app.ink
                            wrapMode: Text.WordWrap
                            maximumLineCount: 3
                            elide: Text.ElideRight
                            Layout.fillWidth: true
                        }

                    }

                }

            }

        }

    }

    Dialog {
        id: pollVoteDialog
        objectName: "pollVoteDialog"
        property var targetRecord: ({})
        property var selectedIndices: []
        property bool reviewing: false
        readonly property var poll: targetRecord.poll || ({})
        readonly property int maxChoices: Number(poll.selectable_options_count || (poll.options || []).length)
        anchors.centerIn: parent
        width: Math.min(480, app.width - 32)
        height: Math.min(540, app.height - 32)
        modal: true
        padding: 20
        standardButtons: Dialog.NoButton
        onClosed: { selectedIndices = []; reviewing = false; targetRecord = ({}); }
        onOpened: pollVoteCancel.forceActiveFocus()
        background: Rectangle { color: app.surface; radius: 16; border.color: app.line }
        header: Label {
            textFormat: Text.PlainText
            text: pollVoteDialog.reviewing ? "Confirm vote" : "Choose poll options"
            color: app.ink
            font.pixelSize: 20
            font.weight: Font.DemiBold
            padding: 20
        }
        contentItem: ScrollView {
            id: pollVoteScroll
            clip: true
            contentWidth: availableWidth
            ColumnLayout {
                width: pollVoteScroll.availableWidth
                spacing: 12
                Label {
                    textFormat: Text.PlainText
                    text: String(pollVoteDialog.poll.question || "")
                    color: app.ink
                    font.weight: Font.DemiBold
                    wrapMode: Text.WrapAnywhere
                    Layout.fillWidth: true
                }
                Label {
                    textFormat: Text.PlainText
                    text: pollVoteDialog.reviewing ? "Only Send vote submits these choices. Cancel sends nothing." : "Choose up to " + pollVoteDialog.maxChoices + ". Nothing is sent before confirmation."
                    color: app.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                Repeater {
                    model: pollVoteDialog.poll.options || []
                    delegate: CheckBox {
                        id: pollChoice
                        required property int index
                        required property var modelData
                        objectName: "pollVoteChoice" + (index + 1)
                        Layout.fillWidth: true
                        visible: !pollVoteDialog.reviewing
                        checked: pollVoteDialog.selectedIndices.indexOf(index) >= 0
                        enabled: checked || pollVoteDialog.selectedIndices.length < pollVoteDialog.maxChoices
                        Accessible.name: "Option " + (index + 1) + ": " + String(modelData)
                        contentItem: Label {
                            textFormat: Text.PlainText
                            text: String(pollChoice.modelData)
                            color: app.ink
                            wrapMode: Text.WrapAnywhere
                            leftPadding: pollChoice.indicator.width + pollChoice.spacing
                            verticalAlignment: Text.AlignVCenter
                        }
                        onToggled: {
                            let values = pollVoteDialog.selectedIndices.slice();
                            let position = values.indexOf(index);
                            if (checked && position < 0) values.push(index);
                            else if (!checked && position >= 0) values.splice(position, 1);
                            pollVoteDialog.selectedIndices = values.sort(function(a, b) { return a - b; });
                        }
                    }
                }
                Label {
                    objectName: "pollVoteReviewChoices"
                    visible: pollVoteDialog.reviewing
                    textFormat: Text.PlainText
                    text: pollVoteDialog.selectedIndices.map(function(i) { return String((pollVoteDialog.poll.options || [])[i] || ""); }).join("\n")
                    color: app.ink
                    wrapMode: Text.WrapAnywhere
                    Layout.fillWidth: true
                }
            }
        }
        footer: Item {
            implicitHeight: 72
            RowLayout {
                anchors.fill: parent
                anchors.margins: 16
                spacing: 8
                ActionButton {
                    id: pollVoteCancel
                    objectName: "pollVoteCancel"
                    theme: app
                    text: "Cancel"
                    onClicked: pollVoteDialog.close()
                }
                Item { Layout.fillWidth: true }
                ActionButton {
                    objectName: "pollVoteReview"
                    theme: app
                    text: "Review vote"
                    visible: !pollVoteDialog.reviewing
                    enabled: pollVoteDialog.selectedIndices.length > 0
                    onClicked: pollVoteDialog.reviewing = true
                }
                ActionButton {
                    objectName: "pollVoteConfirm"
                    theme: app
                    text: "Send vote"
                    primary: true
                    visible: pollVoteDialog.reviewing
                    enabled: pollVoteDialog.targetRecord.chat_id === bridge.selectedId && app.capabilityAvailable("poll.vote") && bridge.pollVoteBlockedTargets.indexOf(pollVoteDialog.targetRecord.id) < 0
                    onClicked: {
                        if (bridge.performMessageAction("poll.vote", pollVoteDialog.targetRecord, {option_indices: pollVoteDialog.selectedIndices})) pollVoteDialog.close();
                    }
                }
            }
        }
    }

    Dialog {
        id: newChatDialog
        objectName: "newChatDialog"
        anchors.centerIn: parent
        width: Math.min(520, app.width - 32)
        height: Math.min(620, app.height - 32)
        modal: true
        padding: 0
        standardButtons: Dialog.NoButton
        title: "New chat"
        onOpened: {
            newChatRecipient.forceActiveFocus()
            bridge.searchContacts(newChatRecipient.text)
        }
        background: Rectangle { color: app.surface; radius: 16; border.color: app.line }
        header: Item {
            implicitHeight: 64
            Label { anchors.left: parent.left; anchors.leftMargin: 22; anchors.verticalCenter: parent.verticalCenter; text: "New chat"; color: app.ink; font.pixelSize: 20; font.weight: Font.DemiBold }
        }
        contentItem: ColumnLayout {
            spacing: 10
            TextField {
                id: newChatRecipient
                objectName: "newChatRecipient"
                Layout.fillWidth: true
                Layout.leftMargin: 20
                Layout.rightMargin: 20
                placeholderText: "Search contacts or enter a recipient"
                color: app.ink
                placeholderTextColor: app.muted
                selectByMouse: true
                maximumLength: 100
                onTextChanged: bridge.searchContacts(text)
                background: Rectangle { color: app.chrome; radius: 10; border.color: parent.activeFocus ? app.accent : app.line }
                Keys.onReturnPressed: if (app.capabilityAvailable("chat.open") && text.trim().length > 0) { bridge.openChatRecipient(text); newChatDialog.close() }
            }
            Label {
                objectName: "contactLookupStatus"
                textFormat: Text.PlainText
                text: app.capabilityAvailable("contact.list") ? (bridge.contactsLoading ? "Looking up contacts…" : bridge.contactsError || "Search results come from the linked service.") : bridge.capabilityReason("contact.list")
                color: bridge.contactsError ? app.danger : app.muted
                wrapMode: Text.WordWrap
                Layout.leftMargin: 20
                Layout.rightMargin: 20
                Layout.fillWidth: true
            }
            ListView {
                objectName: "contactLookupResults"
                Layout.fillWidth: true
                Layout.fillHeight: true
                clip: true
                model: bridge.contacts
                delegate: ItemDelegate {
                    required property var record
                    objectName: "contactLookupRow"
                    width: ListView.view.width
                    enabled: !!record.phone
                    Accessible.name: (record.display_name || record.name || "Contact") + (record.phone ? ", " + record.phone : "")
                    onClicked: { bridge.openChatRecipient("+" + record.phone); newChatDialog.close() }
                    contentItem: RowLayout {
                        Label { textFormat: Text.PlainText; text: record.display_name || record.name || "Contact"; color: app.ink; Layout.fillWidth: true; elide: Text.ElideRight }
                        Label { textFormat: Text.PlainText; text: record.phone || ""; color: app.muted; font.pixelSize: 12 }
                    }
                    background: Rectangle { color: parent.hovered ? app.chrome : app.surface }
                }
            }
            PanelAction {
                objectName: "loadMoreContacts"
                text: bridge.contactsLoading ? "Loading contacts…" : "Load more contacts"
                visible: bridge.contactsHasMore
                enabled: !bridge.contactsLoading
                Layout.leftMargin: 20
                Layout.rightMargin: 20
                Layout.alignment: Qt.AlignHCenter
                onClicked: bridge.loadMoreContacts()
            }
            Label {
                visible: app.capabilityAvailable("contact.list") && bridge.contacts.rowCount() === 0 && !bridge.contactsLoading && !bridge.contactsHasMore
                textFormat: Text.PlainText
                text: newChatRecipient.text.trim().length ? "No verified contacts found. You can enter a recipient only if the backend supports verification." : "No contacts to show yet."
                color: app.muted
                wrapMode: Text.WordWrap
                Layout.leftMargin: 20
                Layout.rightMargin: 20
                Layout.fillWidth: true
            }
        }
        footer: Item {
            implicitHeight: 72
            RowLayout {
                anchors.right: parent.right; anchors.rightMargin: 20; anchors.verticalCenter: parent.verticalCenter; spacing: 8
                PanelAction { text: "Cancel"; onClicked: newChatDialog.close() }
                PanelAction {
                    objectName: "openVerifiedChat"
                    text: "Continue"
                    primary: true
                    enabled: app.capabilityAvailable("chat.open") && newChatRecipient.text.trim().length > 0
                    onClicked: { bridge.openChatRecipient(newChatRecipient.text); newChatDialog.close() }
                }
            }
        }
    }

    Drawer {
        id: mediaLibrary
        objectName: "mediaLibraryDrawer"
        edge: Qt.RightEdge
        width: Math.min(420, app.width - 48)
        height: app.height
        modal: true
        interactive: true
        onOpened: sharedMediaContent.begin()
        onClosed: {
            const target = app.pendingMediaNavigation
            app.pendingMediaNavigation = ({})
            if (target && target.chat_id && target.id)
                bridge.revealMediaLibraryMessage(String(target.chat_id), String(target.id))
        }
        background: Rectangle { color: app.surface; border.color: app.line }
        contentItem: Item {
            ColumnLayout {
                anchors.fill: parent; anchors.margins: 16; spacing: 12
                RowLayout {
                    Layout.fillWidth: true
                    Label { text: "Shared media"; color: app.ink; font.pixelSize: 20; font.weight: Font.DemiBold; Layout.fillWidth: true }
                    IconButton { iconName: "close"; label: "Close shared media"; tint: app.muted; hoverColor: app.chrome; onClicked: mediaLibrary.close() }
                }
                Label { text: sharedMediaContent.scopeMode === "all" ? "All chats" : bridge.selectedTitle; textFormat: Text.PlainText; color: app.muted; elide: Text.ElideRight; Layout.fillWidth: true }
                SharedMediaContent {
                    id: sharedMediaContent
                    Layout.fillWidth: true; Layout.fillHeight: true
                    theme: app; controller: bridge
                    onShowMessageRequested: function(message) {
                        if (!message || !message.chat_id || !message.id)
                            return
                        app.pendingMediaNavigation = ({chat_id: message.chat_id, id: message.id})
                        mediaLibrary.close();
                    }
                }
            }
        }
    }

    Dialog {
        id: hubDialog
        objectName: "capabilityHubDialog"
        anchors.centerIn: parent
        width: Math.min(560, app.width - 32)
        height: Math.min(620, app.height - 32)
        modal: true
        padding: 18
        title: selectedHubMethod
        standardButtons: Dialog.Close
        contentItem: ColumnLayout {
            spacing: 10
            Label { textFormat: Text.PlainText; text: bridge.routeLoading ? "Loading from the local backend…" : bridge.routeError; visible: bridge.routeLoading || bridge.routeError !== ""; color: bridge.routeError ? app.danger : app.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true }
            TextArea { objectName: "hubProjection"; readOnly: true; selectByMouse: true; textFormat: TextEdit.PlainText; wrapMode: TextEdit.Wrap; color: app.ink; text: JSON.stringify(bridge.routeData, null, 2); Layout.fillWidth: true; Layout.fillHeight: true; Accessible.name: "Backend hub results" }
        }
    }

    FileDialog {
        id: filePicker

        objectName: "attachmentFilePicker"
        title: "Choose an attachment"
        fileMode: FileDialog.OpenFile
        onAccepted: { bridge.stageAttachmentAs(selectedFile, app.attachmentMode); attachmentDialog.open() }
    }

    FileDialog {
        id: contactCardFilePicker
        objectName: "contactCardFilePicker"
        title: "Choose a contact vCard"
        fileMode: FileDialog.OpenFile
        nameFilters: ["vCard contact files (*.vcf)"]
        onAccepted: bridge.stageContactFile(selectedFile)
    }

    FileDialog {
        id: albumFilePicker
        objectName: "albumFilePicker"
        title: "Choose 2 to 10 local still images"
        fileMode: FileDialog.OpenFiles
        nameFilters: ["Still images (*.png *.jpg *.jpeg *.webp)"]
        onAccepted: bridge.stageAttachmentAlbum(selectedFiles)
    }

    Connections {
        target: bridge
        function onLocationShareChanged() {
            if (bridge.locationShareAccepted && locationEntryDialog.opened)
                locationEntryDialog.close();
        }
    }

    Dialog {
        id: locationEntryDialog
        objectName: "locationEntryDialog"
        anchors.centerIn: parent
        width: Math.min(480, app.width - 32)
        height: Math.min(560, app.height - 32)
        modal: true
        title: reviewing ? "Review static location" : "Enter static location"
        padding: 18
        standardButtons: Dialog.NoButton
        closePolicy: bridge.locationSending ? Popup.NoAutoClose : Popup.CloseOnEscape
        property bool reviewing: false
        property var reviewedLocation: ({})
        property string localError: ""
        function beginEntry() {
            reviewing = false;
            reviewedLocation = ({});
            localError = "";
            locationLatitudeInput.text = "";
            locationLongitudeInput.text = "";
            locationCaptionInput.text = "";
            locationNameInput.text = "";
            locationAddressInput.text = "";
            bridge.clearLocationShare();
        }
        function validCoordinate(text, minimum, maximum) {
            const value = String(text).trim();
            const decimal = /^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/;
            if (value.length === 0 || value.length > 32 || !decimal.test(value))
                return false;
            const number = Number(value);
            return Number.isFinite(number) && number >= minimum && number <= maximum;
        }
        function reviewEntry() {
            const caption = String(locationCaptionInput.text);
            const name = String(locationNameInput.text);
            const address = String(locationAddressInput.text);
            const values = [caption, name, address];
            if (!validCoordinate(locationLatitudeInput.text, -90, 90) ||
                    !validCoordinate(locationLongitudeInput.text, -180, 180)) {
                localError = "Enter a finite latitude (−90…90) and longitude (−180…180).";
                return;
            }
            for (let i = 0; i < values.length; ++i) {
                if (Array.from(values[i]).length > 256 || /[\u0000-\u001f\u007f-\u009f]/.test(values[i])) {
                    localError = "Optional text must be at most 256 characters and contain no control characters.";
                    return;
                }
            }
            localError = "";
            reviewedLocation = {
                latitude: Number(String(locationLatitudeInput.text).trim()),
                longitude: Number(String(locationLongitudeInput.text).trim()),
                caption: caption,
                name: name,
                address: address,
                reviewed: true
            };
            reviewing = true;
        }
        background: Rectangle { color: app.surface; radius: 14; border.color: app.line }
        onClosed: if (!bridge.locationSending) bridge.clearLocationShare()

        contentItem: ScrollView {
            id: locationEntryScroll
            objectName: "locationEntryScroll"
            clip: true
            contentWidth: availableWidth
            contentHeight: locationEntryContent.implicitHeight

            ColumnLayout {
                id: locationEntryContent
                width: locationEntryScroll.availableWidth
                spacing: 10

                Label {
                    textFormat: Text.PlainText
                    text: "Coordinates are entered manually. This sends a static location, not live tracking."
                    color: app.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                ColumnLayout {
                    visible: !locationEntryDialog.reviewing
                    Layout.fillWidth: true
                    spacing: 8
                    Label { textFormat: Text.PlainText; text: "Latitude"; color: app.ink }
                    TextField {
                        id: locationLatitudeInput
                        objectName: "locationLatitudeInput"
                        placeholderText: "−90 to 90"
                        inputMethodHints: Qt.ImhFormattedNumbersOnly
                        selectByMouse: true
                        Layout.fillWidth: true
                        Accessible.name: "Manual location latitude"
                    }
                    Label { textFormat: Text.PlainText; text: "Longitude"; color: app.ink }
                    TextField {
                        id: locationLongitudeInput
                        objectName: "locationLongitudeInput"
                        placeholderText: "−180 to 180"
                        inputMethodHints: Qt.ImhFormattedNumbersOnly
                        selectByMouse: true
                        Layout.fillWidth: true
                        Accessible.name: "Manual location longitude"
                    }
                    Label { textFormat: Text.PlainText; text: "Caption (optional)"; color: app.ink }
                    TextField { id: locationCaptionInput; objectName: "locationCaptionInput"; selectByMouse: true; Layout.fillWidth: true; Accessible.name: "Optional location caption" }
                    Label { textFormat: Text.PlainText; text: "Place name (optional)"; color: app.ink }
                    TextField { id: locationNameInput; objectName: "locationNameInput"; selectByMouse: true; Layout.fillWidth: true; Accessible.name: "Optional location name" }
                    Label { textFormat: Text.PlainText; text: "Address (optional)"; color: app.ink }
                    TextField { id: locationAddressInput; objectName: "locationAddressInput"; selectByMouse: true; Layout.fillWidth: true; Accessible.name: "Optional location address" }
                }
                ColumnLayout {
                    visible: locationEntryDialog.reviewing
                    Layout.fillWidth: true
                    spacing: 6
                    Label {
                        objectName: "locationReviewCoordinates"
                        textFormat: Text.PlainText
                        text: "Coordinates · " + Number(locationEntryDialog.reviewedLocation.latitude).toFixed(6) + "°, " + Number(locationEntryDialog.reviewedLocation.longitude).toFixed(6) + "°"
                        color: app.ink
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label { visible: String(locationEntryDialog.reviewedLocation.caption || "") !== ""; textFormat: Text.PlainText; text: "Caption"; color: app.muted; font.pixelSize: 11 }
                    Label { objectName: "locationReviewCaption"; visible: String(locationEntryDialog.reviewedLocation.caption || "") !== ""; textFormat: Text.PlainText; text: String(locationEntryDialog.reviewedLocation.caption || ""); color: app.ink; wrapMode: Text.WordWrap; Layout.fillWidth: true }
                    Label { visible: String(locationEntryDialog.reviewedLocation.name || "") !== ""; textFormat: Text.PlainText; text: "Name"; color: app.muted; font.pixelSize: 11 }
                    Label { objectName: "locationReviewName"; visible: String(locationEntryDialog.reviewedLocation.name || "") !== ""; textFormat: Text.PlainText; text: String(locationEntryDialog.reviewedLocation.name || ""); color: app.ink; wrapMode: Text.WordWrap; Layout.fillWidth: true }
                    Label { visible: String(locationEntryDialog.reviewedLocation.address || "") !== ""; textFormat: Text.PlainText; text: "Address"; color: app.muted; font.pixelSize: 11 }
                    Label { objectName: "locationReviewAddress"; visible: String(locationEntryDialog.reviewedLocation.address || "") !== ""; textFormat: Text.PlainText; text: String(locationEntryDialog.reviewedLocation.address || ""); color: app.ink; wrapMode: Text.WordWrap; Layout.fillWidth: true }
                }
                Label {
                    objectName: "locationShareError"
                    textFormat: Text.PlainText
                    text: locationEntryDialog.localError !== "" ? locationEntryDialog.localError : bridge.locationShareError
                    visible: locationEntryDialog.localError !== "" || bridge.locationShareError !== ""
                    color: app.danger
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
            }
        }

        footer: Item {
            implicitHeight: 48
            RowLayout {
                anchors.fill: parent
                anchors.topMargin: 8
                spacing: 8
                ActionButton {
                    objectName: "locationShareCancel"
                    theme: app
                    text: "Cancel"
                    enabled: !bridge.locationSending
                    onClicked: locationEntryDialog.close()
                }
                ActionButton {
                    objectName: "locationReviewButton"
                    theme: app
                    text: "Review location"
                    visible: !locationEntryDialog.reviewing
                    enabled: bridge.connected && bridge.hasCapability("message.location.send") && !bridge.locationOutcomeUnknown
                    onClicked: locationEntryDialog.reviewEntry()
                }
                ActionButton {
                    objectName: "locationEditButton"
                    theme: app
                    text: "Edit"
                    visible: locationEntryDialog.reviewing
                    enabled: !bridge.locationSending
                    onClicked: locationEntryDialog.reviewing = false
                }
                ActionButton {
                    objectName: "locationSendButton"
                    theme: app
                    primary: true
                    text: bridge.locationSending ? "Sending…" : "Send location"
                    visible: locationEntryDialog.reviewing
                    enabled: bridge.connected && bridge.hasCapability("message.location.send") && !bridge.locationSending && !bridge.locationOutcomeUnknown
                    Accessible.name: "Send the reviewed static location"
                    onClicked: bridge.sendLocation(locationEntryDialog.reviewedLocation)
                }
            }
        }
    }

    Connections {
        target: bridge
        function onContactShareChanged() {
            if (bridge.contactPreparing || bridge.contactCards.length > 0 || bridge.contactShareError !== "")
                contactShareDialog.open();
            else if (contactShareDialog.opened)
                contactShareDialog.close();
        }
    }

    Dialog {
        id: contactShareDialog
        objectName: "contactShareDialog"
        anchors.centerIn: parent
        width: Math.min(480, app.width - 32)
        height: Math.min(620, app.height - 32)
        modal: true
        title: "Review contact card"
        padding: 18
        standardButtons: Dialog.NoButton
        closePolicy: bridge.contactSending ? Popup.NoAutoClose : Popup.CloseOnEscape
        background: Rectangle { color: app.surface; radius: 14; border.color: app.line }
        onClosed: if (!bridge.contactSending) bridge.clearContactShare()

        contentItem: ColumnLayout {
            spacing: 10

            Label {
                textFormat: Text.PlainText
                text: bridge.contactPreparing ? "Reading the selected local vCard…" : bridge.contactShareError
                visible: bridge.contactPreparing || bridge.contactShareError !== ""
                color: bridge.contactOutcomeUnknown ? app.danger : bridge.contactShareError !== "" ? app.danger : app.muted
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }

            ScrollView {
                Layout.fillWidth: true
                Layout.fillHeight: true
                clip: true
                contentWidth: availableWidth

                ColumnLayout {
                    width: parent.width
                    spacing: 8

                    Repeater {
                        model: bridge.contactCards
                        delegate: Rectangle {
                            required property var modelData
                            objectName: "contactReviewCardSurface"
                            Layout.fillWidth: true
                            implicitHeight: reviewContent.implicitHeight + 20
                            radius: 9
                            color: app.chrome
                            border.color: app.line

                            ColumnLayout {
                                id: reviewContent
                                anchors.fill: parent
                                anchors.margins: 10
                                spacing: 4

                                Label {
                                    objectName: "contactReviewName"
                                    textFormat: Text.PlainText
                                    text: String(modelData.name || "Contact")
                                    color: app.ink
                                    font.weight: Font.DemiBold
                                    wrapMode: Text.WordWrap
                                    Layout.fillWidth: true
                                }
                                Label {
                                    objectName: "contactReviewPhone"
                                    visible: app.contactValues(modelData.phones) !== ""
                                    textFormat: Text.PlainText
                                    text: "Phone · " + app.contactValues(modelData.phones)
                                    color: app.ink
                                    wrapMode: Text.WrapAnywhere
                                    Layout.fillWidth: true
                                }
                                Label {
                                    objectName: "contactReviewEmail"
                                    visible: app.contactValues(modelData.emails) !== ""
                                    textFormat: Text.PlainText
                                    text: "Email · " + app.contactValues(modelData.emails)
                                    color: app.ink
                                    wrapMode: Text.WrapAnywhere
                                    Layout.fillWidth: true
                                }
                                Label {
                                    objectName: "contactReviewOrganization"
                                    visible: String(modelData.organization || "") !== ""
                                    textFormat: Text.PlainText
                                    text: "Organization · " + String(modelData.organization || "")
                                    color: app.muted
                                    wrapMode: Text.WordWrap
                                    Layout.fillWidth: true
                                }
                            }
                        }
                    }
                }
            }
        }

        footer: Item {
            implicitHeight: 52
            RowLayout {
                anchors.fill: parent
                anchors.topMargin: 8
                spacing: 8
                ActionButton {
                    objectName: "contactShareCancel"
                    theme: app
                    text: "Cancel"
                    enabled: !bridge.contactSending
                    onClicked: contactShareDialog.close()
                }
                Item { Layout.fillWidth: true }
                ActionButton {
                    objectName: "contactShareSend"
                    theme: app
                    primary: true
                    text: bridge.contactSending ? "Sending…" : "Send contact"
                    enabled: bridge.contactCards.length > 0 && !bridge.contactPreparing && !bridge.contactSending && !bridge.contactOutcomeUnknown && bridge.connected && bridge.hasCapability("message.contact.send")
                    onClicked: bridge.sendContactCards()
                }
            }
        }
    }

    Dialog {
        id: messageInfo

        objectName: "messageInfoDialog"
        anchors.centerIn: parent
        width: Math.min(430, app.width - 48)
        height: Math.min(320, app.height - 32)
        modal: true
        title: "Message info"
        standardButtons: Dialog.Close

        background: Rectangle {
            color: app.surface
            radius: 12
            border.color: app.line
        }

        contentItem: ColumnLayout {
            spacing: 12

            Label {
                objectName: "messageInfoConversation"
                textFormat: Text.PlainText
                text: "Conversation: " + bridge.selectedTitle
                color: app.ink
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }

            Label {
                objectName: "messageInfoStatus"
                textFormat: Text.PlainText
                text: "Status: " + bridge.messageStatusDescription(String(app.menuMessage.status || ""))
                Accessible.name: "Message status: " + bridge.messageStatusDescription(String(app.menuMessage.status || ""))
                color: app.muted
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }

            Label {
                objectName: "messageInfoTime"
                textFormat: Text.PlainText
                text: "Time: " + new Date(app.menuMessage.timestamp || 0).toLocaleString()
                color: app.muted
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }

            TextEdit {
                objectName: "messageInfoId"
                text: "ID: " + (app.menuMessage.id || "")
                readOnly: true
                selectByMouse: true
                color: app.muted
                wrapMode: TextEdit.Wrap
                Layout.fillWidth: true
                Layout.preferredHeight: 48
                font.pixelSize: 11
            }

        }

    }

    Dialog {
        id: replyDialog
        objectName: "protocolReplyDialog"
        anchors.centerIn: parent
        width: Math.min(500, app.width - 32)
        title: "Reply"
        modal: true
        standardButtons: Dialog.NoButton
        contentItem: ColumnLayout {
            Label { textFormat: Text.PlainText; text: app.menuMessage.sender_name || "Message"; color: app.muted; font.weight: Font.DemiBold }
            Label { textFormat: Text.PlainText; text: app.menuMessage.body || app.menuMessage.attachment?.name || "Media message"; color: app.ink; wrapMode: Text.WordWrap; maximumLineCount: 3; elide: Text.ElideRight; Layout.fillWidth: true }
            TextArea { id: replyInput; objectName: "protocolReplyText"; text: app.replyText; onTextChanged: app.replyText = text; placeholderText: "Write a reply"; textFormat: TextEdit.PlainText; color: app.ink; wrapMode: TextEdit.Wrap; Layout.fillWidth: true; Layout.preferredHeight: 96; Accessible.name: "Reply text" }
        }
        footer: Item {
            implicitHeight: 68
            RowLayout { anchors.right: parent.right; anchors.rightMargin: 16; anchors.verticalCenter: parent.verticalCenter; spacing: 8
                PanelAction { text: "Cancel"; onClicked: replyDialog.close() }
                PanelAction { objectName: "sendProtocolReply"; text: "Send reply"; primary: true; enabled: replyInput.text.trim().length > 0 && app.capabilityAvailable("message.reply"); onClicked: { bridge.performMessageAction("message.reply", app.menuMessage, {"text": replyInput.text}); replyDialog.close() } }
            }
        }
    }

    Dialog {
        id: editDialog
        objectName: "editMessageDialog"
        anchors.centerIn: parent
        width: Math.min(500, app.width - 32)
        title: "Edit message"
        modal: true
        standardButtons: Dialog.NoButton
        contentItem: TextArea { id: editInput; objectName: "editMessageText"; text: app.editText; onTextChanged: app.editText = text; textFormat: TextEdit.PlainText; color: app.ink; wrapMode: TextEdit.Wrap; Layout.preferredHeight: 140; selectByMouse: true; Accessible.name: "Edited message text" }
        footer: Item {
            implicitHeight: 68
            RowLayout { anchors.right: parent.right; anchors.rightMargin: 16; anchors.verticalCenter: parent.verticalCenter; spacing: 8
                PanelAction { text: "Cancel"; onClicked: editDialog.close() }
                PanelAction { objectName: "saveMessageEdit"; text: "Save edit"; primary: true; enabled: app.capabilityAvailable("message.edit"); onClicked: { bridge.performMessageAction("message.edit", app.menuMessage, {"text": editInput.text}); editDialog.close() } }
            }
        }
    }

    Dialog {
        id: deleteMessageDialog
        objectName: "deleteMessageConfirm"
        anchors.centerIn: parent
        width: Math.min(440, app.width - 32)
        title: app.deleteScope === "everyone" ? "Delete for everyone?" : "Delete for me?"
        modal: true
        standardButtons: Dialog.NoButton
        contentItem: Label {
            textFormat: Text.PlainText
            text: app.deleteScope === "everyone" ? "This sends a delete-for-everyone request for the selected message. The linked service may reject it; the timeline changes only after backend confirmation." : "This sends a local delete request. The message is not removed until the backend confirms the updated projection."
            color: app.ink; wrapMode: Text.WordWrap
        }
        footer: Item {
            implicitHeight: 68
            RowLayout { anchors.right: parent.right; anchors.rightMargin: 16; anchors.verticalCenter: parent.verticalCenter; spacing: 8
                PanelAction { text: "Cancel"; onClicked: deleteMessageDialog.close() }
                PanelAction { objectName: "confirmMessageDelete"; text: "Delete"; primary: true; onClicked: { bridge.performMessageAction("message.delete", app.menuMessage, {"scope": app.deleteScope}); deleteMessageDialog.close() } }
            }
        }
    }

    Dialog {
        id: forwardDialog
        objectName: "forwardMessageDialog"
        anchors.centerIn: parent
        width: Math.min(460, app.width - 32)
        height: Math.min(560, app.height - 32)
        title: "Forward message"
        modal: true
        padding: 0
        standardButtons: Dialog.NoButton
        background: Rectangle { color: app.surface; radius: 14; border.color: app.line }
        contentItem: ListView {
            objectName: "forwardChatList"
            clip: true
            model: bridge.chats
            delegate: ItemDelegate {
                required property var record
                objectName: "forwardChatRow"
                width: ListView.view.width
                enabled: record.id !== bridge.selectedId
                onClicked: app.forwardTargetChatId = record.id
                background: Rectangle { color: app.forwardTargetChatId === record.id ? app.active : parent.hovered ? app.chrome : app.surface }
                contentItem: Label { textFormat: Text.PlainText; text: record.title; color: app.ink; elide: Text.ElideRight; verticalAlignment: Text.AlignVCenter }
            }
        }
        footer: Item {
            implicitHeight: 72
            RowLayout { anchors.left: parent.left; anchors.leftMargin: 16; anchors.right: parent.right; anchors.rightMargin: 16; anchors.verticalCenter: parent.verticalCenter
                Label { textFormat: Text.PlainText; text: app.forwardTargetChatId ? "Destination selected" : "Choose a verified conversation"; color: app.muted; Layout.fillWidth: true }
                PanelAction { text: "Cancel"; onClicked: forwardDialog.close() }
                PanelAction { objectName: "confirmForwardMessage"; text: "Forward"; primary: true; enabled: !!app.forwardTargetChatId && app.capabilityAvailable("message.forward"); onClicked: { bridge.performMessageAction("message.forward", app.menuMessage, {"target_chat_id": app.forwardTargetChatId}); forwardDialog.close() } }
            }
        }
    }

    Dialog {
        id: createSyntheticGroupDialog
        objectName: "createSyntheticGroupDialog"
        anchors.centerIn: parent
        width: Math.min(480, app.width - 32)
        height: Math.min(620, app.height - 32)
        title: "Create synthetic demo group"
        modal: true
        padding: 18
        standardButtons: Dialog.NoButton
        function selectedSyntheticMembers() {
            const members = [];
            if (syntheticGroupMemberAlex.checked) members.push("demo:participant:alex");
            if (syntheticGroupMemberMaya.checked) members.push("demo:participant:maya");
            if (syntheticGroupMemberSam.checked) members.push("demo:participant:sam");
            return members;
        }
        function resetForm() {
            syntheticGroupTitleInput.text = "";
            syntheticGroupDescriptionInput.text = "";
            syntheticGroupMemberAlex.checked = false;
            syntheticGroupMemberMaya.checked = false;
            syntheticGroupMemberSam.checked = false;
        }
        onOpened: syntheticGroupTitleInput.forceActiveFocus()
        background: Rectangle { color: app.surface; radius: 12; border.color: app.line }
        contentItem: ScrollView {
            clip: true
            contentWidth: availableWidth
            ScrollBar.horizontal.policy: ScrollBar.AlwaysOff
            ColumnLayout {
                width: parent.width
                spacing: 12
                Label {
                    text: "Local-only fixture. This creates a synthetic conversation in this demo profile; it does not create or change a WhatsApp group."
                    textFormat: Text.PlainText
                    color: app.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                Label { text: "Group name"; textFormat: Text.PlainText; color: app.ink }
                TextField {
                    id: syntheticGroupTitleInput
                    objectName: "syntheticGroupTitleInput"
                    Layout.fillWidth: true
                    maximumLength: 128
                    placeholderText: "e.g. Weekend plans"
                    color: app.ink
                    selectByMouse: true
                    background: Rectangle { color: app.chrome; radius: 8; border.color: parent.activeFocus ? app.accent : app.line }
                    Accessible.name: "Synthetic group name"
                }
                Label { text: "Description (optional)"; textFormat: Text.PlainText; color: app.ink }
                TextArea {
                    id: syntheticGroupDescriptionInput
                    objectName: "syntheticGroupDescriptionInput"
                    Layout.fillWidth: true
                    Layout.preferredHeight: 76
                    placeholderText: "Local demo description"
                    color: app.ink
                    selectByMouse: true
                    wrapMode: TextEdit.Wrap
                    textFormat: TextEdit.PlainText
                    background: Rectangle { color: app.chrome; radius: 8; border.color: parent.activeFocus ? app.accent : app.line }
                    Accessible.name: "Synthetic group description"
                }
                Label { text: "Add synthetic participants"; textFormat: Text.PlainText; color: app.ink; font.weight: Font.DemiBold }
                CheckBox { id: syntheticGroupMemberAlex; objectName: "syntheticGroupMemberAlex"; text: "Alex Morgan" }
                CheckBox { id: syntheticGroupMemberMaya; objectName: "syntheticGroupMemberMaya"; text: "Maya Chen" }
                CheckBox { id: syntheticGroupMemberSam; objectName: "syntheticGroupMemberSam"; text: "Sam Rivera" }
                Label {
                    objectName: "syntheticGroupCreateError"
                    visible: app.groupActionError.length > 0
                    text: app.groupActionError
                    textFormat: Text.PlainText
                    color: app.danger
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
            }
        }
        footer: Item {
            implicitHeight: 64
            RowLayout {
                anchors.right: parent.right
                anchors.rightMargin: 12
                anchors.verticalCenter: parent.verticalCenter
                spacing: 8
                PanelAction { objectName: "cancelSyntheticGroupCreation"; text: "Cancel"; onClicked: createSyntheticGroupDialog.close() }
                PanelAction {
                    objectName: "confirmSyntheticGroupCreation"
                    text: "Create locally"
                    primary: true
                    enabled: bridge.demo && app.capabilityAvailable("group.create") && !bridge.routeLoading && syntheticGroupTitleInput.text.trim().length > 0 && createSyntheticGroupDialog.selectedSyntheticMembers().length > 0
                    onClicked: {
                        app.groupActionError = "";
                        if (!bridge.performCapabilityAction("group.create", {"title": syntheticGroupTitleInput.text, "description": syntheticGroupDescriptionInput.text, "member_ids": createSyntheticGroupDialog.selectedSyntheticMembers()}))
                            app.groupActionError = "Synthetic group creation is unavailable in this backend.";
                    }
                }
            }
        }
    }

    Dialog {
        id: conversationInfo
        objectName: "conversationInfoDialog"
        anchors.centerIn: parent
        width: Math.min(460, app.width - 48)
        height: Math.min(540, app.height - 32)
        title: "Conversation info"
        modal: true
        padding: 18
        standardButtons: Dialog.Close
        onOpened: bridge.loadHub("chat.info", {"chat_id": bridge.selectedId})
        contentItem: ScrollView {
            id: conversationInfoScroll
            objectName: "conversationInfoScroll"
            clip: true
            contentWidth: availableWidth
            contentHeight: conversationInfoContent.implicitHeight
            ScrollBar.horizontal.policy: ScrollBar.AlwaysOff
            Item {
                id: conversationInfoContent
                width: conversationInfoScroll.availableWidth
                implicitHeight: conversationInfoLayout.implicitHeight
                height: implicitHeight
                Column {
                    id: conversationInfoLayout
                    anchors.fill: parent
                    spacing: 12
                    Label {
                        visible: bridge.routeLoading || bridge.routeError.length > 0
                        width: parent.width
                        height: implicitHeight
                        textFormat: Text.PlainText
                        text: bridge.routeLoading ? "Loading cached conversation details…" : bridge.routeError
                        color: bridge.routeError ? app.danger : app.muted
                        wrapMode: Text.WordWrap
                    }
                    CachedConversationInfo {
                        objectName: "cachedConversationInfo"
                        width: parent.width
                        height: implicitHeight
                        theme: app
                        controller: bridge
                        actionError: app.groupActionError
                        record: bridge.routeMethod === "chat.info" && bridge.routeData.id === bridge.selectedId ? bridge.routeData : ({})
                        visible: !!record.id && !bridge.routeLoading
                    }
                }
            }
        }
    }

    Dialog {
        id: about
        objectName: "aboutDialog"

        anchors.centerIn: parent
        width: Math.min(400, app.width - 48)
        title: "WhatsFeather"
        modal: true
        standardButtons: Dialog.Close

        contentItem: Label {
            textFormat: Text.PlainText
            text: "A native Linux messaging preview. Unofficial and not affiliated with WhatsApp or Meta."
            color: app.ink
            wrapMode: Text.WordWrap
        }

    }

    component PanelAction: Button {
        id: action
        property bool primary: false
        implicitHeight: 40
        implicitWidth: Math.max(104, implicitContentWidth + 32)
        padding: 12
        activeFocusOnTab: true
        Accessible.name: text
        contentItem: Text {
            text: action.text
            textFormat: Text.PlainText
            color: action.primary ? (app.dark ? "#062e26" : "white") : app.ink
            font: action.font
            horizontalAlignment: Text.AlignHCenter
            verticalAlignment: Text.AlignVCenter
            elide: Text.ElideRight
        }
        background: Rectangle {
            radius: 10
            color: action.primary ? app.accent : (action.hovered ? app.line : app.active)
            border.width: action.activeFocus ? 2 : 0
            border.color: app.accent
            opacity: action.enabled ? 1 : 0.45
        }
    }

    component AccountAvatar: Rectangle {
        property int diameter: 64
        width: diameter
        height: diameter
        radius: diameter / 2
        color: "#526a63"
        clip: true
        Label {
            anchors.centerIn: parent
            textFormat: Text.PlainText
            text: app.accountInitials()
            color: "white"
            font.pixelSize: Math.max(14, parent.diameter / 3)
            font.weight: Font.DemiBold
            visible: avatarImage.status !== Image.Ready
        }
        Image {
            id: avatarImage
            anchors.fill: parent
            source: app.accountData.avatar_url || ""
            visible: status === Image.Ready
            fillMode: Image.PreserveAspectCrop
            sourceSize.width: 192
            sourceSize.height: 192
            asynchronous: true
        }
    }

    Dialog {
        id: profile
        objectName: "profileDialog"
        property bool editingDemoProfile: false
        property string demoNameDraft: ""
        property string demoAboutDraft: ""
        anchors.centerIn: parent
        width: Math.min(520, app.width - 32)
        height: Math.min(560, app.height - 32)
        modal: true
        padding: 0
        standardButtons: Dialog.NoButton
        onOpened: profileClose.forceActiveFocus()
        onClosed: editingDemoProfile = false
        background: Rectangle { color: app.surface; radius: 16; border.color: app.line }
        function beginDemoProfileEdit() {
            if (!bridge.demo) return;
            editingDemoProfile = true;
            demoNameDraft = app.accountName();
            demoAboutDraft = app.accountAbout();
            demoProfileNameInput.text = demoNameDraft;
            demoProfileAboutInput.text = demoAboutDraft;
            demoProfileNameInput.forceActiveFocus();
        }
        function cancelDemoProfileEdit() {
            editingDemoProfile = false;
            demoNameDraft = app.accountName();
            demoAboutDraft = app.accountAbout();
        }
        function saveDemoProfileEdit() {
            if (!bridge.demo) return;
            const name = demoProfileNameInput.text.trim();
            const about = demoProfileAboutInput.text.trim();
            if (name.length === 0 || name.length > 80 || about.length > 280) return;
            app.demoProfileOverride = ({"display_name": name, "about": about});
            demoNameDraft = name;
            demoAboutDraft = about;
            editingDemoProfile = false;
        }
        header: Item {
            implicitHeight: 68
            Label {
                anchors.left: parent.left; anchors.leftMargin: 24
                anchors.verticalCenter: parent.verticalCenter
                text: profile.editingDemoProfile ? "Edit demo profile" : "Profile and account"
                color: app.ink; font.pixelSize: 21; font.weight: Font.DemiBold
            }
        }
        contentItem: Flickable {
            objectName: "profileContentViewport"
            clip: true
            contentWidth: width
            contentHeight: profileColumn.implicitHeight + 32
            ScrollBar.vertical: ScrollBar { }
            ColumnLayout {
                id: profileColumn
                x: 24; y: 8; width: parent.width - 48; spacing: 10
                AccountAvatar { visible: !profile.editingDemoProfile; diameter: 72; Layout.alignment: Qt.AlignHCenter }
                Label { objectName: "profileDisplayName"; visible: !profile.editingDemoProfile; textFormat: Text.PlainText; text: app.accountName(); color: app.ink; font.pixelSize: 21; font.weight: Font.DemiBold; Layout.fillWidth: true; horizontalAlignment: Text.AlignHCenter; wrapMode: Text.WordWrap }
                Label { objectName: "profileAccountState"; visible: !profile.editingDemoProfile; textFormat: Text.PlainText; text: bridge.demo ? "Synthetic local profile" : (bridge.status || "Account status"); color: app.muted; font.pixelSize: 12; Layout.fillWidth: true; wrapMode: Text.WordWrap; horizontalAlignment: Text.AlignHCenter }
                Label {
                    objectName: "profileEditStatus"
                    visible: bridge.demo && Object.prototype.hasOwnProperty.call(app.demoProfileOverride, "display_name")
                    textFormat: Text.PlainText
                    text: "Edited in this demo session only; not sent to WhatsApp."
                    color: app.muted; font.pixelSize: 12; Layout.fillWidth: true
                    wrapMode: Text.WordWrap; horizontalAlignment: Text.AlignHCenter
                }
                Rectangle { visible: !profile.editingDemoProfile; Layout.fillWidth: true; implicitHeight: 1; color: app.line }
                Label { visible: !profile.editingDemoProfile; text: "Phone"; color: app.muted; font.pixelSize: 12 }
                Label { objectName: "profilePhone"; visible: !profile.editingDemoProfile; textFormat: Text.PlainText; text: app.accountData.phone || "Not available"; color: app.ink; Layout.fillWidth: true; wrapMode: Text.WrapAnywhere }
                Label { visible: !profile.editingDemoProfile; text: "Username"; color: app.muted; font.pixelSize: 12 }
                Label { objectName: "profileUsername"; visible: !profile.editingDemoProfile; textFormat: Text.PlainText; text: app.accountData.username ? "@" + app.accountData.username : "WhatsApp has not supplied a username for this account."; color: app.ink; Layout.fillWidth: true; wrapMode: Text.WordWrap }
                Label { visible: !profile.editingDemoProfile && !!app.accountAbout(); text: "About"; color: app.muted; font.pixelSize: 12 }
                Label { objectName: "profileAbout"; visible: !profile.editingDemoProfile && !!app.accountAbout(); textFormat: Text.PlainText; text: app.accountAbout(); color: app.ink; Layout.fillWidth: true; wrapMode: Text.WordWrap }
                PanelAction {
                    objectName: "editDemoProfileButton"
                    visible: bridge.demo && !profile.editingDemoProfile
                    text: "Edit local demo profile"
                    Layout.fillWidth: true
                    onClicked: profile.beginDemoProfileEdit()
                }
                ColumnLayout {
                    id: demoProfileEditor
                    objectName: "demoProfileEditor"
                    visible: bridge.demo && profile.editingDemoProfile
                    Layout.fillWidth: true
                    spacing: 8
                    Label { textFormat: Text.PlainText; text: "Demo display name"; color: app.ink; font.pixelSize: 13 }
                    TextField {
                        id: demoProfileNameInput
                        objectName: "demoProfileNameInput"
                        Layout.fillWidth: true
                        maximumLength: 80
                        selectByMouse: true
                        color: app.ink
                        text: ""
                        Accessible.name: "Demo profile display name"
                        onTextChanged: if (profile.editingDemoProfile) profile.demoNameDraft = text
                        background: Rectangle { implicitHeight: 40; color: app.chrome; radius: 8; border.color: demoProfileNameInput.activeFocus ? app.accent : app.line }
                    }
                    Label { textFormat: Text.PlainText; text: "Demo About"; color: app.ink; font.pixelSize: 13 }
                    TextField {
                        id: demoProfileAboutInput
                        objectName: "demoProfileAboutInput"
                        Layout.fillWidth: true
                        maximumLength: 280
                        selectByMouse: true
                        color: app.ink
                        text: ""
                        Accessible.name: "Demo profile About"
                        onTextChanged: if (profile.editingDemoProfile) profile.demoAboutDraft = text
                        background: Rectangle { implicitHeight: 40; color: app.chrome; radius: 8; border.color: demoProfileAboutInput.activeFocus ? app.accent : app.line }
                    }
                    Label {
                        objectName: "profileEditLocalNotice"
                        textFormat: Text.PlainText
                        text: "Demo only. Save updates this local profile for the current app session; it is not sent to WhatsApp."
                        color: app.muted; font.pixelSize: 12; Layout.fillWidth: true
                        wrapMode: Text.WordWrap
                    }
                }
            }
        }
        footer: Item {
            implicitHeight: 72
            PanelAction {
                id: profileClose
                objectName: "profileClose"
                visible: !profile.editingDemoProfile
                anchors.right: parent.right; anchors.rightMargin: 24
                anchors.top: parent.top; anchors.topMargin: 12
                anchors.bottom: parent.bottom; anchors.bottomMargin: 16
                width: 112; text: "Close"
                onClicked: profile.close()
            }
            RowLayout {
                visible: profile.editingDemoProfile
                anchors.right: parent.right; anchors.rightMargin: 24
                anchors.top: parent.top; anchors.topMargin: 12
                anchors.bottom: parent.bottom; anchors.bottomMargin: 16
                spacing: 8
                PanelAction {
                    objectName: "profileEditCancel"
                    text: "Cancel"
                    width: 96
                    onClicked: profile.cancelDemoProfileEdit()
                }
                PanelAction {
                    objectName: "profileEditSave"
                    text: "Save locally"
                    width: 132
                    primary: true
                    enabled: bridge.demo && profile.editingDemoProfile && demoProfileNameInput.text.trim().length > 0
                    onClicked: profile.saveDemoProfileEdit()
                }
            }
        }
    }

    Dialog {
        id: settings
        objectName: "settingsDialog"
        anchors.centerIn: parent
        width: Math.min(700, app.width - 32)
        height: Math.min(560, app.height - 32)
        modal: true
        padding: 0
        standardButtons: Dialog.NoButton
        onOpened: settingsClose.forceActiveFocus()
        background: Rectangle { color: app.surface; radius: 16; border.color: app.line }
        header: Item {
            implicitHeight: 72
            RowLayout {
                anchors.fill: parent; anchors.leftMargin: 24; anchors.rightMargin: 24
                Label { objectName: "settingsContentHeading"; text: "Settings"; color: app.ink; font.pixelSize: 22; font.weight: Font.DemiBold; Layout.fillWidth: true }
                Label { textFormat: Text.PlainText; text: bridge.demo ? "Demo profile" : bridge.connectionState; color: app.muted; font.pixelSize: 12 }
            }
        }
        contentItem: Flickable {
            objectName: "settingsScroll"
            clip: true
            contentWidth: width
            contentHeight: settingsColumn.implicitHeight + 24
            ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }
            ColumnLayout {
                id: settingsColumn
                x: 24; y: 0; width: parent.width - 48; spacing: 16
                Rectangle {
                    Layout.fillWidth: true; implicitHeight: 108; radius: 12; color: app.chrome
                    RowLayout {
                        anchors.fill: parent; anchors.margins: 16; spacing: 16
                        AccountAvatar { diameter: 60 }
                        ColumnLayout {
                            Layout.fillWidth: true; spacing: 4
                            Label { textFormat: Text.PlainText; text: app.accountName(); color: app.ink; font.pixelSize: 16; font.weight: Font.DemiBold; Layout.fillWidth: true; elide: Text.ElideRight }
                            Label { textFormat: Text.PlainText; text: app.accountData.phone || "Phone not available"; color: app.muted; Layout.fillWidth: true; elide: Text.ElideRight }
                            Label { textFormat: Text.PlainText; text: app.accountData.username ? "@" + app.accountData.username : "No username supplied by WhatsApp"; color: app.muted; font.pixelSize: 12; Layout.fillWidth: true; elide: Text.ElideRight }
                        }
                        PanelAction { text: "View profile"; onClicked: { settings.close(); profile.open(); } }
                    }
                }
                Label { text: "Appearance"; color: app.ink; font.weight: Font.DemiBold }
                RowLayout {
                    spacing: 8
                    PanelAction { objectName: "settingsThemeLight"; text: "Light"; primary: !app.dark; onClicked: app.dark = false }
                    PanelAction { objectName: "settingsThemeDark"; text: "Dark"; primary: app.dark; onClicked: app.dark = true }
                }
                NotificationPreferences {
                    objectName: "notificationPreferences"
                    Layout.fillWidth: true
                    ink: app.ink; muted: app.muted; danger: app.danger
                    notificationsEnabled: desktopNotifications.notificationsEnabled
                    messagePreviewsEnabled: desktopNotifications.messagePreviewsEnabled
                    trayAvailable: desktopNotifications.trayAvailable
                    notificationsAvailable: desktopNotifications.notificationsAvailable
                    onNotificationsEnabledRequested: function(enabled) { desktopNotifications.notificationsEnabled = enabled; }
                    onMessagePreviewsEnabledRequested: function(enabled) { desktopNotifications.messagePreviewsEnabled = enabled; }
                }
                Label { objectName: "settingsConnectionHeading"; text: "Connection"; color: app.ink; font.weight: Font.DemiBold }
                Label { objectName: "settingsConnectionStatus"; textFormat: Text.PlainText; text: bridge.demo ? "Synthetic conversations and attachments stay on this computer." : bridge.status; color: app.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true }
                PanelAction { objectName: "connectAccountAction"; visible: !bridge.demo && bridge.connectionState !== "connected" && bridge.connectionState !== "connecting" && bridge.connectionState !== "qr"; text: "Connect account"; primary: true; onClicked: risk.open() }
                PanelAction { objectName: "localDisconnectAction"; visible: !bridge.demo && bridge.connectionState === "connected"; text: "Disconnect here — keep device linked"; onClicked: bridge.disconnectLive() }
                Label { objectName: "connectionDisconnectScope"; textFormat: Text.PlainText; text: "In live mode, Disconnect here closes only this app's connection; the linked-device session and local history remain on this computer. It does not unlink or log out this device from WhatsApp."; color: app.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; font.pixelSize: 12 }
                Label { objectName: "remoteUnlinkAvailability"; textFormat: Text.PlainText; text: "Remote unlink/logout is not available here. Use WhatsApp's Linked devices settings to remove this device; this client does not send a remote logout request."; color: app.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; font.pixelSize: 12 }
                Image { visible: bridge.qr !== ""; source: bridge.qr; cache: false; Layout.preferredWidth: 220; Layout.preferredHeight: 220; Layout.alignment: Qt.AlignHCenter }
                Label { text: "About and limitations"; color: app.ink; font.weight: Font.DemiBold }
                Label { text: "Media is downloaded only when you choose Download. Calls and recording are not available in this build. Local data is not encrypted by this app."; color: app.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; font.pixelSize: 12 }
            }
        }
        footer: Item {
            objectName: "settingsFooter"
            implicitHeight: 72
            PanelAction {
                id: settingsClose
                objectName: "settingsClose"
                anchors.right: parent.right; anchors.rightMargin: 24
                anchors.top: parent.top; anchors.topMargin: 12
                anchors.bottom: parent.bottom; anchors.bottomMargin: 16
                width: 112; text: "Close"
                onClicked: settings.close()
            }
        }
    }
    MediaViewer {
        id: mediaViewer
        theme: app
    }

    AttachmentDialog {
        id: attachmentDialog
        theme: app
        controller: bridge
    }

    Dialog {
        id: risk
        objectName: "connectRiskDialog"

        anchors.centerIn: parent
        width: 420
        title: "Connect experimental client?"
        modal: true
        standardButtons: Dialog.Cancel | Dialog.Ok
        onAccepted: bridge.connectLive()

        contentItem: Label {
            textFormat: Text.PlainText
            text: "This can expose the account to restrictions. Only continue with an authorized spare number. Connection starts only after confirmation."
            color: app.ink
            wrapMode: Text.WordWrap
        }

    }

}
