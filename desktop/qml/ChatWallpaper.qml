import QtQuick

Item {
    id: wallpaper
    objectName: "chatWallpaper"
    required property bool dark
    readonly property int loadStatus: texture.status
    readonly property bool aspectPreserving: texture.fillMode === Image.PreserveAspectCrop
    readonly property url wallpaperSource: texture.source
    clip: true

    Image {
        id: texture
        objectName: "chatWallpaperImage"
        anchors.fill: parent
        source: wallpaper.dark ? "qrc:/backgrounds/chat-dark.webp" : "qrc:/backgrounds/chat-green.webp"
        fillMode: Image.PreserveAspectCrop
        horizontalAlignment: Image.AlignHCenter
        verticalAlignment: Image.AlignVCenter
        asynchronous: true
        smooth: true
        mipmap: true
        cache: true
    }
}
