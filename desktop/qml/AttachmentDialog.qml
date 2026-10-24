import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import QtMultimedia

Dialog {
    id: dialog
    required property var theme
    required property var controller
    readonly property var meta: controller.attachment || ({})
    readonly property bool preparing: meta.state === "preparing"
    readonly property bool animated: meta.kind === "gif" || meta.mime === "image/gif"
    readonly property bool videoReview: meta.kind === "video" && meta.mime === "video/mp4"
    readonly property bool audioReview: meta.kind === "audio" && (meta.mode === "audio" || meta.mode === "voice-note")
    readonly property bool audioCaptionPresent: audioReview && controller.draft.length > 0
    readonly property bool stickerReview: meta.kind === "sticker" || meta.mode === "sticker"
    readonly property bool stickerCaptionPresent: stickerReview && controller.draft.length > 0
    readonly property var albumItems: validatedAlbumItems()
    readonly property bool albumReview: meta.kind === "album" && meta.provenance === "local_demo" &&
            meta.state === "ready" && albumItems.length >= 2 && albumItems.length <= 10
    readonly property string localVideoSource: videoReview && meta.state === "ready" && String(meta.url || "").indexOf("file:///") === 0 ? String(meta.url) : ""
    readonly property string localAudioSource: audioReview && meta.state === "ready" && String(meta.url || "").indexOf("file:///") === 0 ? String(meta.url) : ""
    readonly property bool isVideo: videoReview && localVideoSource !== ""
    readonly property string audioLabel: meta.is_voice_note ? "Voice note" : "Audio file"
    property bool videoAudioMuted: true
    property bool videoAudioOutputArmed: false
    property bool audioMuted: true
    property bool audioOutputArmed: false
    property bool localStickerPackSaved: false
    property bool localStickerPackAttempted: false
    MediaPlayer {
        id: videoReviewPlayer
        objectName: "videoReviewPlayer"
        source: dialog.visible && dialog.isVideo ? dialog.localVideoSource : ""
        audioOutput: videoReviewAudioOutputLoader.item
        videoOutput: videoReviewOutput
    }
    Loader {
        id: videoReviewAudioOutputLoader
        objectName: "videoReviewAudioOutputLoader"
        active: dialog.visible && dialog.videoReview && dialog.videoAudioOutputArmed
        sourceComponent: AudioOutput {
            objectName: "videoReviewAudioOutput"
            muted: dialog.videoAudioMuted
            volume: 1
        }
    }
    MediaPlayer {
        id: audioReviewPlayer
        objectName: "audioReviewPlayer"
        source: dialog.visible && dialog.audioReview ? dialog.localAudioSource : ""
        audioOutput: audioReviewAudioOutputLoader.item
    }
    Loader {
        id: audioReviewAudioOutputLoader
        objectName: "audioReviewAudioOutputLoader"
        active: dialog.visible && dialog.audioReview && dialog.audioOutputArmed
        sourceComponent: AudioOutput {
            objectName: "audioReviewAudioOutput"
            muted: dialog.audioMuted
            volume: 1
        }
    }
    signal sent()
    function cancel() { controller.clearAttachment(); close() }
    function sizeLabel(bytes) {
        if (!bytes || bytes < 0) return "—";
        return bytes >= 1048576 ? (bytes / 1048576).toFixed(1) + " MiB" : Math.ceil(bytes / 1024) + " KiB";
    }
    function validatedAlbumItems() {
        const values = meta.items;
        if (meta.kind !== "album" || meta.provenance !== "local_demo" ||
                !values || typeof values.length !== "number" || values.length < 2 || values.length > 10)
            return [];
        const items = [];
        let total = 0;
        for (let i = 0; i < values.length; ++i) {
            const item = values[i];
            const mime = String(item && item.mime || "").toLowerCase();
            const url = String(item && item.url || "");
            const size = Number(item && item.prepared_size);
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
        const reportedTotal = Number(meta.total_prepared_size);
        return Number.isSafeInteger(reportedTotal) && reportedTotal === total ? items : [];
    }
    function videoDescription() {
        let audio = meta.audio_codec === "aac" ? "AAC-LC audio" : "no audio track";
        let duration = Number(meta.duration || 0);
        return "Validated MP4 · H.264 AVC · yuv420p · " + audio + (duration > 0 ? " · " + duration + (duration === 1 ? " second" : " seconds") : " · duration unavailable");
    }
    function audioDescription() {
        let duration = Number(meta.duration || 0);
        return "Validated OGG/Opus · mono 48 kHz" + (duration > 0 ? " · " + duration + (duration === 1 ? " second" : " seconds") : " · duration unavailable");
    }
    objectName: "attachmentPreviewDialog"
    anchors.centerIn: parent
    modal: true
    focus: true
    padding: 0
    width: Math.min(760, parent ? parent.width - 32 : 760)
    height: Math.min(680, parent ? parent.height - 32 : 680)
    standardButtons: Dialog.NoButton
    closePolicy: Popup.CloseOnEscape
    onOpened: {
        videoAudioMuted = true;
        videoAudioOutputArmed = false;
        videoReviewPlayer.stop();
        audioMuted = true;
        audioOutputArmed = false;
        audioReviewPlayer.stop();
        localStickerPackSaved = false;
        localStickerPackAttempted = false;
        stickerPackName.text = "";
        cancelButton.forceActiveFocus();
    }
    onClosed: {
        videoReviewPlayer.stop();
        videoReviewPlayer.position = 0;
        videoAudioOutputArmed = false;
        videoAudioMuted = true;
        audioReviewPlayer.stop();
        audioReviewPlayer.position = 0;
        audioOutputArmed = false;
        audioMuted = true;
    }
    onRejected: controller.clearAttachment()
    background: Rectangle { color: dialog.theme.surface; radius: 16; border.color: dialog.theme.line }
    header: Item {
        implicitHeight: 68
        RowLayout {
            anchors.fill: parent; anchors.leftMargin: 24; anchors.rightMargin: 24; spacing: 16
            Label { objectName: "attachmentReviewTitle"; text: dialog.albumReview ? "Review photo album" : "Review attachment"; color: dialog.theme.ink; font.pixelSize: 21; font.weight: Font.DemiBold; Layout.fillWidth: true }
            BusyIndicator { running: dialog.preparing; visible: running; Layout.preferredWidth: 28; Layout.preferredHeight: 28 }
            Label { text: dialog.preparing ? "Preparing…" : dialog.meta.state === "error" ? "Needs attention" : "Ready"; color: dialog.meta.state === "error" ? dialog.theme.danger : dialog.theme.muted }
        }
    }
    contentItem: Flickable {
        id: scroll
        clip: true
        contentWidth: width
        contentHeight: previewContent.implicitHeight + 32
        ScrollBar.vertical: ScrollBar { }
        Item {
            id: previewContent
            x: 24; y: 4; width: parent.width - 48
            implicitHeight: body.implicitHeight
            height: implicitHeight
        }
        Column {
            id: body
            parent: previewContent
            x: 0; y: 0; width: parent.width; height: implicitHeight; spacing: 12
            Rectangle {
                width: parent.width
                height: implicitHeight
                implicitHeight: dialog.albumReview ? Math.min(320, Math.max(150, Math.ceil(dialog.albumItems.length / 2) * 118 + 8)) : dialog.stickerReview ? Math.min(180, Math.max(120, scroll.height * 0.30)) : Math.min(260, Math.max(150, scroll.height * 0.46))
                radius: 12
                color: dialog.theme.chrome
                Image {
                    objectName: dialog.stickerReview ? "stickerReviewImage" : "attachmentReviewImage"
                    anchors.fill: parent; anchors.margins: 16
                    source: !dialog.albumReview && dialog.meta.kind !== "album" && !dialog.animated && dialog.meta.kind !== "document" && !dialog.videoReview && !dialog.audioReview ? String(dialog.meta.url || "") : ""
                    visible: !dialog.albumReview && dialog.meta.kind !== "album" && !dialog.animated && dialog.meta.kind !== "document" && !dialog.videoReview && !dialog.audioReview
                    fillMode: Image.PreserveAspectFit
                    sourceSize.width: 960; sourceSize.height: 600
                    asynchronous: true
                }
                GridView {
                    id: albumReviewGrid
                    objectName: "albumReviewGrid"
                    anchors.fill: parent
                    anchors.margins: 12
                    visible: dialog.albumReview
                    clip: true
                    cellWidth: Math.floor(width / 2)
                    cellHeight: 118
                    model: dialog.albumReview ? dialog.albumItems : []
                    interactive: count > 4
                    Accessible.name: "Prepared photo album previews"
                    Accessible.description: dialog.albumItems.length + " still images; scroll to inspect the full album."
                    ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }
                    delegate: Rectangle {
                        required property int index
                        required property var modelData
                        objectName: "albumReviewTile" + index
                        width: albumReviewGrid.cellWidth - 8
                        height: albumReviewGrid.cellHeight - 8
                        radius: 8
                        color: dialog.theme.surface
                        border.color: dialog.theme.line
                        clip: true
                        Image {
                            objectName: "albumReviewImage" + index
                            anchors.fill: parent
                            anchors.margins: 5
                            source: String(modelData.url || "")
                            fillMode: Image.PreserveAspectFit
                            sourceSize.width: 480
                            sourceSize.height: 300
                            asynchronous: true
                            Accessible.name: "Album photo " + (index + 1)
                        }
                        Label {
                            anchors.left: parent.left
                            anchors.top: parent.top
                            anchors.margins: 6
                            textFormat: Text.PlainText
                            text: "Photo " + (index + 1)
                            color: dialog.theme.ink
                            font.pixelSize: 10
                            padding: 4
                            background: Rectangle { color: dialog.theme.surface; radius: 4; opacity: 0.88 }
                        }
                    }
                }
                Label {
                    objectName: "albumReviewPlaceholder"
                    anchors.centerIn: parent
                    visible: dialog.meta.kind === "album" && !dialog.albumReview && dialog.meta.state !== "error"
                    textFormat: Text.PlainText
                    text: dialog.preparing ? "Preparing local photo album…" : "Album preview is unavailable."
                    color: dialog.theme.muted
                    wrapMode: Text.WordWrap
                    horizontalAlignment: Text.AlignHCenter
                }
                AnimatedImage {
                    anchors.fill: parent; anchors.margins: 16
                    source: dialog.visible && dialog.animated ? String(dialog.meta.url || "") : ""
                    visible: dialog.animated
                    playing: dialog.visible && dialog.animated
                    fillMode: Image.PreserveAspectFit
                    cache: false
                }
                VideoOutput {
                    id: videoReviewOutput
                    objectName: "videoReviewOutput"
                    anchors.fill: parent; anchors.margins: 16
                    visible: dialog.isVideo
                    fillMode: VideoOutput.PreserveAspectFit
                }
                Label {
                    anchors.centerIn: parent
                    width: parent.width - 32
                    visible: dialog.videoReview && (dialog.preparing || dialog.meta.state === "error" || !dialog.isVideo || videoReviewPlayer.errorString !== "" || videoReviewPlayer.playbackState !== MediaPlayer.PlayingState)
                    textFormat: Text.PlainText
                    text: dialog.preparing ? "Preparing video…" : dialog.meta.state === "error" ? (dialog.meta.error || "Video could not be prepared.") : !dialog.isVideo ? "Validated local video preview is unavailable." : videoReviewPlayer.errorString !== "" ? videoReviewPlayer.errorString : "Preview paused · press Play to inspect. Audio is muted."
                    color: dialog.meta.state === "error" || videoReviewPlayer.errorString !== "" ? dialog.theme.danger : dialog.theme.muted
                    horizontalAlignment: Text.AlignHCenter
                    wrapMode: Text.WordWrap
                }
                ColumnLayout {
                    objectName: "audioAttachmentReviewCard"
                    anchors.centerIn: parent
                    width: parent.width - 40
                    visible: dialog.audioReview
                    spacing: 8
                    Label { objectName: "audioAttachmentReviewTitle"; textFormat: Text.PlainText; text: dialog.audioLabel; color: dialog.theme.ink; font.pixelSize: 18; font.weight: Font.DemiBold; Layout.fillWidth: true; horizontalAlignment: Text.AlignHCenter }
                    Label { objectName: "audioAttachmentReviewDetails"; textFormat: Text.PlainText; text: dialog.audioDescription(); color: dialog.theme.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; horizontalAlignment: Text.AlignHCenter }
                    Label { textFormat: Text.PlainText; text: audioReviewPlayer.errorString !== "" ? audioReviewPlayer.errorString : dialog.localAudioSource === "" ? "Local audio preview is unavailable." : "Preview paused · press Play to inspect. Audio is muted."; color: audioReviewPlayer.errorString !== "" ? dialog.theme.danger : dialog.theme.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; horizontalAlignment: Text.AlignHCenter }
                }
                Column {
                    objectName: "documentAttachmentCard"
                    anchors.centerIn: parent; spacing: 12
                    visible: dialog.meta.kind === "document"
                    Image { anchors.horizontalCenter: parent.horizontalCenter; source: "qrc:/icons/file.svg"; width: 44; height: 44 }
                    Label { objectName: "documentAttachmentLabel"; text: "Document · original bytes"; color: dialog.theme.muted; font.pixelSize: 12 }
                }
            }
            RowLayout {
                visible: dialog.videoReview
                width: parent.width
                height: implicitHeight
                spacing: 8
                ActionButton {
                    theme: dialog.theme
                    objectName: "videoReviewPlayPause"
                    text: videoReviewPlayer.playbackState === MediaPlayer.PlayingState ? "Pause" : "Play"
                    Accessible.name: videoReviewPlayer.playbackState === MediaPlayer.PlayingState ? "Pause video preview" : "Play video preview"
                    enabled: dialog.isVideo && videoReviewPlayer.errorString === ""
                    onClicked: videoReviewPlayer.playbackState === MediaPlayer.PlayingState ? videoReviewPlayer.pause() : videoReviewPlayer.play()
                }
                ActionButton {
                    theme: dialog.theme
                    objectName: "videoReviewMute"
                    text: dialog.videoAudioMuted ? "Unmute" : "Mute"
                    Accessible.name: dialog.videoAudioMuted ? "Unmute video audio" : "Mute video audio"
                    enabled: dialog.isVideo && videoReviewPlayer.errorString === ""
                    onClicked: {
                        if (dialog.videoAudioMuted) {
                            dialog.videoAudioOutputArmed = true;
                            dialog.videoAudioMuted = false;
                        } else {
                            dialog.videoAudioMuted = true;
                        }
                    }
                }
                Slider {
                    objectName: "videoReviewProgress"
                    visible: dialog.videoReview
                    enabled: dialog.isVideo && videoReviewPlayer.duration > 0 && videoReviewPlayer.errorString === ""
                    Layout.fillWidth: true
                    from: 0
                    to: Math.max(1, videoReviewPlayer.duration)
                    value: videoReviewPlayer.position
                    onMoved: videoReviewPlayer.position = value
                    Accessible.name: "Video review position"
                }
            }
            RowLayout {
                objectName: "audioReviewControls"
                visible: dialog.audioReview
                width: parent.width
                height: implicitHeight
                spacing: 8
                ActionButton {
                    theme: dialog.theme
                    objectName: "audioReviewPlayPause"
                    text: audioReviewPlayer.playbackState === MediaPlayer.PlayingState ? "Pause" : "Play"
                    Accessible.name: audioReviewPlayer.playbackState === MediaPlayer.PlayingState ? "Pause audio preview" : "Play audio preview"
                    enabled: dialog.localAudioSource !== "" && audioReviewPlayer.errorString === ""
                    onClicked: audioReviewPlayer.playbackState === MediaPlayer.PlayingState ? audioReviewPlayer.pause() : audioReviewPlayer.play()
                }
                ActionButton {
                    theme: dialog.theme
                    objectName: "audioReviewMute"
                    text: dialog.audioMuted ? "Unmute" : "Mute"
                    Accessible.name: dialog.audioMuted ? "Unmute audio preview" : "Mute audio preview"
                    enabled: dialog.localAudioSource !== "" && audioReviewPlayer.errorString === ""
                    onClicked: {
                        if (dialog.audioMuted) {
                            dialog.audioOutputArmed = true;
                            dialog.audioMuted = false;
                        } else {
                            dialog.audioMuted = true;
                        }
                    }
                }
                Slider {
                    objectName: "audioReviewProgress"
                    enabled: dialog.audioReview && audioReviewPlayer.duration > 0 && audioReviewPlayer.errorString === ""
                    Layout.fillWidth: true
                    from: 0
                    to: Math.max(1, audioReviewPlayer.duration)
                    value: audioReviewPlayer.position
                    onMoved: audioReviewPlayer.position = value
                    Accessible.name: "Audio review position"
                }
            }
            Label { textFormat: Text.PlainText; text: dialog.albumReview ? dialog.albumItems.length + " selected photos" : dialog.meta.display_name || dialog.meta.name || "Attachment"; color: dialog.theme.ink; font.weight: Font.DemiBold; elide: Text.ElideMiddle; width: parent.width; height: implicitHeight }
            RowLayout {
                width: parent.width; height: implicitHeight; spacing: 16
                Label { visible: !dialog.albumReview; text: "Original  " + dialog.sizeLabel(dialog.meta.original_size || dialog.meta.size); color: dialog.theme.muted; font.pixelSize: 12 }
                Label { text: (dialog.albumReview ? "Album total  " + dialog.sizeLabel(dialog.meta.total_prepared_size) : "To send  " + dialog.sizeLabel(dialog.meta.prepared_size || dialog.meta.size)); color: dialog.theme.ink; font.pixelSize: 12; font.weight: Font.DemiBold }
                Item { Layout.fillWidth: true }
                Label { visible: !dialog.albumReview; text: dialog.meta.width && dialog.meta.height ? dialog.meta.width + " × " + dialog.meta.height : ""; color: dialog.theme.muted; font.pixelSize: 12 }
            }
            Label {
                objectName: "attachmentPreparationDescription"
                text: dialog.albumReview ? "Local demo album · 2–10 still images · up to 20 MiB per prepared image and 50 MiB total" : dialog.videoReview ? dialog.videoDescription() : dialog.audioReview ? dialog.audioDescription() : dialog.stickerReview ? "Static transparent 512 × 512 WebP sticker · up to 100 KB · captions unavailable" : dialog.meta.compressed ? "Optimized photo · the original file is unchanged" : dialog.meta.mode === "document" ? "No recompression · the source file is unchanged" : dialog.animated ? "Animated GIF · sent as a silent video, up to 15 seconds" : "Original quality · the source file is unchanged"
                color: dialog.theme.muted; font.pixelSize: 12; wrapMode: Text.WordWrap; width: parent.width; height: implicitHeight
            }
            Label {
                objectName: "albumLocalOnlyDisclosure"
                visible: dialog.albumReview
                textFormat: Text.PlainText
                text: "Local demo only. Photos stay in this profile; Send album creates one synthetic message locally. Nothing is uploaded to WhatsApp."
                color: dialog.theme.muted
                wrapMode: Text.WordWrap
                width: parent.width
                height: implicitHeight
            }
            ColumnLayout {
                objectName: "localStickerPackSaveSection"
                visible: dialog.stickerReview && dialog.meta.state === "ready"
                width: parent.width
                height: implicitHeight
                spacing: 6
                Label { text: "Save a local copy to a sticker pack"; color: dialog.theme.ink; font.weight: Font.DemiBold; Layout.fillWidth: true }
                RowLayout {
                    Layout.fillWidth: true
                    TextField {
                        id: stickerPackName
                        objectName: "stickerPackName"
                        maximumLength: 32
                        placeholderText: "Pack name"
                        Accessible.name: "Local sticker pack name"
                        color: dialog.theme.ink
                        placeholderTextColor: dialog.theme.muted
                        Layout.fillWidth: true
                        background: Rectangle { color: dialog.theme.chrome; radius: 8; border.color: parent.activeFocus ? dialog.theme.accent : dialog.theme.line }
                    }
                    ActionButton {
                        objectName: "saveStickerToLocalPack"
                        theme: dialog.theme
                        text: "Save copy"
                        enabled: dialog.meta.state === "ready" && stickerPackName.text.trim().length > 0 && !controller.sending
                        Accessible.name: "Save a copy to a local sticker pack"
                        onClicked: {
                            dialog.localStickerPackAttempted = true;
                            dialog.localStickerPackSaved = controller.addCurrentStickerToPack(stickerPackName.text);
                            if (dialog.localStickerPackSaved)
                                stickerPackName.text = "";
                        }
                    }
                }
                Label {
                    objectName: "stickerPackLocalOnly"
                    textFormat: Text.PlainText
                    text: "Enter a new name to create a pack or reuse one to add a sticker. Copies stay in this profile; nothing is uploaded or sent."
                    color: dialog.theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                Label {
                    objectName: "stickerPackSaveResult"
                    visible: dialog.localStickerPackSaved || (dialog.localStickerPackAttempted && controller.expressionStoreError !== "")
                    textFormat: Text.PlainText
                    text: dialog.localStickerPackSaved ? "Saved to the local sticker pack." : controller.expressionStoreError
                    color: dialog.localStickerPackSaved ? dialog.theme.ink : dialog.theme.danger
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
            }
            RowLayout {
                objectName: "attachmentModeOptions"
                visible: !dialog.albumReview && !dialog.videoReview && !dialog.audioReview && !dialog.stickerReview
                width: parent.width
                height: implicitHeight
                spacing: 8
                ActionButton { theme: dialog.theme; objectName: "photoModeButton"; text: "Photo"; primary: dialog.meta.mode === "photo"; enabled: !dialog.preparing && !dialog.animated && !controller.sending; onClicked: controller.changeAttachmentMode("photo") }
                ActionButton { theme: dialog.theme; objectName: "originalModeButton"; text: "Original"; primary: dialog.meta.mode === "original"; enabled: !dialog.preparing && !controller.sending; onClicked: controller.changeAttachmentMode("original") }
                ActionButton { theme: dialog.theme; objectName: "documentModeButton"; text: "Document"; primary: dialog.meta.mode === "document"; enabled: !dialog.preparing && !controller.sending; onClicked: controller.changeAttachmentMode("document") }
            }
            Label { text: dialog.albumReview ? "Album caption" : dialog.stickerReview ? "Caption · unavailable for stickers" : dialog.audioReview ? "Caption · unavailable for audio" : "Caption"; color: dialog.theme.ink; font.weight: Font.DemiBold; width: parent.width; height: implicitHeight }
            TextArea {
                objectName: "attachmentCaption"
                placeholderText: dialog.albumReview ? "Add a caption for these photos…" : dialog.stickerReview ? "Stickers cannot include captions" : dialog.audioReview ? "Audio attachments cannot include a caption" : "Add a caption…"
                text: controller.draft
                textFormat: TextEdit.PlainText
                readOnly: dialog.stickerReview && !dialog.stickerCaptionPresent
                color: dialog.theme.ink
                placeholderTextColor: dialog.theme.muted
                width: parent.width
                height: 72
                wrapMode: TextEdit.Wrap
                selectByMouse: true
                padding: 12
                Accessible.name: dialog.albumReview ? "Album caption" : dialog.stickerReview ? (dialog.stickerCaptionPresent ? "Clear sticker caption" : "Sticker captions unavailable") : "Attachment caption"
                background: Rectangle { color: dialog.theme.chrome; radius: 10; border.color: parent.activeFocus ? dialog.theme.accent : dialog.theme.line }
                onTextChanged: if (text !== controller.draft) controller.setDraft(text)
            }
            Label {
                objectName: "stickerCaptionError"
                visible: dialog.stickerCaptionPresent
                textFormat: Text.PlainText
                text: "Stickers cannot include a caption. Clear this text to send the sticker."
                color: dialog.theme.danger
                wrapMode: Text.WordWrap
                width: parent.width
                height: implicitHeight
            }
            Label {
                objectName: "audioCaptionError"
                visible: dialog.audioCaptionPresent
                textFormat: Text.PlainText
                text: "Audio messages cannot include a caption. Clear this draft to continue."
                color: dialog.theme.danger
                wrapMode: Text.WordWrap
                width: parent.width
                height: implicitHeight
            }
            Label { visible: !!dialog.meta.attribution; textFormat: Text.PlainText; text: dialog.meta.attribution || ""; color: dialog.theme.muted; font.pixelSize: 11; wrapMode: Text.WordWrap; width: parent.width; height: implicitHeight }
            Label { visible: dialog.meta.state === "error"; textFormat: Text.PlainText; text: dialog.meta.error || "The attachment could not be prepared."; color: dialog.theme.danger; wrapMode: Text.WordWrap; width: parent.width; height: implicitHeight }
        }
    }
    footer: Item {
        implicitHeight: 76
        RowLayout {
            anchors.right: parent.right; anchors.rightMargin: 24
            anchors.top: parent.top; anchors.topMargin: 12
            anchors.bottom: parent.bottom; anchors.bottomMargin: 20
            spacing: 8
            ActionButton { id: cancelButton; theme: dialog.theme; objectName: "attachmentCancel"; text: "Cancel"; Accessible.name: "Cancel attachment"; enabled: !controller.sending; onClicked: dialog.cancel() }
            ActionButton {
                theme: dialog.theme; objectName: "attachmentSend"; text: dialog.albumReview ? "Send album" : "Send"; primary: true
                Accessible.name: dialog.albumReview ? "Send photo album" : "Send attachment"
                enabled: dialog.meta.state === "ready" && (dialog.meta.kind !== "album" || dialog.albumReview) && !controller.sending && !dialog.stickerCaptionPresent && !dialog.audioCaptionPresent
                onClicked: { dialog.sent(); controller.send(); if (controller.sending) dialog.close() }
            }
        }
    }
}
