import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

Popup {
    id: picker
    required property var theme
    required property var controller
    readonly property var stickerFeature: {
        const snapshot = controller.capabilities || ({ });
        const features = snapshot.features || ({ });
        return features.stickers || ({ });
    }
    readonly property bool stickerAvailable: controller.capabilitiesReady && stickerFeature.available === true
    readonly property string stickerReason: controller.capabilitiesReady ? (stickerFeature.reason || "This backend does not advertise static stickers.") : "Checking backend support."

    signal emojiSelected(string emoji)
    signal gifRequested()
    signal stickerRequested()

    property string emojiQuery: ""
    property string emojiCategory: "All"
    property string selectedEmoji: ""
    readonly property var recentEmojis: controller.recentEmojis || []
    readonly property var favoriteEmojis: controller.favoriteEmojis || []
    readonly property var localStickerPacks: controller.localStickerPacks || []
    readonly property var selectedLocalStickerPack: localStickerPackSelector.currentIndex >= 0 && localStickerPackSelector.currentIndex < localStickerPacks.length ? localStickerPacks[localStickerPackSelector.currentIndex] : null
    readonly property var emojiCatalog: [
        {value:"😀", category:"Faces", words:"grinning smile face"}, {value:"😃", category:"Faces", words:"smile eyes face"}, {value:"😄", category:"Faces", words:"laugh smile face"}, {value:"😁", category:"Faces", words:"grin teeth face"}, {value:"😅", category:"Faces", words:"sweat laugh face"}, {value:"😂", category:"Faces", words:"tears laugh joy face"}, {value:"🙂", category:"Faces", words:"slight smile face"}, {value:"🙃", category:"Faces", words:"upside down face"}, {value:"😉", category:"Faces", words:"wink face"}, {value:"😊", category:"Faces", words:"blush smile face"}, {value:"😍", category:"Faces", words:"heart eyes love face"}, {value:"🥰", category:"Faces", words:"hearts love face"}, {value:"😎", category:"Faces", words:"cool sunglasses face"}, {value:"🤔", category:"Faces", words:"think question face"}, {value:"🫡", category:"Faces", words:"salute face"}, {value:"🤗", category:"Faces", words:"hug face"}, {value:"😴", category:"Faces", words:"sleep face"}, {value:"🥳", category:"Faces", words:"party face"}, {value:"😭", category:"Faces", words:"cry sad face"}, {value:"😮", category:"Faces", words:"surprised face"},
        {value:"👍", category:"People", words:"thumbs up approve"}, {value:"👎", category:"People", words:"thumbs down"}, {value:"👏", category:"People", words:"clap hands"}, {value:"🙌", category:"People", words:"raised hands"}, {value:"🤝", category:"People", words:"handshake"}, {value:"🙏", category:"People", words:"thanks please pray"}, {value:"💪", category:"People", words:"strong arm"}, {value:"👋", category:"People", words:"wave hello hand"},
        {value:"🐈", category:"Animals", words:"cat pet"}, {value:"🐕", category:"Animals", words:"dog pet"}, {value:"🐢", category:"Animals", words:"turtle"}, {value:"🦋", category:"Animals", words:"butterfly"}, {value:"🌿", category:"Nature", words:"leaf plant"}, {value:"🌻", category:"Nature", words:"sunflower flower"}, {value:"🌧️", category:"Nature", words:"rain cloud weather"}, {value:"☀️", category:"Nature", words:"sun weather"},
        {value:"❤️", category:"Symbols", words:"heart love red"}, {value:"🧡", category:"Symbols", words:"orange heart"}, {value:"💛", category:"Symbols", words:"yellow heart"}, {value:"💚", category:"Symbols", words:"green heart"}, {value:"💙", category:"Symbols", words:"blue heart"}, {value:"💜", category:"Symbols", words:"purple heart"}, {value:"✨", category:"Symbols", words:"sparkles star"}, {value:"✅", category:"Symbols", words:"check mark"},
        {value:"☕", category:"Food", words:"coffee drink"}, {value:"🍕", category:"Food", words:"pizza food"}, {value:"🎂", category:"Food", words:"cake birthday"}, {value:"🍓", category:"Food", words:"strawberry fruit"}, {value:"🎉", category:"Activities", words:"celebrate party"}, {value:"🔥", category:"Activities", words:"fire hot"}, {value:"🎮", category:"Activities", words:"game controller"}, {value:"🎵", category:"Activities", words:"music note"}, {value:"📚", category:"Objects", words:"books study"}, {value:"💻", category:"Objects", words:"computer laptop"}, {value:"💡", category:"Objects", words:"idea light bulb"}, {value:"🚀", category:"Travel", words:"rocket space"}
    ]

    function orderedEmojiEntries(values) {
        let ordered = []
        for (let index = 0; index < values.length; index++) {
            for (let catalogIndex = 0; catalogIndex < emojiCatalog.length; catalogIndex++) {
                if (emojiCatalog[catalogIndex].value === values[index]) {
                    ordered.push(emojiCatalog[catalogIndex])
                    break
                }
            }
        }
        return ordered
    }

    function filteredEmoji() {
        let category = picker.emojiCategory;
        let query = emojiQuery.trim().toLowerCase();
        let values = emojiCatalog;
        if (category === "Recent")
            values = orderedEmojiEntries(recentEmojis);
        else if (category === "Favorites")
            values = orderedEmojiEntries(favoriteEmojis);
        else if (category !== "All")
            values = emojiCatalog.filter(function(item) { return item.category === category; });
        if (query.length)
            values = values.filter(function(item) { return item.value.indexOf(query) >= 0 || item.words.indexOf(query) >= 0; });
        return values;
    }

    function chooseEmoji(value) {
        selectedEmoji = value;
        controller.recordRecentEmoji(value);
        emojiSelected(value);
    }

    function toggleFavorite() {
        if (!selectedEmoji)
            return;
        controller.toggleFavoriteEmoji(selectedEmoji);
    }

    objectName: "expressionPicker"
    width: Math.min(390, parent.width - 24)
    height: 380
    padding: 14
    focus: true
    modal: false
    closePolicy: Popup.CloseOnEscape | Popup.CloseOnPressOutside
    background: Rectangle { color: picker.theme.chrome; radius: 16; border.color: picker.theme.line }

    contentItem: ColumnLayout {
        spacing: 10
        TabBar {
            id: tabs
            objectName: "expressionTabs"
            Layout.fillWidth: true
            TabButton { text: "Emoji"; objectName: "emojiTab" }
            TabButton { text: "GIF"; objectName: "gifTab" }
            TabButton { text: "Stickers"; objectName: "stickerTab" }
        }
        StackLayout {
            Layout.fillWidth: true
            Layout.fillHeight: true
            currentIndex: tabs.currentIndex
            ColumnLayout {
                spacing: 8
                RowLayout {
                    Layout.fillWidth: true
                    TextField {
                        id: emojiSearch
                        objectName: "emojiSearch"
                        placeholderText: "Search emoji"
                        Accessible.name: "Search emoji by name or character"
                        text: picker.emojiQuery
                        onTextChanged: picker.emojiQuery = text
                        color: picker.theme.ink
                        placeholderTextColor: picker.theme.muted
                        Layout.fillWidth: true
                        background: Rectangle { color: picker.theme.surface; radius: 8; border.color: parent.activeFocus ? picker.theme.accent : picker.theme.line }
                    }
                    ComboBox {
                        id: emojiCategory
                        objectName: "emojiCategory"
                        model: ["All", "Recent", "Favorites", "Faces", "People", "Animals", "Nature", "Food", "Activities", "Travel", "Objects", "Symbols"]
                        currentIndex: 0
                        Accessible.name: "Emoji category"
                        Layout.preferredWidth: 128
                        onCurrentTextChanged: picker.emojiCategory = currentText
                    }
                }
                GridView {
                    id: grid
                    objectName: "emojiGrid"
                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    clip: true
                    cellWidth: 44
                    cellHeight: 44
                    model: picker.filteredEmoji()
                    delegate: Button {
                        required property var modelData
                        required property int index
                        width: 42
                        height: 42
                        text: modelData.value
                        font.pixelSize: 24
                        Accessible.name: "Insert " + modelData.words + " emoji"
                        objectName: index === 0 ? "firstEmoji" : "emojiOption"
                        onClicked: picker.chooseEmoji(modelData.value)
                        background: Rectangle { radius: 8; color: parent.hovered ? picker.theme.active : "transparent" }
                    }
                }
                RowLayout {
                    Layout.fillWidth: true
                    Label { objectName: "expressionStoreError"; textFormat: Text.PlainText; text: controller.expressionStoreError || (picker.selectedEmoji ? "Selected " + picker.selectedEmoji : (picker.emojiCategory === "Recent" ? "Recently used emoji appear here." : picker.emojiCategory === "Favorites" ? "Favorite emoji appear here." : "Emoji catalogue")); color: controller.expressionStoreError ? picker.theme.danger : picker.theme.muted; Layout.fillWidth: true; font.pixelSize: 11; elide: Text.ElideRight }
                    ActionButton {
                        objectName: "emojiFavoriteToggle"
                        theme: picker.theme
                        text: picker.selectedEmoji && picker.favoriteEmojis.indexOf(picker.selectedEmoji) >= 0 ? "Remove favorite" : "Add favorite"
                        enabled: !!picker.selectedEmoji
                        implicitHeight: 34
                        onClicked: picker.toggleFavorite()
                    }
                }
            }
            ColumnLayout {
                spacing: 8
                GifBrowser { theme: picker.theme; controller: picker.controller; Layout.fillWidth: true; Layout.fillHeight: true; onSelected: picker.close() }
                Button { objectName: "browseGifButton"; text: "From computer…"; Accessible.name: "Choose a GIF from computer"; Layout.alignment: Qt.AlignHCenter; onClicked: { picker.close(); picker.gifRequested(); } }
            }
            ColumnLayout {
                spacing: 8
                Label {
                    text: "Static WebP stickers"
                    color: picker.theme.ink
                    font.weight: Font.DemiBold
                    Layout.fillWidth: true
                }
                Label {
                    objectName: "stickerCapabilityNotice"
                    textFormat: Text.PlainText
                    text: picker.stickerReason
                    color: picker.theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                Label {
                    objectName: "localStickerPackDisclosure"
                    textFormat: Text.PlainText
                    text: "Local packs stay in this profile. Nothing is fetched from an account or network; choosing a sticker opens review and does not send it."
                    color: picker.theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                Rectangle {
                    Layout.fillWidth: true
                    implicitHeight: 1
                    color: picker.theme.line
                }
                ComboBox {
                    id: localStickerPackSelector
                    objectName: "localStickerPackSelector"
                    model: picker.localStickerPacks
                    textRole: "name"
                    valueRole: "id"
                    visible: picker.localStickerPacks.length > 0
                    enabled: picker.stickerAvailable
                    Layout.fillWidth: true
                    Accessible.name: "Local sticker pack"
                }
                Label {
                    objectName: "localStickerEmptyState"
                    visible: picker.localStickerPacks.length === 0 || !picker.selectedLocalStickerPack || picker.selectedLocalStickerPack.count === 0
                    textFormat: Text.PlainText
                    text: picker.localStickerPacks.length === 0 ? "No local packs yet. Choose a static WebP sticker, review it, then save a copy here." : "This pack has no stickers yet. Save a reviewed static WebP sticker to add one."
                    color: picker.theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                GridView {
                    id: localStickerGrid
                    objectName: "localStickerGrid"
                    Layout.fillWidth: true
                    Layout.fillHeight: true
                    Layout.minimumHeight: 70
                    Layout.preferredHeight: 76
                    visible: picker.selectedLocalStickerPack && picker.selectedLocalStickerPack.stickers.length > 0
                    clip: true
                    cellWidth: 70
                    cellHeight: 70
                    model: picker.selectedLocalStickerPack ? picker.selectedLocalStickerPack.stickers : []
                    delegate: Button {
                        required property var modelData
                        required property int index
                        width: 62
                        height: 62
                        padding: 4
                        objectName: index === 0 ? "firstLocalPackSticker" : "localPackSticker"
                        Accessible.name: "Review local sticker " + modelData.name + " from " + picker.selectedLocalStickerPack.name
                        enabled: picker.stickerAvailable
                        contentItem: Image {
                            source: modelData.url
                            sourceSize.width: 96
                            sourceSize.height: 96
                            fillMode: Image.PreserveAspectFit
                            asynchronous: true
                            cache: false
                        }
                        background: Rectangle {
                            radius: 8
                            color: parent.hovered ? picker.theme.active : picker.theme.surface
                            border.color: picker.theme.line
                        }
                        onClicked: {
                            if (picker.selectedLocalStickerPack && picker.controller.stageLocalSticker(picker.selectedLocalStickerPack.id, modelData.id))
                                picker.close();
                        }
                    }
                }
                Label {
                    objectName: "stickerStoreError"
                    visible: controller.expressionStoreError !== ""
                    textFormat: Text.PlainText
                    text: controller.expressionStoreError
                    color: picker.theme.danger
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
                RowLayout {
                    Layout.fillWidth: true
                    Label {
                        text: picker.stickerAvailable ? "Choose a local sticker" : "Sticker sending unavailable"
                        color: picker.theme.ink
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    ActionButton {
                        objectName: "chooseStickerButton"
                        theme: picker.theme
                        text: "Choose sticker…"
                        primary: true
                        enabled: picker.stickerAvailable
                        Accessible.name: "Choose a static WebP sticker"
                        Accessible.description: picker.stickerReason
                        onClicked: {
                            picker.close();
                            picker.stickerRequested();
                        }
                    }
                }
            }
        }
    }
}
