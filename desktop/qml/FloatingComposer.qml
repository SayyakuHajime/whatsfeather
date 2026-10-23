import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

Item {
    id: floating

    required property var theme
    required property var controller
    readonly property bool hasAttachment: !!controller.attachment.name

    signal attachRequested(string kind)
    signal beforeSend()

    function dismissPopups() {
        if (expressions.visible) {
            expressions.close();
            return true;
        }
        if (attachmentMenu.visible) {
            attachmentMenu.close();
            return true;
        }
        return false;
    }

    function focusEditor() {
        editor.forceActiveFocus();
    }

    function insertEmoji(text) {
        let start = editor.selectionStart;
        let end = editor.selectionEnd;
        if (start !== end)
            editor.remove(start, end);

        editor.insert(editor.cursorPosition, text);
        editor.forceActiveFocus();
    }

    implicitHeight: Math.min(150, Math.max(52, editor.contentHeight + 20)) + (hasAttachment ? 80 : 0) + 12

    Rectangle {
        objectName: "floatingComposerSurface"
        anchors.fill: parent
        anchors.leftMargin: 12
        anchors.rightMargin: 12
        anchors.bottomMargin: 12
        radius: 28
        color: floating.theme.chrome
        border.width: editor.activeFocus ? 1 : 0
        border.color: floating.theme.line

        ColumnLayout {
            anchors.fill: parent
            anchors.leftMargin: 10
            anchors.rightMargin: 10
            anchors.topMargin: 6
            anchors.bottomMargin: 6
            spacing: 4

            RowLayout {
                visible: floating.hasAttachment
                Layout.fillWidth: true
                Layout.preferredHeight: 72
                spacing: 12

                Rectangle {
                    Layout.preferredWidth: 60
                    Layout.preferredHeight: 60
                    radius: 8
                    color: floating.theme.active

                    Image {
                        anchors.fill: parent
                        anchors.margins: 4
                        source: floating.hasAttachment && (controller.attachment.kind === "image" || controller.attachment.kind === "gif") ? (controller.attachment.url || "") : ""
                        visible: floating.hasAttachment && (controller.attachment.kind === "image" || controller.attachment.kind === "gif")
                        asynchronous: true
                        sourceSize.width: 120
                        sourceSize.height: 120
                        fillMode: Image.PreserveAspectFit
                    }

                    Image {
                        anchors.centerIn: parent
                        width: 24
                        height: 24
                        source: controller.attachment.kind === "video" ? "qrc:/icons/video.svg" : "qrc:/icons/file.svg"
                        visible: controller.attachment.kind === "document" || controller.attachment.kind === "video"
                    }

                }

                ColumnLayout {
                    Layout.fillWidth: true
                    spacing: 4

                    Label {
                        textFormat: Text.PlainText
                        text: controller.attachment.name || ""
                        color: floating.theme.ink
                        elide: Text.ElideRight
                        Layout.fillWidth: true
                    }

                    Label {
                        textFormat: Text.PlainText
                        text: controller.attachment.mode === "audio" || controller.attachment.mode === "voice-note"
                            ? (controller.attachment.is_voice_note ? "Voice note" : "Audio file") + " · OGG/Opus · " + (controller.attachment.duration || 0) + " s" + (controller.attachment.state === "preparing" ? " · Preparing…" : controller.attachment.state === "error" ? " · " + (controller.attachment.error || "Audio validation failed") : " · " + (controller.demo ? "local demo" : "ready to send"))
                            : (controller.attachment.mode === "video" || controller.attachment.kind === "video"
                                ? (controller.attachment.state === "preparing" ? "Preparing video…"
                                    : controller.attachment.state === "error" ? (controller.attachment.error || "Video validation failed")
                                    : controller.attachment.kind === "video" && controller.attachment.video_codec === "h264"
                                        ? "MP4 · H.264 AVC · yuv420p · " + (controller.attachment.audio_codec === "aac" ? "AAC-LC" : "no audio") + (controller.attachment.width && controller.attachment.height ? " · " + controller.attachment.width + " × " + controller.attachment.height : "") + (controller.attachment.duration ? " · " + controller.attachment.duration + " s" : "")
                                        : "Video metadata unavailable")
                                : (controller.attachment.kind === "gif" ? "GIF · " : "") + Math.ceil((controller.attachment.prepared_size || controller.attachment.size || 0) / 1024) + " KiB · " + (controller.demo ? "local demo" : "ready to send"))
                        color: floating.theme.muted
                        font.pixelSize: 11
                    }

                }

                IconButton {
                    objectName: "cancelAttachment"
                    iconName: "close"
                    label: "Remove pending attachment"
                    tint: floating.theme.muted
                    hoverColor: floating.theme.active
                    onClicked: controller.clearAttachment()
                }

            }

            RowLayout {
                Layout.fillWidth: true
                Layout.fillHeight: true
                spacing: 4

                IconButton {
                    objectName: "attachmentButton"
                    iconName: "attach"
                    label: "Attach a file"
                    tint: floating.theme.muted
                    hoverColor: floating.theme.active
                    enabled: controller.connected && (controller.demo || controller.connectionState === "connected") && !controller.voiceRecording && !controller.voiceEncoding
                    onClicked: attachmentMenu.popup()
                }

                IconButton {
                    id: expressionsButton

                    objectName: "expressionButton"
                    iconName: "emoji"
                    label: "Emoji and GIF"
                    tint: floating.theme.muted
                    hoverColor: floating.theme.active
                    enabled: controller.connected && !controller.voiceRecording && !controller.voiceEncoding
                    onClicked: expressions.open()
                }

                ScrollView {
                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    clip: true

                    TextArea {
                        id: editor

                        objectName: "composer"
                        placeholderText: floating.hasAttachment ? "Add a caption" : "Type a message"
                        placeholderTextColor: floating.theme.muted
                        color: floating.theme.ink
                        background: null
                        wrapMode: TextEdit.Wrap
                        selectByMouse: true
                        font.pixelSize: 14
                        leftPadding: 8
                        rightPadding: 8
                        topPadding: 10
                        bottomPadding: 8
                        Accessible.name: "Message draft"
                        enabled: controller.connected && !controller.voiceRecording && !controller.voiceEncoding
                        onTextChanged: {
                            if (text !== controller.draft)
                                controller.setDraft(text);

                        }
                        Keys.onReturnPressed: function(event) {
                            if (!(event.modifiers & Qt.ShiftModifier)) {
                                floating.beforeSend();
                                controller.send();
                                event.accepted = true;
                            } else {
                                event.accepted = false;
                            }
                        }
                        Keys.onEnterPressed: function(event) {
                            if (!(event.modifiers & Qt.ShiftModifier)) {
                                floating.beforeSend();
                                controller.send();
                                event.accepted = true;
                            } else {
                                event.accepted = false;
                            }
                        }

                        Connections {
                            function onDraftChanged() {
                                if (editor.text !== controller.draft)
                                    editor.text = controller.draft;

                            }

                            target: controller
                        }

                    }

                }

                Label {
                    objectName: "voiceRecordingStatus"
                    readonly property bool visibleState: controller.voiceRecording || controller.voiceEncoding || (controller.voiceRecordingError.length > 0 && controller.draft.length === 0 && !floating.hasAttachment)
                    visible: visibleState
                    Layout.preferredWidth: visible ? Math.min(150, implicitWidth) : 0
                    Layout.maximumWidth: 150
                    textFormat: Text.PlainText
                    text: controller.voiceEncoding ? "Preparing voice note…" : controller.voiceRecording ? "Recording · " + controller.voiceRecordingSeconds + " s" : controller.voiceRecordingError
                    color: controller.voiceRecordingError.length > 0 || controller.voiceRecording ? floating.theme.danger : floating.theme.muted
                    font.pixelSize: 11
                    elide: Text.ElideRight
                    Accessible.name: text
                }

                IconButton {
                    objectName: "sendButton"
                    iconName: "send"
                    label: controller.demo ? "Save demo message locally" : "Send message"
                    enabled: controller.connected && !controller.sending && !controller.voiceRecording && !controller.voiceEncoding && (controller.draft.trim().length > 0 || floating.hasAttachment)
                    visible: controller.draft.trim().length > 0 || floating.hasAttachment
                    tint: theme.dark ? "#062e26" : "#ffffff"
                    onClicked: {
                        floating.beforeSend();
                        controller.send();
                    }

                    background: Rectangle {
                        radius: 22
                        color: parent.enabled ? floating.theme.accent : floating.theme.active
                    }

                }

                IconButton {
                    objectName: "voiceNoteButton"
                    visible: !floating.hasAttachment && (controller.draft.length === 0 || controller.voiceRecording || controller.voiceEncoding)
                    enabled: controller.connected && controller.selectedId.length > 0 && !controller.sending && !controller.voiceEncoding && (controller.voiceRecording || (controller.draft.length === 0 && !floating.hasAttachment))
                    iconName: controller.voiceRecording ? "stop" : "mic"
                    label: controller.voiceRecording ? "Stop and review voice note" : controller.voiceEncoding ? "Preparing voice note" : "Record a voice note"
                    Accessible.description: controller.voiceRecording
                        ? "Microphone capture is active. Stop to review the recording or cancel to discard it. It will not be sent automatically."
                        : "Microphone capture starts only after activation. The voice note must be reviewed before sending."
                    tint: controller.voiceRecording ? floating.theme.surface : floating.theme.muted
                    hoverColor: floating.theme.active
                    onClicked: {
                        if (controller.voiceRecording)
                            controller.stopVoiceNoteRecording();
                        else if (!controller.voiceEncoding)
                            controller.startVoiceNoteRecording();
                    }
                    background: Rectangle {
                        radius: 20
                        color: controller.voiceRecording ? floating.theme.danger : parent.hovered ? floating.theme.active : "transparent"
                        border.width: parent.activeFocus ? 2 : 0
                        border.color: floating.theme.line
                    }
                }

                IconButton {
                    objectName: "cancelVoiceNoteButton"
                    visible: controller.voiceRecording || controller.voiceEncoding
                    enabled: visible
                    iconName: "close"
                    label: "Cancel voice note and discard recording"
                    tint: floating.theme.muted
                    hoverColor: floating.theme.active
                    onClicked: controller.cancelVoiceNoteRecording()
                }

            }

        }

    }

    Menu {
        id: attachmentMenu

        objectName: "attachmentMenu"

        MenuItem {
            text: "Photos and GIFs…"
            onTriggered: floating.attachRequested("image")
        }

        MenuItem {
            objectName: "photoAlbumAttachmentAction"
            text: "Photo album…"
            enabled: bridge.demo && app.featureAvailable("multi-attachment-albums") &&
                     app.capabilityAvailable("attachment.album.prepare") &&
                     app.capabilityAvailable("message.album.send") &&
                     app.capabilityAvailable("attachment.album.release")
            Accessible.name: "Choose 2 to 10 local still images for a photo album"
            Accessible.description: enabled ? "Review a bounded local demo album before saving one synthetic message. Nothing is uploaded to WhatsApp." : bridge.capabilityReason("attachment.album.prepare")
            onTriggered: floating.attachRequested("photo-album")
        }

        MenuItem {
            objectName: "videoAttachmentAction"
            text: "Video…"
            enabled: app.featureAvailable("video")
            Accessible.name: "Select MP4 video"
            Accessible.description: enabled ? "MP4 with validated H.264 AVC 4:2:0 and optional AAC-LC" : bridge.capabilityReason("video")
            onTriggered: floating.attachRequested("video")
        }

        MenuItem {
            objectName: "audioAttachmentAction"
            text: "Audio file (OGG/Opus)…"
            Accessible.name: "Choose an OGG/Opus audio file"
            onTriggered: floating.attachRequested("audio")
        }

        MenuItem {
            objectName: "voiceNoteAttachmentAction"
            text: "Voice note from file (OGG/Opus)…"
            Accessible.name: "Choose an OGG/Opus voice note from a file; microphone recording is unavailable"
            onTriggered: floating.attachRequested("voice-note")
        }

        MenuItem {
            objectName: "documentAttachmentAction"
            text: "Document…"
            Accessible.name: "Choose a document"
            Accessible.description: "Send the original file bytes with an optional caption"
            onTriggered: floating.attachRequested("document")
        }

        MenuItem {
            objectName: "contactCardAttachmentAction"
            text: "Contact…"
            enabled: app.featureAvailable("contact-cards")
            Accessible.name: "Choose and review a local vCard contact"
            Accessible.description: enabled ? "Select one local .vcf file, review its fields, then choose Send" : bridge.capabilityReason("contact-cards")
            onTriggered: floating.attachRequested("contact-card")
        }

        MenuItem {
            objectName: "locationAttachmentAction"
            text: "Location…"
            enabled: app.featureAvailable("location-messages") &&
                     (controller.demo || controller.connectionState === "connected") &&
                     !controller.locationOutcomeUnknown
            Accessible.name: "Enter and review coordinates to share a static location"
            Accessible.description: controller.locationOutcomeUnknown ? "A previous send has an unknown outcome. Check the conversation; it will not be retried automatically." : enabled ? "Enter coordinates manually, review the location, then choose Send" : bridge.capabilityReason("location-messages")
            onTriggered: floating.attachRequested("location")
        }

    }

    ExpressionPicker {
        id: expressions

        theme: floating.theme
        controller: floating.controller
        x: 12
        y: -height - 4
        onEmojiSelected: function(emoji) {
            floating.insertEmoji(emoji);
        }
        onGifRequested: floating.attachRequested("gif")
        onStickerRequested: floating.attachRequested("sticker")
    }

}
