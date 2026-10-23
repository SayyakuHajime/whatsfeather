import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

ColumnLayout {
    id: browser
    required property var theme
    required property var controller
    signal selected()
    spacing: 8
    RowLayout { Layout.fillWidth: true
        Label { text: "GIF provider"; color: theme.muted; Layout.fillWidth: true }
        Label { objectName: "gifProviderName"; text: "Wikimedia Commons"; Accessible.name: "GIF search provider: Wikimedia Commons"; color: theme.ink }
    }
    RowLayout { Layout.fillWidth: true
        TextField { id: query; objectName: "gifQuery"; placeholderText: "Search public GIFs"; Accessible.name: "Search Wikimedia Commons for GIFs"; color: theme.ink; Layout.fillWidth: true; maximumLength: 50; onAccepted: if (search.enabled) search.clicked() }
        Button { id: search; objectName: "gifSearch"; text: "Search"; Accessible.name: "Search GIFs"; enabled: query.text.trim().length > 0 && !controller.gifsLoading; onClicked: controller.searchGifs(query.text, "commons") }
    }
    Label { objectName: "gifPrivacyNotice"; textFormat: Text.PlainText; text: "Search terms and previews are fetched from Wikimedia Commons only after you press Search. Selecting a result downloads that GIF for review."; color: theme.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true }
    Label { visible: controller.gifError !== ""; textFormat: Text.PlainText; text: controller.gifError; color: theme.danger; wrapMode: Text.WordWrap; Layout.fillWidth: true }
    Label { visible: controller.gifsLoading; text: "Searching Wikimedia Commons…"; color: theme.muted }
    Label { visible: !controller.gifsLoading && controller.gifError === "" && controller.gifItems.length === 0; text: "Search Wikimedia Commons for a GIF. Results are selected, then staged for review."; color: theme.muted; wrapMode: Text.WordWrap; Layout.fillWidth: true }
    GridView { objectName: "gifResults"; Layout.fillWidth: true; Layout.fillHeight: true; clip: true; cellWidth: 104; cellHeight: 104; model: controller.gifItems
        delegate: Button { required property var modelData; width: 96; height: 96; objectName: "gifResult"; Accessible.name: (modelData.title || "GIF") + " from " + (modelData.attribution || "Wikimedia Commons"); onClicked: { controller.prepareGif(modelData.id, modelData.provider || "commons"); browser.selected() }
            contentItem: Column { spacing: 2; Image { width: 92; height: 70; source: modelData.preview_url || ""; asynchronous: true; fillMode: Image.PreserveAspectCrop; cache: false }
                Label { width: 92; textFormat: Text.PlainText; text: modelData.title || "GIF"; color: theme.ink; elide: Text.ElideRight; font.pixelSize: 10 }
            }
        }
    }
}
