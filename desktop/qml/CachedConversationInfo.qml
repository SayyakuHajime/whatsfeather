import QtQuick
import QtQuick.Controls
import QtQuick.Layouts

ColumnLayout {
    id: root
    required property var theme
    required property var controller
    property var record: ({})
    property string actionError: ""
    property string localActionError: ""
    readonly property var contactDetails: root.record.contact_details || ({})
    readonly property var groupDetails: root.record.group_details || ({})
    readonly property var groupParticipants: root.groupDetails.participants || []
    readonly property bool managedGroup: !!root.record.is_group && root.groupDetails.managed === true && root.record.details_provenance === "demo_synthetic" && root.controller.demo && root.controller.hasCapability("group.update")
    property double presenceClockMs: Date.now()
    function isSyntheticPresence(presence) {
        return root.record.details_provenance === "demo_synthetic" && !!presence && presence.provenance === "demo_synthetic"
    }
    function hasExpiringPresence(presence) {
        if (!root.isSyntheticPresence(presence))
            return false
        const state = String(presence.state || "")
        const expires = Number(presence.expires_at_ms)
        return (state === "online" || state === "typing") && isFinite(expires) && Math.floor(expires) === expires && expires > root.presenceClockMs && expires - root.presenceClockMs <= 60000
    }
    function syntheticPresenceText(presence) {
        if (!root.isSyntheticPresence(presence))
            return ""
        const state = String(presence.state || "")
        if (state === "online" || state === "typing") {
            const expires = Number(presence.expires_at_ms)
            if (!isFinite(expires) || Math.floor(expires) !== expires || expires - root.presenceClockMs > 60000)
                return "Synthetic presence · invalid snapshot"
            if (expires <= root.presenceClockMs)
                return "Synthetic presence · snapshot expired"
            return state === "typing" ? "Synthetic presence · Typing…" : "Synthetic presence · Online"
        }
        if (state === "offline") {
            const lastSeen = Number(presence.last_seen_at_ms)
            if (!isFinite(lastSeen) || Math.floor(lastSeen) !== lastSeen || lastSeen <= 0 || lastSeen > root.presenceClockMs || root.presenceClockMs - lastSeen > 30 * 24 * 60 * 60 * 1000)
                return "Synthetic presence · Offline; last seen unavailable"
            const minutes = Math.floor((root.presenceClockMs - lastSeen) / 60000)
            if (minutes < 1)
                return "Synthetic presence · Last seen just now"
            if (minutes < 60)
                return "Synthetic presence · Last seen " + minutes + " min ago"
            const hours = Math.floor(minutes / 60)
            if (hours < 24)
                return "Synthetic presence · Last seen " + hours + " hr ago"
            return "Synthetic presence · Last seen " + Math.floor(hours / 24) + " days ago"
        }
        return "Synthetic presence · Unavailable"
    }
    readonly property bool presenceClockNeeded: {
        if (root.hasExpiringPresence(root.contactDetails.presence))
            return true
        for (let i = 0; i < root.groupParticipants.length; ++i) {
            if (root.hasExpiringPresence(root.groupParticipants[i].presence))
                return true
        }
        return false
    }
    function availableSyntheticParticipants() {
        const known = [
            {"id": "demo:participant:alex", "name": "Alex Morgan"},
            {"id": "demo:participant:maya", "name": "Maya Chen"},
            {"id": "demo:participant:sam", "name": "Sam Rivera"}
        ];
        const present = root.groupParticipants.map(function(member) { return String(member.id || ""); });
        return known.filter(function(member) { return present.indexOf(member.id) < 0; });
    }
    function saveManagedGroup() {
        const params = {"group_id": String(root.record.id || "")};
        const title = managedGroupTitleInput.text.trim();
        const description = managedGroupDescriptionInput.text;
        if (!title) {
            root.localActionError = "Group name cannot be empty.";
            return;
        }
        const titleChanged = title !== String(root.record.title || "");
        const descriptionChanged = description !== String(root.groupDetails.description || "");
        if (!titleChanged && !descriptionChanged) {
            root.localActionError = "No group details have changed.";
            return;
        }
        if (titleChanged)
            params.title = title;
        if (descriptionChanged)
            params.description = description;
        root.localActionError = "";
        if (!root.controller.performCapabilityAction("group.update", params))
            root.localActionError = "Group editing is unavailable in this synthetic demo.";
    }
    function changeParticipant(action, participantId) {
        root.localActionError = "";
        if (!root.controller.performCapabilityAction("group.participants", {"group_id": String(root.record.id || ""), "action": action, "participant_id": participantId}))
            root.localActionError = "Participant management is unavailable in this synthetic demo.";
    }
    function changeMembership(method) {
        root.localActionError = "";
        if (!root.controller.performCapabilityAction(method, {"group_id": String(root.record.id || "")}))
            root.localActionError = "Membership changes are unavailable in this synthetic demo.";
    }
    onRecordChanged: Qt.callLater(function() {
        managedGroupTitleInput.text = String(root.record.title || "");
        managedGroupDescriptionInput.text = String(root.groupDetails.description || "");
        root.localActionError = "";
        root.presenceClockMs = Date.now();
    })
    Timer {
        interval: 1000
        repeat: true
        running: root.presenceClockNeeded
        onTriggered: root.presenceClockMs = Date.now()
    }
    spacing: 12

    Label {
        objectName: "cachedConversationTitle"
        text: String(root.record.title || "")
        textFormat: Text.PlainText
        color: root.theme.ink
        font.pixelSize: 20
        font.weight: Font.DemiBold
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedConversationScope"
        text: root.record.details_provenance === "demo_synthetic" ? "Synthetic local demo · no live profile data fetched" : "Cached on this device · not a full WhatsApp profile"
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        text: root.record.is_group ? "Group conversation" : "Direct conversation"
        textFormat: Text.PlainText
        color: root.theme.ink
        visible: !!root.record.id
        Layout.fillWidth: true
    }
    TextArea {
        objectName: "cachedConversationIdentity"
        text: String(root.record.id || "")
        textFormat: TextEdit.PlainText
        readOnly: true
        selectByMouse: true
        wrapMode: TextEdit.WrapAnywhere
        color: root.theme.muted
        padding: 0
        background: null
        Layout.fillWidth: true
        Accessible.name: "Cached conversation identity"
    }
    Label {
        objectName: "cachedDetailsProvenance"
        visible: root.record.details_provenance === "demo_synthetic"
        text: "Synthetic demo fixture · local-only"
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedContactPhone"
        visible: !!root.contactDetails.phone
        text: "Phone · " + String(root.contactDetails.phone || "")
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WrapAnywhere
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedContactAbout"
        visible: !!root.contactDetails.about
        text: "About · " + String(root.contactDetails.about || "")
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedContactPresence"
        visible: !!root.record.id && !root.record.is_group && (root.record.details_provenance !== "demo_synthetic" || root.syntheticPresenceText(root.contactDetails.presence) !== "")
        text: root.record.details_provenance === "demo_synthetic" ? root.syntheticPresenceText(root.contactDetails.presence) : "Presence and typing · not fetched in this view"
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
        Accessible.name: "Contact presence and typing availability"
    }
    Label {
        objectName: "cachedGroupDescription"
        visible: !!root.groupDetails.description && !root.managedGroup
        text: "Description · " + String(root.groupDetails.description || "")
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedGroupParticipantCount"
        visible: !!root.record.is_group && root.groupDetails.participant_count !== undefined
        text: "Participants · " + Number(root.groupDetails.participant_count || 0)
        textFormat: Text.PlainText
        color: root.theme.ink
        Layout.fillWidth: true
    }
    Label {
        objectName: "cachedGroupParticipants"
        visible: root.groupParticipants.length > 0 && !root.managedGroup
        text: root.groupParticipants.map(function(member) {
            const identity = String(member.display_name || "Unknown participant") + " · " + String(member.role || "Member")
            const presence = root.syntheticPresenceText(member.presence)
            return identity + (presence ? "\n" + presence : "")
        }).join("\n")
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Rectangle {
        objectName: "managedSyntheticGroupControls"
        visible: root.managedGroup
        Layout.fillWidth: true
        implicitHeight: managedSyntheticGroupColumn.implicitHeight + 24
        radius: 10
        color: root.theme.surface
        border.color: root.theme.line
        ColumnLayout {
            id: managedSyntheticGroupColumn
            anchors.fill: parent
            anchors.margins: 12
            spacing: 10
            Label {
                text: "Manage synthetic demo group"
                textFormat: Text.PlainText
                color: root.theme.ink
                font.weight: Font.DemiBold
                Layout.fillWidth: true
            }
            Label {
                text: "Local-only fixture · changes stay in this demo profile and are never sent to WhatsApp."
                textFormat: Text.PlainText
                color: root.theme.muted
                font.pixelSize: 12
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }
            TextField {
                id: managedGroupTitleInput
                objectName: "managedGroupTitleInput"
                visible: root.groupDetails.can_manage === true
                Layout.fillWidth: true
                maximumLength: 128
                placeholderText: "Group name"
                color: root.theme.ink
                selectByMouse: true
                Accessible.name: "Synthetic group name"
                background: Rectangle { color: root.theme.chrome; radius: 8; border.color: parent.activeFocus ? root.theme.accent : root.theme.line }
            }
            TextArea {
                id: managedGroupDescriptionInput
                objectName: "managedGroupDescriptionInput"
                visible: root.groupDetails.can_manage === true
                Layout.fillWidth: true
                Layout.preferredHeight: 76
                placeholderText: "Group description"
                color: root.theme.ink
                selectByMouse: true
                wrapMode: TextEdit.Wrap
                textFormat: TextEdit.PlainText
                Accessible.name: "Synthetic group description"
                background: Rectangle { color: root.theme.chrome; radius: 8; border.color: parent.activeFocus ? root.theme.accent : root.theme.line }
            }
            GroupAction {
                objectName: "saveManagedGroupDetails"
                visible: root.groupDetails.can_manage === true
                enabled: !root.controller.routeLoading
                text: "Save group details"
                primary: true
                onClicked: root.saveManagedGroup()
                Accessible.name: "Save synthetic group details"
            }
            Label {
                text: root.groupDetails.membership_state === "joined" ? "Membership · Joined locally" : "Membership · Left locally"
                textFormat: Text.PlainText
                color: root.theme.muted
                Layout.fillWidth: true
            }
            RowLayout {
                visible: root.groupDetails.can_manage === true
                Layout.fillWidth: true
                spacing: 8
                ComboBox {
                    id: syntheticGroupParticipantChoice
                    objectName: "syntheticGroupParticipantChoice"
                    Layout.fillWidth: true
                    model: root.availableSyntheticParticipants()
                    textRole: "name"
                    valueRole: "id"
                    visible: count > 0
                    Accessible.name: "Synthetic participants to add"
                }
                GroupAction {
                    objectName: "addSyntheticGroupParticipant"
                    visible: syntheticGroupParticipantChoice.visible
                    enabled: !root.controller.routeLoading && syntheticGroupParticipantChoice.currentIndex >= 0
                    text: "Add"
                    primary: true
                    onClicked: root.changeParticipant("add", String(syntheticGroupParticipantChoice.currentValue || ""))
                }
                Label {
                    visible: syntheticGroupParticipantChoice.count === 0
                    text: "All synthetic participants are already in this group."
                    textFormat: Text.PlainText
                    color: root.theme.muted
                    wrapMode: Text.WordWrap
                    Layout.fillWidth: true
                }
            }
            Repeater {
                objectName: "managedSyntheticGroupParticipantRows"
                model: root.groupParticipants
                delegate: RowLayout {
                    required property var modelData
                    readonly property string participantId: String(modelData.id || "")
                    Layout.fillWidth: true
                    spacing: 6
                    Label {
                        text: String(modelData.display_name || "Synthetic participant") + " · " + String(modelData.role || "Member")
                        textFormat: Text.PlainText
                        color: root.theme.ink
                        wrapMode: Text.WordWrap
                        Layout.fillWidth: true
                    }
                    GroupAction {
                        objectName: "promoteSyntheticParticipant_" + participantId.replace(/[^A-Za-z0-9]/g, "_")
                        visible: root.groupDetails.can_manage === true && modelData.role === "Member"
                        enabled: !root.controller.routeLoading
                        text: "Make admin"
                        onClicked: root.changeParticipant("promote", participantId)
                    }
                    GroupAction {
                        objectName: "demoteSyntheticParticipant_" + participantId.replace(/[^A-Za-z0-9]/g, "_")
                        visible: root.groupDetails.can_manage === true && modelData.role === "Admin"
                        enabled: !root.controller.routeLoading
                        text: "Remove admin"
                        onClicked: root.changeParticipant("demote", participantId)
                    }
                    GroupAction {
                        objectName: "removeSyntheticParticipant_" + participantId.replace(/[^A-Za-z0-9]/g, "_")
                        visible: root.groupDetails.can_manage === true && modelData.role !== "Owner"
                        enabled: !root.controller.routeLoading
                        text: "Remove"
                        onClicked: root.changeParticipant("remove", participantId)
                    }
                }
            }
            RowLayout {
                visible: root.groupDetails.membership_state === "joined" || root.groupDetails.membership_state === "left"
                Layout.fillWidth: true
                GroupAction {
                    objectName: "leaveSyntheticGroup"
                    visible: root.groupDetails.membership_state === "joined"
                    enabled: !root.controller.routeLoading
                    text: "Leave locally"
                    onClicked: root.changeMembership("group.leave")
                }
                GroupAction {
                    objectName: "joinSyntheticGroup"
                    visible: root.groupDetails.membership_state === "left"
                    enabled: !root.controller.routeLoading
                    text: "Rejoin locally"
                    onClicked: root.changeMembership("group.join")
                }
            }
            Label {
                objectName: "managedSyntheticGroupActionError"
                visible: root.localActionError.length > 0 || root.actionError.length > 0
                text: root.localActionError.length > 0 ? root.localActionError : root.actionError
                textFormat: Text.PlainText
                color: root.theme.danger
                wrapMode: Text.WordWrap
                Layout.fillWidth: true
            }
        }
    }
    Label {
        visible: !!root.record.community_id
        text: "Community: " + String(root.record.community_title || "Untitled community")
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        text: "Retained messages: " + Number(root.record.message_count || 0) + "\nMedia items: " + Number(root.record.media_count || 0)
        textFormat: Text.PlainText
        color: root.theme.ink
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        text: "Counts cover retained local messages only. Deleted and unavailable protected content is excluded."
        textFormat: Text.PlainText
        color: root.theme.muted
        font.pixelSize: 12
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        text: "Local organization: " + [root.record.archived ? "Archived" : "", root.record.pinned ? "Pinned" : "", root.record.muted ? "Muted" : "", root.record.favorite ? "Favorite" : "", root.record.marked_unread ? "Marked unread" : ""].filter(function(value) { return value.length > 0; }).join(" · ")
        visible: !!(root.record.archived || root.record.pinned || root.record.muted || root.record.favorite || root.record.marked_unread)
        textFormat: Text.PlainText
        color: root.theme.muted
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }
    Label {
        text: root.record.details_provenance === "demo_synthetic" ? "Illustrative fixture only; no live profile details were fetched." : "Participants, About, avatar, presence and privacy settings are not supplied by this local view."
        textFormat: Text.PlainText
        color: root.theme.muted
        font.pixelSize: 12
        wrapMode: Text.WordWrap
        Layout.fillWidth: true
    }

    component GroupAction: Button {
        id: action
        property bool primary: false
        implicitHeight: 40
        implicitWidth: Math.max(96, implicitContentWidth + 28)
        padding: 12
        Accessible.name: text
        contentItem: Text {
            text: action.text
            textFormat: Text.PlainText
            color: action.primary ? (root.theme.dark ? "#062e26" : "white") : root.theme.ink
            font: action.font
            horizontalAlignment: Text.AlignHCenter
            verticalAlignment: Text.AlignVCenter
            elide: Text.ElideRight
        }
        background: Rectangle {
            radius: 8
            color: action.primary ? root.theme.accent : (action.hovered ? root.theme.line : root.theme.active)
            border.width: action.activeFocus ? 2 : 1
            border.color: action.activeFocus ? root.theme.accent : root.theme.line
            opacity: action.enabled ? 1 : 0.45
        }
    }
}
