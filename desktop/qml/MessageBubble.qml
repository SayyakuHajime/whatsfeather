import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

Item {
    id: row

    required property var record
    required property var theme
    required property var controller
    required property int index
    property bool highlighted: false
    property bool selected: false
    readonly property var attachment: record.attachment || ({})
    readonly property bool hasAttachment: Object.keys(attachment).length > 0
    readonly property bool isAlbum: record.kind === "album"
    readonly property var albumItems: validatedAlbumItems(record.album)
    readonly property var linkPreview: record.link_preview || ({})
    readonly property bool isLinkPreview: !record.deleted && record.kind === "text" &&
            typeof record.body === "string" && /https?:\/\/\S/i.test(record.body) &&
            validLinkPreviewProjection(linkPreview)
    readonly property bool isContact: record.kind === "contact" && record.contacts && record.contacts.length > 0
    readonly property var location: record.location || ({})
    readonly property bool isLocation: record.kind === "location" && Object.keys(location).length > 0
    readonly property var poll: record.poll || ({})
    readonly property bool isPoll: record.kind === "poll" && typeof poll.question === "string" && poll.options && typeof poll.options.length === "number" && poll.options.length > 0
    readonly property bool isLegacyPoll: record.kind === "poll" && !isPoll
    readonly property bool hasPollSelectableCount: isPoll && Number.isInteger(poll.selectable_options_count) && poll.selectable_options_count > 0 && poll.selectable_options_count <= Math.min(12, poll.options.length)
    readonly property var pollResults: record.poll_results || ({})
    readonly property bool isPollResults: (record.kind === "poll_results" || isPoll) && validPollResultsProjection(pollResults)
    readonly property bool canVote: isPoll && !record.deleted && !record.protected && !record.view_once && !record.ephemeral && controller.capabilitiesReady && controller.hasCapability("poll.vote")
    readonly property bool isLegacyPollResults: record.kind === "poll_results" && !isPollResults
    readonly property var listProjection: record.list || ({})
    readonly property bool isList: record.kind === "list" && typeof listProjection.title === "string" && typeof listProjection.button_label === "string" && listProjection.sections && typeof listProjection.sections.length === "number" && listProjection.sections.length > 0
    readonly property bool isLegacyList: record.kind === "list" && !isList
    readonly property string listReplyState: String(record.list_reply_state || "")
    readonly property bool listReplyLocked: listReplyState !== "" || controller.listReplyBlockedTargets.indexOf(String(record.id || "")) >= 0
    readonly property bool canSelectListRow: isList && record.list_reply_available === true && !record.outgoing && !record.deleted && !record.protected && !record.view_once && !record.ephemeral && !listReplyLocked && controller.capabilitiesReady && controller.hasCapability("list.reply")
    readonly property string listReplyStatusText: listReplyState === "accepted" ? "Response accepted" : listReplyState === "unknown" ? "Outcome unknown · check before retrying" : (listReplyState === "pending" || listReplyLocked) ? "Response pending · check before retrying" : "Display only"
    readonly property var textCardProjection: record.kind === "interactive" ? (record.interactive || ({})) : (record.template || ({}))
    readonly property bool isTextCard: (record.kind === "interactive" || record.kind === "template") && !!(record.interactive || record.template)
    readonly property var buttonsProjection: record.buttons || ({})
    readonly property var buttonLabels: buttonLabelValues(buttonsProjection.buttons)
    readonly property bool isButtons: record.kind === "buttons" && buttonLabels.length > 0
    readonly property string buttonReplyState: String(record.button_reply_state || "")
    readonly property bool buttonReplyLocked: buttonReplyState !== "" || controller.buttonReplyBlockedTargets.indexOf(String(record.id || "")) >= 0
    readonly property bool canSelectButtonResponse: isButtons && controller.demo && controller.capabilitiesReady && controller.hasCapability("button.reply") && record.button_reply_available === true && !record.outgoing && !record.deleted && !record.protected && !record.view_once && !record.ephemeral && !buttonReplyLocked
    readonly property string buttonReplyStatusText: buttonReplyState === "local" ? "Recorded locally · not sent to WhatsApp" : buttonReplyState === "accepted" ? "Response accepted" : buttonReplyState === "unknown" ? "Outcome unknown · check before retrying" : (buttonReplyState === "pending" || buttonReplyLocked) ? "Response pending · check before retrying" : ""
    readonly property string audioLabel: attachment.is_voice_note ? "Voice note" : "Audio"
    readonly property bool readyMedia: hasAttachment && attachment.state === "ready" && String(attachment.url || "").indexOf("file:") === 0
    readonly property bool showDate: index === 0 || dateKey(controller.messages.row(index - 1).timestamp) !== dateKey(record.timestamp)
    readonly property real maxBubbleWidth: Math.min(560, Math.max(220, row.width * 0.72))
    readonly property real textWidth: Math.max(0, measure.implicitWidth)
    readonly property real naturalBubbleWidth: isContact || isLocation || isPoll || isPollResults || isList || isButtons || isTextCard || isLinkPreview || isAlbum ? 300 : hasAttachment ? 320 : textWidth + 28
    readonly property real bubbleWidth: Math.min(maxBubbleWidth, Math.max(hasAttachment ? 260 : 42, naturalBubbleWidth, metadataRow.implicitWidth + 20))

    signal menuRequested(var message, Item anchor)
    signal mediaRequested(var message)
    signal pollVoteRequested(var message)

    objectName: "messageBubble"
    width: ListView.view ? ListView.view.width : 600
    height: bubble.y + bubble.height + 4

    function contactValues(values) {
        if (!values || typeof values.length !== "number")
            return "";
        let output = [];
        for (let i = 0; i < values.length && i < 5; ++i)
            if (typeof values[i] === "string")
                output.push(values[i]);
        return output.join(", ");
    }

    function pollOptionValues(values) {
        if (!values || typeof values.length !== "number")
            return [];
        let output = [];
        for (let i = 0; i < values.length && i < 12; ++i)
            output.push(values[i]);
        return output;
    }

    function validPollResultsProjection(value) {
        if (!value || typeof value.question !== "string" || value.question.trim() === "" ||
                !value.options || typeof value.options.length !== "number" ||
                value.options.length < 1 || value.options.length > 12)
            return false;
        for (let i = 0; i < value.options.length; ++i) {
            const option = value.options[i];
            if (!option || typeof option.name !== "string" || option.name.trim() === "" ||
                    !Number.isInteger(option.vote_count) || option.vote_count < 0 ||
                    option.vote_count > 9007199254740991)
                return false;
        }
        return true;
    }

    function validLinkPreviewText(value, maximumLength) {
        return typeof value === "string" && value.length <= maximumLength &&
                !/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(value);
    }

    function validLinkPreviewProjection(value) {
        const site = value && typeof value.site === "string" ? value.site : "";
        const ipv6Host = site.indexOf(":") >= 0 && /^[0-9a-f:]+$/i.test(site);
        if (!value || typeof value !== "object" || Array.isArray(value) ||
                typeof value.site !== "string" || site.trim() === "" || site.length > 253 ||
                /[\u0000-\u0020\u007f\/?#@\[\]%]/.test(site) || (site.indexOf(":") >= 0 && !ipv6Host))
            return false;
        const title = typeof value.title === "undefined" ? "" : value.title;
        const description = typeof value.description === "undefined" ? "" : value.description;
        if (!validLinkPreviewText(title, 256) || !validLinkPreviewText(description, 512))
            return false;
        return title.trim() !== "" || description.trim() !== "";
    }

    function pollResultOptionValues(values) {
        if (!values || typeof values.length !== "number")
            return [];
        let output = [];
        for (let i = 0; i < values.length && i < 12; ++i)
            output.push(values[i]);
        return output;
    }

    function pollVoteCountText(value) {
        const count = Number(value);
        return String(count) + (count === 1 ? " vote" : " votes");
    }

    function validatedAlbumItems(values) {
        if (!isAlbum || record.outgoing !== true || record.status !== "demo_sent" ||
                !values || typeof values.length !== "number" || values.length < 2 || values.length > 10)
            return [];
        const items = [];
        let total = 0;
        for (let i = 0; i < values.length; ++i) {
            const item = values[i];
            const mime = String(item && item.mime || "").toLowerCase();
            const url = String(item && item.url || "");
            const size = Number(item && item.size);
            if (!item || item.kind !== "image" || item.mode !== "photo" ||
                    item.state !== "ready" || !mime.startsWith("image/") || mime === "image/gif" ||
                    !url.startsWith("file:///") || !Number.isSafeInteger(size) ||
                    size <= 0 || size > 20 * 1024 * 1024)
                return [];
            total += size;
            if (total > 50 * 1024 * 1024)
                return [];
            items.push(item);
        }
        return items;
    }

    function listSectionValues(values) {
        if (!values || typeof values.length !== "number")
            return [];
        let output = [];
        for (let i = 0; i < values.length && i < 10; ++i)
            output.push(values[i]);
        return output;
    }

    function listRowValues(values) {
        if (!values || typeof values.length !== "number")
            return [];
        let output = [];
        for (let i = 0; i < values.length && i < 10; ++i)
            output.push(values[i]);
        return output;
    }

    function buttonLabelValues(values) {
        if (!values || typeof values.length !== "number" || values.length < 1 || values.length > 3)
            return [];
        let output = [];
        for (let i = 0; i < values.length; ++i) {
            if (typeof values[i] !== "string" || values[i].trim() === "" || values[i].length > 256 ||
                    /[\u0000-\u001f\u007f]/.test(values[i]))
                return [];
            output.push(values[i]);
        }
        return output;
    }

    function dateKey(value) {
        if (!value)
            return "";
        return new Date(value).toLocaleDateString(Qt.locale(), "yyyy-MM-dd");
    }

    function escapeRich(value) {
        return String(value || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/\"/g, "&quot;");
    }

    function richBody(value) {
        // Only http(s) links from the original plain text become anchors. The
        // display text and href are both escaped before entering RichText.
        let source = String(value || "");
        let output = "";
        let cursor = 0;
        let match;
        let links = /https?:\/\/[^\s<>\"']+/ig;
        while ((match = links.exec(source)) !== null) {
            output += escapeRich(source.slice(cursor, match.index)).replace(/\n/g, "<br>");
            let link = match[0];
            let trailing = "";
            while (/[),.!?;:]$/.test(link))
                trailing = link.slice(-1) + trailing, link = link.slice(0, -1);
            output += "<a href=\"" + escapeRich(link) + "\"><font color=\"" + theme.accent + "\"><u>" + escapeRich(link) + "</u></font></a>" + escapeRich(trailing);
            cursor = match.index + match[0].length;
        }
        return output + escapeRich(source.slice(cursor)).replace(/\n/g, "<br>");
    }

    Text {
        id: measure
        text: record.body || ""
        textFormat: Text.PlainText
        font.family: theme.font.family
        font.pixelSize: 14
        wrapMode: Text.NoWrap
        visible: false
    }

    Label {
        objectName: "messageDateLabel"
        visible: row.showDate
        anchors.top: parent.top
        anchors.horizontalCenter: parent.horizontalCenter
        height: 24
        textFormat: Text.PlainText
        text: new Date(record.timestamp).toLocaleDateString(Qt.locale(), "dd MMM yyyy")
        color: theme.muted
        font.pixelSize: 11
        verticalAlignment: Text.AlignVCenter
        leftPadding: 8
        rightPadding: 8
        background: Rectangle { color: row.theme.chrome; radius: 8; border.color: row.theme.line }
    }

    Rectangle {
        id: bubble
        objectName: "bubbleSurface"
        y: row.showDate ? 28 : 0
        width: row.bubbleWidth
        height: content.implicitHeight + 20
        anchors.left: record.outgoing ? undefined : parent.left
        anchors.right: record.outgoing ? parent.right : undefined
        radius: 10
        color: record.outgoing ? theme.outgoing : theme.chrome
        border.width: highlighted || selected ? 2 : 0
        border.color: selected ? theme.accent : theme.accent

        HoverHandler { id: hover }
        TapHandler {
            acceptedButtons: Qt.RightButton
            onTapped: row.menuRequested(row.record, bubble)
        }

        ColumnLayout {
            id: content
            anchors.fill: parent
            anchors.margins: 10
            spacing: 5

            Label {
                visible: !!record.sender_name && !record.outgoing
                textFormat: Text.PlainText
                text: record.sender_name || ""
                color: theme.accent
                font.pixelSize: 12
                font.weight: Font.DemiBold
                elide: Text.ElideRight
                Layout.fillWidth: true
            }

            Label {
                visible: !!record.quoted_body
                textFormat: Text.PlainText
                text: (record.quoted_sender ? record.quoted_sender + " · " : "") + record.quoted_body
                color: theme.muted
                wrapMode: Text.WordWrap
                maximumLineCount: 2
                elide: Text.ElideRight
                Layout.fillWidth: true
                leftPadding: 8
                background: Rectangle { color: theme.active; radius: 3 }
            }

            ColumnLayout {
                visible: row.hasAttachment
                Layout.fillWidth: true
                spacing: 6

                Image {
                    objectName: "messageMediaPreview"
                    id: mediaPreview
                    visible: row.readyMedia && (row.attachment.kind === "image" || row.attachment.kind === "sticker" || row.attachment.kind === "gif")
                    source: visible ? String(row.attachment.url || "") : ""
                    Layout.fillWidth: true
                    Layout.preferredHeight: row.attachment.kind === "sticker" ? 130 : 170
                    sourceSize.width: 560
                    sourceSize.height: 340
                    fillMode: Image.PreserveAspectFit
                    asynchronous: true
                    mipmap: true
                    Accessible.name: String(row.attachment.name || row.attachment.kind || "Media")
                    Button {
                        objectName: "openMediaButton"
                        anchors.fill: parent
                        Accessible.name: "Open " + String(row.attachment.name || row.attachment.kind || "media")
                        background: Rectangle { color: "transparent"; border.width: parent.activeFocus ? 2 : 0; border.color: theme.accent; radius: 6 }
                        onClicked: row.mediaRequested(row.record)
                    }
                }

                Rectangle {
                    visible: row.attachment.kind === "document" || row.attachment.kind === "audio" || row.attachment.kind === "video" || (!row.readyMedia && row.hasAttachment)
                    Layout.fillWidth: true
                    implicitHeight: 58
                    color: theme.active
                    radius: 8

                    RowLayout {
                        anchors.fill: parent
                        anchors.margins: 10
                        spacing: 10

                        Image {
                            source: row.attachment.kind === "document" ? "qrc:/icons/file.svg" : row.attachment.kind === "audio" ? "qrc:/icons/mic.svg" : row.attachment.kind === "video" ? "qrc:/icons/video.svg" : "qrc:/icons/file.svg"
                            sourceSize.width: 24
                            sourceSize.height: 24
                            Layout.preferredWidth: 24
                            Layout.preferredHeight: 24
                        }

                        ColumnLayout {
                            Layout.fillWidth: true
                            spacing: 2
                            Label {
                                textFormat: Text.PlainText
                                text: row.attachment.name || (row.attachment.kind === "sticker" ? "Sticker" : row.attachment.kind === "audio" ? row.audioLabel : row.attachment.kind || "Unsupported attachment")
                                color: theme.ink
                                elide: Text.ElideMiddle
                                Layout.fillWidth: true
                            }
                            Label {
                                textFormat: Text.PlainText
                                text: {
                                    if (row.attachment.state === "error")
                                        return row.attachment.error || "Media unavailable";
                                    if (row.attachment.state === "remote")
                                        return row.attachment.kind === "audio" ? "Not downloaded · " + row.audioLabel : "Not downloaded";
                                    if (row.attachment.kind === "audio" || row.attachment.kind === "video") {
                                        const mediaLabel = row.attachment.kind === "audio" ? row.audioLabel : row.attachment.kind;
                                        return row.attachment.duration ? Math.round(row.attachment.duration) + " s · " + mediaLabel : mediaLabel;
                                    }
                                    return row.attachment.size ? Math.ceil(row.attachment.size / 1024) + " KiB · " + (row.attachment.mime || row.attachment.kind) : (row.attachment.mime || row.attachment.kind || "Attachment");
                                }
                                color: row.attachment.state === "error" ? theme.danger : theme.muted
                                font.pixelSize: 11
                                elide: Text.ElideRight
                                Layout.fillWidth: true
                            }
                        }

                        ActionButton {
                            visible: row.readyMedia
                            theme: row.theme
                            objectName: "openMediaButton"
                            implicitWidth: 70
                            implicitHeight: 30
                            text: row.attachment.kind === "audio" || row.attachment.kind === "video" ? "Play" : "Open"
                            Accessible.name: "Open " + String(row.attachment.name || (row.attachment.kind === "audio" ? row.audioLabel.toLowerCase() : row.attachment.kind) || "media")
                            onClicked: {
                                if (row.attachment.kind === "audio" || row.attachment.kind === "video")
                                    row.mediaRequested(row.record);
                                else
                                    controller.openAttachment(record.id);
                            }
                        }
                        ActionButton {
                            id: downloadAction
                            objectName: "downloadMediaButton"
                            visible: !row.readyMedia && row.attachment.state !== "downloading"
                            enabled: !controller.messageBusy(record.id)
                            theme: row.theme
                            primary: true
                            implicitWidth: 76
                            implicitHeight: 30
                            padding: 0
                            font.pixelSize: 12
                            text: row.attachment.state === "error" ? "Retry" : "Download"
                            background: Rectangle {
                                radius: 15
                                color: downloadAction.theme.accent
                                border.width: downloadAction.activeFocus ? 2 : 0
                                border.color: downloadAction.theme.accent
                                opacity: downloadAction.enabled ? 1 : 0.4
                            }
                            onClicked: controller.downloadMessage(record.id)
                        }
                        Label {
                            visible: row.attachment.state === "downloading"
                            text: "Downloading…"
                            color: theme.muted
                            font.pixelSize: 11
                        }
                    }
                }
            }

            ColumnLayout {
                objectName: "albumPreviewCard"
                visible: row.isAlbum
                Layout.fillWidth: true
                spacing: 6

                Label {
                    objectName: "albumPreviewTitle"
                    textFormat: Text.PlainText
                    text: row.albumItems.length > 0 ? row.albumItems.length + " photos" : "Photo album"
                    color: theme.ink
                    font.weight: Font.DemiBold
                    Accessible.name: text
                }

                GridLayout {
                    objectName: "albumPreviewGrid"
                    visible: row.albumItems.length >= 2
                    Layout.fillWidth: true
                    columns: Math.min(3, Math.max(2, row.albumItems.length))
                    rowSpacing: 4
                    columnSpacing: 4
                    property int albumItemCount: albumPreviewRepeater.count

                    Repeater {
                        id: albumPreviewRepeater
                        model: row.albumItems
                        delegate: Rectangle {
                            required property int index
                            required property var modelData
                            objectName: "albumPreviewTile" + index
                            Layout.fillWidth: true
                            Layout.minimumWidth: 0
                            Layout.preferredWidth: 96
                            Layout.preferredHeight: 88
                            radius: 8
                            color: theme.active
                            border.color: theme.line
                            clip: true

                            Image {
                                objectName: "albumPreviewImage" + index
                                anchors.fill: parent
                                anchors.margins: 4
                                source: String(modelData.url || "")
                                fillMode: Image.PreserveAspectFit
                                sourceSize.width: 320
                                sourceSize.height: 240
                                asynchronous: true
                                Accessible.name: "Album photo " + (index + 1)
                            }
                            Label {
                                anchors.left: parent.left
                                anchors.top: parent.top
                                anchors.margins: 5
                                textFormat: Text.PlainText
                                text: "Photo " + (index + 1)
                                color: theme.ink
                                font.pixelSize: 10
                                padding: 3
                                background: Rectangle { color: theme.chrome; radius: 4; opacity: 0.9 }
                            }
                        }
                    }
                }

                Label {
                    objectName: "albumPreviewUnavailable"
                    visible: row.albumItems.length === 0
                    textFormat: Text.PlainText
                    text: "Album photo previews are unavailable."
                    color: theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }

                Label {
                    objectName: "albumLocalOnlyTimelineDisclosure"
                    visible: record.outgoing === true && record.status === "demo_sent"
                    textFormat: Text.PlainText
                    text: "Saved locally · not sent to WhatsApp"
                    color: theme.muted
                    font.pixelSize: 11
                    Accessible.name: text
                }
            }

            ColumnLayout {
                objectName: "contactCardsList"
                visible: row.isContact
                Layout.fillWidth: true
                spacing: 8

                Repeater {
                    model: row.isContact ? record.contacts : []
                    delegate: Rectangle {
                        required property var modelData
                        objectName: "contactCardSurface"
                        Layout.fillWidth: true
                        implicitWidth: Math.min(320, row.maxBubbleWidth - 20)
                        implicitHeight: contactContent.implicitHeight + 20
                        radius: 8
                        color: theme.active
                        border.color: theme.line

                        ColumnLayout {
                            id: contactContent
                            anchors.fill: parent
                            anchors.margins: 10
                            spacing: 4

                            Label {
                                objectName: "contactCardName"
                                textFormat: Text.PlainText
                                text: String(modelData.name || "Contact")
                                color: theme.ink
                                font.weight: Font.DemiBold
                                wrapMode: Text.WordWrap
                                Layout.fillWidth: true
                            }

                            Label {
                                objectName: "contactCardPhone"
                                visible: row.contactValues(modelData.phones) !== ""
                                textFormat: Text.PlainText
                                text: "Phone · " + row.contactValues(modelData.phones)
                                color: theme.ink
                                wrapMode: Text.WrapAnywhere
                                Layout.fillWidth: true
                            }

                            Label {
                                objectName: "contactCardEmail"
                                visible: row.contactValues(modelData.emails) !== ""
                                textFormat: Text.PlainText
                                text: "Email · " + row.contactValues(modelData.emails)
                                color: theme.ink
                                wrapMode: Text.WrapAnywhere
                                Layout.fillWidth: true
                            }

                            Label {
                                objectName: "contactCardOrganization"
                                visible: String(modelData.organization || "") !== ""
                                textFormat: Text.PlainText
                                text: "Organization · " + String(modelData.organization || "")
                                color: theme.muted
                                wrapMode: Text.WordWrap
                                Layout.fillWidth: true
                            }
                        }
                    }
                }
            }

            Rectangle {
                objectName: "locationCardSurface"
                visible: row.isLocation
                Layout.fillWidth: true
                implicitHeight: locationContent.implicitHeight + 20
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: locationContent
                    anchors.fill: parent
                    anchors.margins: 10
                    spacing: 5

                    Label {
                        objectName: "locationSnapshotLabel"
                        textFormat: Text.PlainText
                        text: row.location.is_live ? "Live location · received snapshot" : "Shared location"
                        color: theme.accent
                        font.weight: Font.DemiBold
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "locationCoordinates"
                        textFormat: Text.PlainText
                        text: "Coordinates · " + Number(row.location.latitude).toFixed(6) + "°, " + Number(row.location.longitude).toFixed(6) + "°"
                        color: theme.ink
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        visible: String(row.location.caption || "") !== ""
                        textFormat: Text.PlainText
                        text: "Caption"
                        color: theme.muted
                        font.pixelSize: 11
                        Accessible.name: "Location caption"
                    }
                    Label {
                        objectName: "locationCaption"
                        visible: String(row.location.caption || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.location.caption || "")
                        color: theme.ink
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    Label {
                        visible: String(row.location.name || "") !== ""
                        textFormat: Text.PlainText
                        text: "Name"
                        color: theme.muted
                        font.pixelSize: 11
                        Accessible.name: "Location name label"
                    }
                    Label {
                        objectName: "locationName"
                        visible: String(row.location.name || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.location.name || "")
                        color: theme.ink
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "locationAddress"
                        visible: String(row.location.address || "") !== ""
                        textFormat: Text.PlainText
                        text: "Address · " + String(row.location.address || "")
                        color: theme.muted
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                }
            }

            Rectangle {
                objectName: "textCardCardSurface"
                visible: row.isTextCard
                Layout.fillWidth: true
                implicitHeight: textCardContent.implicitHeight + 18
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: textCardContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 5

                    Label {
                        objectName: "textCardTitle"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.textCardProjection.title || "")
                        Accessible.role: Accessible.StaticText
                        Accessible.name: text
                        color: theme.ink
                        font.weight: Font.DemiBold
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "textCardSubtitle"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.textCardProjection.subtitle || "")
                        Accessible.role: Accessible.StaticText
                        Accessible.name: text
                        color: theme.ink
                        font.weight: Font.Normal
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "textCardBody"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.textCardProjection.body || "")
                        Accessible.role: Accessible.StaticText
                        Accessible.name: text
                        color: theme.ink
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "textCardFooter"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.textCardProjection.footer || "")
                        Accessible.role: Accessible.StaticText
                        Accessible.name: text
                        color: theme.muted
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "textCardDisclosure"
                        textFormat: Text.PlainText
                        text: "Display only"
                        Accessible.role: Accessible.StaticText
                        Accessible.name: text
                        color: theme.muted
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    Repeater {
                        model: row.isTextCard ? (row.textCardProjection.labels || []) : []
                        delegate: Label {
                            required property int index
                            required property string modelData
                            objectName: "textCardLabel" + (index + 1)
                            textFormat: Text.PlainText
                            text: modelData
                            color: theme.ink
                            background: null
                            padding: 0
                            wrapMode: Text.WrapAnywhere
                            Layout.fillWidth: true
                            Accessible.role: Accessible.StaticText
                            Accessible.name: "Template label (display only): " + text
                        }
                    }
                }
            }

            Rectangle {
                objectName: "buttonsCardSurface"
                visible: row.isButtons
                Layout.fillWidth: true
                implicitHeight: buttonsContent.implicitHeight + 18
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: buttonsContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 5

                    Label {
                        objectName: "buttonsHeader"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.buttonsProjection.header || "")
                        color: theme.ink
                        font.weight: Font.DemiBold
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "buttonsBody"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.buttonsProjection.body || "")
                        color: theme.ink
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "buttonsFooter"
                        visible: text !== ""
                        textFormat: Text.PlainText
                        text: String(row.buttonsProjection.footer || "")
                        color: theme.muted
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                    }
                    Label {
                        objectName: "buttonsDisclosure"
                        textFormat: Text.PlainText
                        text: row.canSelectButtonResponse ? "Local demo only · choosing a response records it here; nothing is sent to WhatsApp." : row.buttonReplyState === "local" ? "Local demo · response recorded here only." : controller.demo ? "Display only · local response selection is unavailable." : "Display only · live response sending is unavailable."
                        color: theme.muted
                        Accessible.name: text
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    Repeater {
                        model: row.isButtons ? row.buttonLabels : []
                        delegate: Label {
                            required property int index
                            required property string modelData
                            objectName: "buttonsLabel" + (index + 1)
                            textFormat: Text.PlainText
                            text: modelData
                            color: theme.ink
                            background: null
                            padding: 0
                            wrapMode: Text.WrapAnywhere
                            Layout.fillWidth: true
                            Accessible.role: Accessible.StaticText
                            Accessible.name: "Button label: " + text
                        }
                    }
                    ActionButton {
                        objectName: "buttonsSelectButton"
                        visible: row.canSelectButtonResponse
                        enabled: row.canSelectButtonResponse
                        theme: row.theme
                        text: "Choose a response"
                        primary: true
                        Accessible.name: "Choose a response to this ButtonsMessage in the local demo"
                        Layout.fillWidth: true
                        onClicked: buttonsReplyDialog.open()
                    }
                    Label {
                        objectName: "buttonReplyStatus"
                        visible: row.buttonReplyStatusText !== ""
                        textFormat: Text.PlainText
                        text: row.buttonReplyStatusText
                        color: theme.muted
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                        Accessible.name: text
                    }
                }
            }

            Rectangle {
                objectName: "listCardSurface"
                visible: row.isList
                Layout.fillWidth: true
                implicitHeight: listContent.implicitHeight + 18
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: listContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 5

                    Label {
                        objectName: "listTitle"
                        textFormat: Text.PlainText
                        text: String(row.listProjection.title || "")
                        color: theme.ink
                        font.weight: Font.DemiBold
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                        Accessible.name: "List title: " + text
                    }
                    Label {
                        objectName: "listDescription"
                        visible: String(row.listProjection.description || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.listProjection.description || "")
                        color: theme.muted
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    ColumnLayout {
                        objectName: "listSections"
                        visible: row.isList
                        Layout.fillWidth: true
                        spacing: 5
                        Repeater {
                            model: row.isList ? row.listSectionValues(row.listProjection.sections) : []
                            delegate: ColumnLayout {
                                required property int index
                                required property var modelData
                                property int sectionIndex: index + 1
                                objectName: "listSection" + sectionIndex
                                Layout.fillWidth: true
                                spacing: 3

                                Label {
                                    objectName: "listSectionTitle" + sectionIndex
                                    visible: String(modelData.title || "") !== ""
                                    textFormat: Text.PlainText
                                    text: String(modelData.title || "")
                                    color: theme.accent
                                    font.pixelSize: 11
                                    font.weight: Font.DemiBold
                                    wrapMode: Text.WordWrap
                                    Layout.fillWidth: true
                                }
                                Repeater {
                                    model: row.listRowValues(modelData.rows)
                                    delegate: ColumnLayout {
                                        required property int index
                                        required property var modelData
                                        objectName: "listRow" + sectionIndex + "_" + (index + 1)
                                        Layout.fillWidth: true
                                        spacing: 1

                                        Label {
                                            objectName: "listRowTitle" + sectionIndex + "_" + (index + 1)
                                            textFormat: Text.PlainText
                                            text: String(modelData.title || "")
                                            color: theme.ink
                                            wrapMode: Text.WordWrap
                                            Layout.fillWidth: true
                                            Accessible.name: "List row: " + text
                                        }
                                        Label {
                                            objectName: "listRowDescription" + sectionIndex + "_" + (index + 1)
                                            visible: String(modelData.description || "") !== ""
                                            textFormat: Text.PlainText
                                            text: String(modelData.description || "")
                                            color: theme.muted
                                            wrapMode: Text.WordWrap
                                            Layout.fillWidth: true
                                        }
                                    }
                                }
                            }
                        }
                    }
                    Label {
                        objectName: "listFooter"
                        visible: String(row.listProjection.footer || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.listProjection.footer || "")
                        color: theme.muted
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    ActionButton {
                        objectName: "listSelectButton"
                        visible: row.canSelectListRow
                        enabled: row.canSelectListRow
                        theme: row.theme
                        text: String(row.listProjection.button_label || "Choose")
                        primary: true
                        Accessible.name: "Choose a response to " + String(row.listProjection.title || "list")
                        Layout.fillWidth: true
                        onClicked: listReplyDialog.open()
                    }
                    Label {
                        objectName: "listButtonLabel"
                        visible: !row.canSelectListRow
                        textFormat: Text.PlainText
                        text: String(row.listProjection.button_label || "List") + " · " + row.listReplyStatusText
                        color: theme.muted
                        font.pixelSize: 11
                        Accessible.name: "List response status: " + row.listReplyStatusText + ": " + String(row.listProjection.button_label || "")
                        Layout.fillWidth: true
                    }
                }
            }

            Rectangle {
                objectName: "pollCardSurface"
                visible: row.isPoll
                Layout.fillWidth: true
                implicitHeight: pollContent.implicitHeight + 18
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: pollContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 4

                    Label {
                        objectName: "pollTypeLabel"
                        textFormat: Text.PlainText
                        text: "Poll"
                        color: theme.accent
                        font.pixelSize: 11
                        font.weight: Font.DemiBold
                        Accessible.name: "Poll"
                    }
                    Label {
                        objectName: "pollQuestion"
                        textFormat: Text.PlainText
                        text: String(row.poll.question || "")
                        color: theme.ink
                        font.weight: Font.DemiBold
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                        Accessible.name: "Poll question: " + text
                    }
                    ColumnLayout {
                        objectName: "pollOptionsList"
                        visible: row.isPoll
                        Layout.fillWidth: true
                        spacing: 2
                        Repeater {
                            model: row.isPoll ? row.pollOptionValues(row.poll.options) : []
                            delegate: RowLayout {
                                required property int index
                                required property var modelData
                                objectName: "pollOptionRow" + (index + 1)
                                Layout.fillWidth: true
                                spacing: 8
                                Rectangle {
                                    Layout.preferredWidth: 8
                                    Layout.preferredHeight: 8
                                    radius: 4
                                    color: "transparent"
                                    border.color: theme.muted
                                }
                                Label {
                                    objectName: "pollOptionLabel" + (index + 1)
                                    textFormat: Text.PlainText
                                    text: String(modelData || "")
                                    color: theme.ink
                                    wrapMode: Text.WordWrap
                                    Layout.fillWidth: true
                                    Accessible.name: "Option " + (index + 1) + ": " + text
                                }
                            }
                        }
                    }
                    Label {
                        objectName: "pollSelectableCount"
                        visible: row.hasPollSelectableCount
                        textFormat: Text.PlainText
                        text: "Select up to " + Number(row.poll.selectable_options_count) + " option" + (Number(row.poll.selectable_options_count) === 1 ? "" : "s")
                        color: theme.muted
                        font.pixelSize: 11
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                        Accessible.name: "Selectable options count: " + Number(row.poll.selectable_options_count)
                    }
                    Loader {
                        active: row.canVote
                        Layout.fillWidth: true
                        sourceComponent: ActionButton {
                            objectName: "pollVoteButton"
                            theme: row.theme
                            readonly property bool voteBlocked: String(record.poll_vote_state || "").length > 0 || controller.pollVoteBlockedTargets.indexOf(record.id) >= 0
                            text: voteBlocked ? "Vote pending · check outcome" : "Vote…"
                            enabled: !voteBlocked
                            onClicked: row.pollVoteRequested(row.record)
                        }
                    }
                }
            }

            Rectangle {
                objectName: "pollResultsCardSurface"
                visible: row.isPollResults
                Layout.fillWidth: true
                implicitHeight: pollResultsContent.implicitHeight + 18
                radius: 8
                color: theme.active
                border.color: theme.line

                ColumnLayout {
                    id: pollResultsContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 5

                    Label {
                        objectName: "pollResultsTypeLabel"
                        textFormat: Text.PlainText
                        text: "Poll results · read-only"
                        color: theme.accent
                        font.pixelSize: 11
                        font.weight: Font.DemiBold
                        Accessible.name: text
                    }
                    Label {
                        objectName: "pollResultsQuestion"
                        textFormat: Text.PlainText
                        text: String(row.pollResults.question || "")
                        color: theme.ink
                        font.weight: Font.DemiBold
                        wrapMode: Text.WrapAnywhere
                        Layout.fillWidth: true
                        Accessible.name: "Poll results question: " + text
                    }
                    ColumnLayout {
                        objectName: "pollResultsOptionsList"
                        Layout.fillWidth: true
                        spacing: 3
                        Repeater {
                            model: row.isPollResults ? row.pollResultOptionValues(row.pollResults.options) : []
                            delegate: RowLayout {
                                required property int index
                                required property var modelData
                                objectName: "pollResultsOptionRow" + (index + 1)
                                Layout.fillWidth: true
                                spacing: 8

                                Label {
                                    objectName: "pollResultsOptionLabel" + (index + 1)
                                    textFormat: Text.PlainText
                                    text: String(modelData.name || "")
                                    color: theme.ink
                                    wrapMode: Text.WrapAnywhere
                                    Layout.fillWidth: true
                                    Accessible.name: "Option " + (index + 1) + ": " + text
                                }
                                Label {
                                    objectName: "pollResultsOptionCount" + (index + 1)
                                    textFormat: Text.PlainText
                                    text: row.pollVoteCountText(modelData.vote_count)
                                    color: theme.muted
                                    horizontalAlignment: Text.AlignRight
                                    Accessible.name: "Vote count: " + String(modelData.vote_count)
                                }
                            }
                        }
                    }
                }
            }

            TextEdit {
                id: body
                objectName: "messageBody"
                visible: !row.isContact && !row.isLocation && !row.isPoll && !row.isPollResults && !row.isList && !row.isButtons && !row.isTextCard && text !== ""
                text: record.deleted ? "This message was deleted." : row.isLegacyPollResults ? "Poll results unavailable." : (row.isLegacyPoll || row.isLegacyList || record.kind === "call") ? String(record.body || "") : (row.isPoll || row.isPollResults || row.isList || row.isButtons || row.isTextCard) ? "" : richBody(record.body || "")
                textFormat: (row.isLegacyPoll || row.isLegacyList || row.isLegacyPollResults || row.isList || record.kind === "call") ? TextEdit.PlainText : TextEdit.RichText
                color: theme.ink
                font.family: theme.font.family
                font.pixelSize: 14
                readOnly: true
                selectByMouse: true
                wrapMode: TextEdit.Wrap
                Layout.fillWidth: true
                Layout.preferredHeight: visible ? contentHeight : 0
                onLinkActivated: if (!row.isLegacyPoll && !row.isLegacyList && !row.isList) controller.openExternalLink(link)
                Accessible.name: record.deleted ? "This message was deleted." : (record.body || "Message")
            }

            Rectangle {
                objectName: "linkPreviewCard"
                visible: row.isLinkPreview
                Layout.fillWidth: true
                implicitHeight: linkPreviewContent.implicitHeight + 18
                color: theme.active
                radius: 8
                border.color: theme.line
                Accessible.name: "Link preview for " + String(row.linkPreview.site || "")

                ColumnLayout {
                    id: linkPreviewContent
                    anchors.fill: parent
                    anchors.margins: 9
                    spacing: 3

                    Label {
                        objectName: "linkPreviewSite"
                        textFormat: Text.PlainText
                        text: String(row.linkPreview.site || "")
                        color: theme.accent
                        font.pixelSize: 11
                        font.weight: Font.DemiBold
                        wrapMode: Text.WrapAnywhere
                        maximumLineCount: 1
                        elide: Text.ElideRight
                        Layout.fillWidth: true
                        Accessible.name: "Link preview site: " + text
                    }
                    Label {
                        objectName: "linkPreviewTitle"
                        visible: String(row.linkPreview.title || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.linkPreview.title || "")
                        color: theme.ink
                        font.pixelSize: 14
                        font.weight: Font.DemiBold
                        wrapMode: Text.WordWrap
                        maximumLineCount: 2
                        elide: Text.ElideRight
                        Layout.fillWidth: true
                        Accessible.name: "Link preview title: " + text
                    }
                    Label {
                        objectName: "linkPreviewDescription"
                        visible: String(row.linkPreview.description || "") !== ""
                        textFormat: Text.PlainText
                        text: String(row.linkPreview.description || "")
                        color: theme.muted
                        font.pixelSize: 12
                        wrapMode: Text.WordWrap
                        maximumLineCount: 3
                        elide: Text.ElideRight
                        Layout.fillWidth: true
                        Accessible.name: "Link preview description: " + text
                    }
                    Label {
                        objectName: "linkPreviewPrivacy"
                        textFormat: Text.PlainText
                        text: "Page and thumbnail not fetched"
                        color: theme.muted
                        font.pixelSize: 10
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                        Accessible.name: text
                    }
                }
            }

            Flow {
                objectName: "messageReactions"
                visible: Array.isArray(record.reactions) && record.reactions.length > 0
                Layout.fillWidth: true
                spacing: 4
                Repeater {
                    model: visible ? record.reactions : []
                    delegate: Label {
                        required property var modelData
                        textFormat: Text.PlainText
                        text: (modelData.reaction || modelData.emoji || "") + (Number(modelData.count || 0) > 1 ? " " + modelData.count : "")
                        Accessible.name: "Reaction " + text
                        color: theme.ink
                        leftPadding: 8; rightPadding: 8; topPadding: 3; bottomPadding: 3
                        background: Rectangle { color: theme.active; radius: 10; border.color: theme.line }
                    }
                }
            }

            Label {
                objectName: "messageAnnotations"
                visible: !!record.edited || !!record.starred || !!record.pinned || !!record.forwarded
                textFormat: Text.PlainText
                text: [record.forwarded ? "Forwarded" : "", record.edited ? "Edited" : "", record.starred ? "Starred" : "", record.pinned ? "Pinned" : ""].filter(function(item) { return item !== ""; }).join(" · ")
                color: theme.muted
                font.pixelSize: 10
                Accessible.name: text
            }

            RowLayout {
                id: metadataRow
                Layout.alignment: Qt.AlignRight
                spacing: 6
                Label {
                    textFormat: Text.PlainText
                    text: controller.timeLabel(record.timestamp)
                    color: theme.muted
                    font.pixelSize: 10
                }
                Label {
                    objectName: "messageStatusLabel"
                    textFormat: Text.PlainText
                    visible: record.outgoing
                    text: controller.messageStatusLabel(String(record.status || ""))
                    color: theme.muted
                    font.pixelSize: 10
                    Accessible.name: "Message status: " + controller.messageStatusDescription(String(record.status || ""))
                }
            }
        }

        IconButton {
            objectName: "messageMenuButton"
            anchors.top: parent.top
            anchors.right: parent.right
            anchors.margins: 2
            implicitWidth: 26
            implicitHeight: 24
            iconName: "chevron"
            label: "Message actions"
            tint: theme.muted
            hoverColor: theme.active
            opacity: hover.hovered || hovered || activeFocus ? 1 : 0
            onClicked: row.menuRequested(row.record, this)
        }
    }

    Dialog {
        id: listReplyDialog
        objectName: "listReplyDialog"
        anchors.centerIn: Overlay.overlay
        width: Math.min(440, Math.max(280, row.width - 32))
        title: "Choose a response"
        modal: true
        focus: true
        padding: 16
        standardButtons: Dialog.NoButton
        closePolicy: Popup.CloseOnEscape | Popup.CloseOnPressOutside
        background: Rectangle { color: row.theme.chrome; radius: 12; border.color: row.theme.line }
        header: Label {
            textFormat: Text.PlainText
            text: listReplyDialog.title
            color: row.theme.ink
            font.pixelSize: 16
            font.weight: Font.DemiBold
            leftPadding: 16
            rightPadding: 16
            topPadding: 14
            bottomPadding: 8
        }
        contentItem: ColumnLayout {
            spacing: 8
            Label {
                textFormat: Text.PlainText
                text: "Select one response. This choice will be sent to the list sender."
                color: row.theme.muted
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }
            ScrollView {
                id: listReplyOptionsView
                objectName: "listReplyOptionsView"
                clip: true
                contentWidth: availableWidth
                Layout.fillWidth: true
                Layout.preferredHeight: Math.min(360, Math.max(96, replySections.implicitHeight))

                ColumnLayout {
                    id: replySections
                    width: listReplyOptionsView.availableWidth
                    spacing: 8
                    Repeater {
                        model: row.listSectionValues(row.listProjection.sections)
                        delegate: ColumnLayout {
                            id: replySection
                            required property int index
                            required property var modelData
                            readonly property int replySectionIndex: index
                            Layout.fillWidth: true
                            spacing: 4

                            Label {
                                visible: String(replySection.modelData.title || "") !== ""
                                textFormat: Text.PlainText
                                text: String(replySection.modelData.title || "")
                                color: row.theme.accent
                                font.pixelSize: 11
                                font.weight: Font.DemiBold
                                wrapMode: Text.WordWrap
                                Layout.fillWidth: true
                            }
                            Repeater {
                                model: row.listRowValues(replySection.modelData.rows)
                                delegate: Button {
                                    id: replyOption
                                    required property int index
                                    required property var modelData
                                    objectName: "listReplyOption" + replySection.replySectionIndex + "_" + index
                                    text: String(modelData.title || "")
                                    Layout.fillWidth: true
                                    implicitHeight: Math.max(44, replyOptionContent.implicitHeight + 16)
                                    padding: 8
                                    Accessible.name: "Select list option: " + String(modelData.title || "") + (String(modelData.description || "") !== "" ? " · " + String(modelData.description) : "")
                                    background: Rectangle {
                                        radius: 6
                                        color: replyOption.hovered ? row.theme.active : row.theme.chrome
                                        border.width: replyOption.activeFocus ? 2 : 1
                                        border.color: replyOption.activeFocus ? row.theme.accent : row.theme.line
                                        opacity: replyOption.enabled ? 1 : 0.5
                                    }
                                    contentItem: ColumnLayout {
                                        id: replyOptionContent
                                        spacing: 2
                                        Label {
                                            textFormat: Text.PlainText
                                            text: String(replyOption.modelData.title || "")
                                            color: row.theme.ink
                                            wrapMode: Text.WordWrap
                                            Layout.fillWidth: true
                                        }
                                        Label {
                                            visible: String(replyOption.modelData.description || "") !== ""
                                            textFormat: Text.PlainText
                                            text: String(replyOption.modelData.description || "")
                                            color: row.theme.muted
                                            wrapMode: Text.WordWrap
                                            Layout.fillWidth: true
                                        }
                                    }
                                    onClicked: {
                                        if (row.controller.performMessageAction("list.reply", row.record, {
                                            "section_index": replySection.replySectionIndex,
                                            "row_index": replyOption.index
                                        }))
                                            listReplyDialog.close();
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
        footer: Item {
            implicitHeight: 48
            RowLayout {
                anchors.fill: parent
                anchors.rightMargin: 16
                anchors.leftMargin: 16
                spacing: 8
                Item { Layout.fillWidth: true }
                ActionButton {
                    objectName: "cancelListReply"
                    theme: row.theme
                    text: "Cancel"
                    onClicked: listReplyDialog.reject()
                }
            }
        }
    }

    Dialog {
        id: buttonsReplyDialog
        objectName: "buttonsReplyDialog"
        anchors.centerIn: Overlay.overlay
        width: Math.min(440, Math.max(280, row.width - 32))
        title: "Choose a demo response"
        modal: true
        focus: true
        padding: 16
        standardButtons: Dialog.NoButton
        closePolicy: Popup.CloseOnEscape | Popup.CloseOnPressOutside
        background: Rectangle { color: row.theme.chrome; radius: 12; border.color: row.theme.line }
        header: Label {
            textFormat: Text.PlainText
            text: buttonsReplyDialog.title
            color: row.theme.ink
            font.pixelSize: 16
            font.weight: Font.DemiBold
            leftPadding: 16
            rightPadding: 16
            topPadding: 14
            bottomPadding: 8
        }
        contentItem: ColumnLayout {
            spacing: 8
            Label {
                textFormat: Text.PlainText
                text: "Choose one displayed response to record it in this local demo. Nothing is sent to WhatsApp."
                color: row.theme.muted
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }
            ScrollView {
                id: buttonsReplyOptionsView
                objectName: "buttonsReplyOptionsView"
                clip: true
                contentWidth: availableWidth
                Layout.fillWidth: true
                Layout.preferredHeight: Math.min(260, Math.max(64, buttonsReplyOptions.implicitHeight))

                ColumnLayout {
                    id: buttonsReplyOptions
                    width: buttonsReplyOptionsView.availableWidth
                    spacing: 6
                    Repeater {
                        model: row.isButtons ? row.buttonLabels : []
                        delegate: Button {
                            id: buttonsReplyOption
                            required property int index
                            required property string modelData
                            objectName: "buttonsReplyOption" + (index + 1)
                            text: modelData
                            Layout.fillWidth: true
                            implicitHeight: Math.max(44, buttonsReplyOptionContent.implicitHeight + 16)
                            padding: 8
                            Accessible.name: "Select local demo response: " + modelData
                            background: Rectangle {
                                radius: 6
                                color: buttonsReplyOption.hovered ? row.theme.active : row.theme.chrome
                                border.width: buttonsReplyOption.activeFocus ? 2 : 1
                                border.color: buttonsReplyOption.activeFocus ? row.theme.accent : row.theme.line
                                opacity: buttonsReplyOption.enabled ? 1 : 0.5
                            }
                            contentItem: Label {
                                id: buttonsReplyOptionContent
                                textFormat: Text.PlainText
                                text: buttonsReplyOption.modelData
                                color: row.theme.ink
                                wrapMode: Text.WordWrap
                                horizontalAlignment: Text.AlignHCenter
                                verticalAlignment: Text.AlignVCenter
                            }
                            onClicked: {
                                if (row.controller.performMessageAction("button.reply", row.record, {
                                    "button_index": buttonsReplyOption.index
                                }))
                                    buttonsReplyDialog.close();
                            }
                        }
                    }
                }
            }
        }
        footer: Item {
            implicitHeight: 48
            RowLayout {
                anchors.fill: parent
                anchors.rightMargin: 16
                anchors.leftMargin: 16
                spacing: 8
                Item { Layout.fillWidth: true }
                ActionButton {
                    objectName: "cancelButtonsReply"
                    theme: row.theme
                    text: "Cancel"
                    onClicked: buttonsReplyDialog.reject()
                }
            }
        }
    }
}
