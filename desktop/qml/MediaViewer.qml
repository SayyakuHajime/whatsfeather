import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import QtMultimedia

Dialog {
    id: viewer
    required property var theme
    property var record: ({})
    property real zoom: 1
    readonly property var attachment: record.attachment || ({})
    readonly property bool isAudio: attachment.kind === "audio"
    readonly property string audioLabel: attachment.is_voice_note ? "Voice note" : "Audio"
    readonly property bool isVideo: attachment.kind === "video"
    readonly property bool playable: isAudio || isVideo
    property bool audioMuted: true
    property bool audioOutputArmed: false
    readonly property string localSource: String(attachment.url || "").indexOf("file:///") === 0 && attachment.state === "ready" ? String(attachment.url) : ""
    readonly property bool animated: attachment.kind === "gif" || attachment.mime === "image/gif" || !!attachment.animated
    readonly property real naturalWidth: (animated ? motion.implicitWidth : still.implicitWidth) || attachment.width || 480
    readonly property real naturalHeight: (animated ? motion.implicitHeight : still.implicitHeight) || attachment.height || 320
    readonly property real fitScale: Math.min(Math.max(1, viewport.width - 32) / Math.max(1, naturalWidth), Math.max(1, viewport.height - 32) / Math.max(1, naturalHeight))
    readonly property int loadStatus: animated ? motion.status : still.status
    MediaPlayer {
        id: player
        objectName: "viewerPlayer"
        source: viewer.visible && viewer.playable ? viewer.localSource : ""
        audioOutput: audioOutputLoader.item
        videoOutput: viewerVideoSurface
    }
    Loader {
        id: audioOutputLoader
        objectName: "viewerAudioOutputLoader"
        active: viewer.audioOutputArmed
        sourceComponent: AudioOutput {
            objectName: "viewerAudioOutput"
            muted: viewer.audioMuted
            volume: 1
        }
    }
    objectName: "mediaViewer"
    anchors.centerIn: parent
    modal: true
    focus: true
    width: Math.min(960, parent ? parent.width - 32 : 960)
    height: Math.min(700, parent ? parent.height - 32 : 700)
    padding: 0
    standardButtons: Dialog.NoButton
    closePolicy: Popup.CloseOnEscape | Popup.CloseOnPressOutside
    onOpened: { zoom = 1; audioMuted = true; audioOutputArmed = false; player.stop(); closeButton.forceActiveFocus() }
    onClosed: { zoom = 1; audioMuted = true; audioOutputArmed = false; player.stop(); record = ({}) }
    onZoomChanged: Qt.callLater(function() {
        viewport.contentX = Math.max(0, (viewport.contentWidth - viewport.width) / 2);
        viewport.contentY = Math.max(0, (viewport.contentHeight - viewport.height) / 2);
    })
    background: Rectangle { color: viewer.theme.surface; radius: 16; border.color: viewer.theme.line }
    header: Item {
        implicitHeight: 68
        RowLayout {
            anchors.fill: parent; anchors.leftMargin: 24; anchors.rightMargin: 16; spacing: 16
            Label {
                textFormat: Text.PlainText
                text: viewer.isAudio ? viewer.audioLabel : viewer.attachment.name || (viewer.animated ? "GIF" : "Photo")
                color: viewer.theme.ink; font.pixelSize: 16; font.weight: Font.DemiBold
                Layout.fillWidth: true; elide: Text.ElideMiddle
            }
            ActionButton { id: closeButton; theme: viewer.theme; objectName: "mediaViewerClose"; text: "Close"; Accessible.name: "Close media viewer"; onClicked: viewer.close() }
        }
    }
    contentItem: Rectangle {
        color: viewer.theme.canvas
        Flickable {
            id: viewport
            objectName: "mediaViewport"
            anchors.fill: parent
            clip: true
            contentWidth: Math.max(width, viewer.naturalWidth * viewer.fitScale * viewer.zoom)
            contentHeight: Math.max(height, viewer.naturalHeight * viewer.fitScale * viewer.zoom)
            boundsBehavior: Flickable.StopAtBounds
            Image {
                id: still
                objectName: "viewerStillImage"
                x: (viewport.contentWidth - width) / 2
                y: (viewport.contentHeight - height) / 2
                width: viewer.naturalWidth * viewer.fitScale * viewer.zoom
                height: viewer.naturalHeight * viewer.fitScale * viewer.zoom
                source: viewer.visible && !viewer.animated && !viewer.isVideo && !viewer.isAudio ? viewer.localSource : ""
                sourceSize.width: 2048; sourceSize.height: 2048
                visible: !viewer.animated && !viewer.isVideo && !viewer.isAudio
                fillMode: Image.PreserveAspectFit
                asynchronous: true
                cache: false
            }
            AnimatedImage {
                id: motion
                objectName: "viewerAnimatedImage"
                x: (viewport.contentWidth - width) / 2
                y: (viewport.contentHeight - height) / 2
                width: viewer.naturalWidth * viewer.fitScale * viewer.zoom
                height: viewer.naturalHeight * viewer.fitScale * viewer.zoom
                source: viewer.visible && viewer.animated ? viewer.localSource : ""
                visible: viewer.animated
                playing: viewer.visible && viewer.animated
                fillMode: Image.PreserveAspectFit
                cache: false
            }
            ScrollBar.horizontal: ScrollBar { policy: viewer.zoom > 1 ? ScrollBar.AsNeeded : ScrollBar.AlwaysOff }
            ScrollBar.vertical: ScrollBar { policy: viewer.zoom > 1 ? ScrollBar.AsNeeded : ScrollBar.AlwaysOff }
        }
        Label {
            anchors.centerIn: parent
            visible: !viewer.playable && (viewer.localSource === "" || viewer.loadStatus !== Image.Ready)
            text: viewer.localSource === "" ? "Media is not available locally." : viewer.loadStatus === Image.Error ? "Unable to open this media." : "Loading media…"
            textFormat: Text.PlainText
            color: viewer.theme.muted
        }
        VideoOutput {
            id: viewerVideoSurface
            objectName: "viewerVideoOutput"
            anchors.fill: parent
            visible: viewer.isVideo
            fillMode: VideoOutput.PreserveAspectFit
        }
        ColumnLayout {
            objectName: "audioReviewPanel"
            anchors.centerIn: parent
            visible: viewer.isAudio
            width: Math.min(parent.width - 48, 420)
            Label {
                objectName: "audioReviewTitle"
                text: viewer.audioLabel + " · local file"
                color: viewer.theme.ink
                font.pixelSize: 18
                Layout.alignment: Qt.AlignHCenter
            }
            Label {
                text: viewer.attachment.name || (viewer.attachment.is_voice_note ? "Voice message" : "Audio message")
                textFormat: Text.PlainText
                color: viewer.theme.muted
                elide: Text.ElideMiddle
                Layout.fillWidth: true
                horizontalAlignment: Text.AlignHCenter
            }
            Label { text: player.errorString || "Playback starts only when you press Play."; textFormat: Text.PlainText; color: viewer.theme.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true; horizontalAlignment: Text.AlignHCenter }
        }
    }
    footer: Item {
        implicitHeight: viewer.playable ? 82 : 72
        RowLayout {
            anchors.centerIn: parent; spacing: 8
            ActionButton {
                theme: viewer.theme
                objectName: "viewerPlayPause"
                visible: viewer.playable
                text: player.playbackState === MediaPlayer.PlayingState ? "Pause" : "Play"
                Accessible.name: player.playbackState === MediaPlayer.PlayingState ? "Pause media" : "Play media"
                onClicked: player.playbackState === MediaPlayer.PlayingState ? player.pause() : player.play()
            }
            ActionButton {
                theme: viewer.theme
                objectName: "viewerAudioMute"
                visible: viewer.playable
                text: viewer.audioMuted ? "Unmute" : "Mute"
                Accessible.name: viewer.audioMuted ? "Unmute media audio" : "Mute media audio"
                onClicked: {
                    if (viewer.audioMuted) {
                        viewer.audioOutputArmed = true;
                        viewer.audioMuted = false;
                    } else {
                        viewer.audioMuted = true;
                    }
                }
            }
            Slider {
                objectName: "viewerProgress"
                visible: viewer.playable
                enabled: player.duration > 0
                Layout.preferredWidth: Math.min(220, viewer.width * 0.28)
                from: 0
                to: Math.max(1, player.duration)
                value: player.position
                onMoved: player.position = value
                Accessible.name: "Media playback position"
            }
            ActionButton { theme: viewer.theme; objectName: "mediaZoomOut"; text: "Zoom out"; visible: !viewer.playable; enabled: viewer.zoom > 0.5; onClicked: viewer.zoom = Math.max(0.5, viewer.zoom - 0.25) }
            ActionButton { theme: viewer.theme; objectName: "mediaFit"; text: "Fit"; visible: !viewer.playable; onClicked: viewer.zoom = 1 }
            ActionButton { theme: viewer.theme; objectName: "mediaZoomIn"; text: "Zoom in"; visible: !viewer.playable; enabled: viewer.zoom < 4; onClicked: viewer.zoom = Math.min(4, viewer.zoom + 0.25) }
            Label { visible: !viewer.playable; text: Math.round(viewer.zoom * 100) + "%"; color: viewer.theme.muted; Layout.preferredWidth: 48; horizontalAlignment: Text.AlignRight }
        }
    }
}
