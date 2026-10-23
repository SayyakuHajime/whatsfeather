package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func decodeGroupResult(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDemoGroupCreateIsLocalSyntheticAndDurable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo-profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	result, err := a.Handle(t.Context(), "group.create", json.RawMessage(`{
		"title":"Weekend plans",
		"description":"Synthetic local group fixture",
		"member_ids":["demo:participant:maya"]
	}`))
	if err != nil {
		a.Close()
		t.Fatalf("demo group.create should create a local synthetic group: %v", err)
	}
	created := decodeGroupResult(t, result)
	chat, ok := created["chat"].(map[string]any)
	if !ok || chat["is_group"] != true || chat["title"] != "Weekend plans" {
		a.Close()
		t.Fatalf("unexpected created group projection: %#v", created)
	}
	groupID, _ := chat["id"].(string)
	if !strings.HasPrefix(groupID, "demo:group:") || len(groupID) != len("demo:group:")+32 {
		a.Close()
		t.Fatalf("group ID is not a local synthetic ID: %q", groupID)
	}
	if created["provenance"] != "demo_synthetic" || created["membership_state"] != "joined" {
		a.Close()
		t.Fatalf("group scope or membership not disclosed: %#v", created)
	}

	listed, err := a.Handle(t.Context(), "chat.list", mustJSONGroup(t, map[string]any{"query": "Weekend plans"}))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	listedChats := decodeGroupResult(t, listed)["chats"].([]any)
	if len(listedChats) != 1 || listedChats[0].(map[string]any)["id"] != groupID {
		a.Close()
		t.Fatalf("created group was not available through the chat list: %#v", listedChats)
	}

	info, err := a.Handle(t.Context(), "chat.info", mustJSONGroup(t, map[string]any{"chat_id": groupID}))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	groupInfo := decodeGroupResult(t, info)
	details, ok := groupInfo["group_details"].(map[string]any)
	participants, participantsOK := details["participants"].([]any)
	if !ok || groupInfo["details_provenance"] != "demo_synthetic" || details["description"] != "Synthetic local group fixture" || details["membership_state"] != "joined" || !participantsOK || len(participants) != 2 {
		a.Close()
		t.Fatalf("unexpected group details: %#v", groupInfo)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	info, err = a.Handle(t.Context(), "chat.info", mustJSONGroup(t, map[string]any{"chat_id": groupID}))
	if err != nil {
		t.Fatal(err)
	}
	groupInfo = decodeGroupResult(t, info)
	details, ok = groupInfo["group_details"].(map[string]any)
	if !ok || details["description"] != "Synthetic local group fixture" || details["membership_state"] != "joined" {
		t.Fatalf("group state did not persist across profile reopen: %#v", groupInfo)
	}
}

func TestGroupManagementCapabilityIsDemoOnlyAndLiveCallsFailClosed(t *testing.T) {
	live, err := Open(filepath.Join(t.TempDir(), "live-profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	liveCapabilities := live.capabilities()
	for _, method := range []string{"group.create", "group.update", "group.participants", "group.join", "group.leave"} {
		for _, advertised := range liveCapabilities["methods"].([]string) {
			if advertised == method {
				t.Fatalf("live capabilities unexpectedly advertise %s", method)
			}
		}
	}
	liveFeatures := liveCapabilities["features"].(map[string]map[string]any)
	if liveFeatures["groups-management"]["available"] != false {
		t.Fatalf("live group-management capability is not fail-closed: %#v", liveFeatures["groups-management"])
	}
	requests := map[string]string{
		"group.create":       `{"title":"Not live","description":"","member_ids":["demo:participant:maya"]}`,
		"group.update":       `{"group_id":"demo:group:00000000000000000000000000000000","title":"Not live"}`,
		"group.participants": `{"group_id":"demo:group:00000000000000000000000000000000","action":"add","participant_id":"demo:participant:maya"}`,
		"group.join":         `{"group_id":"demo:group:00000000000000000000000000000000"}`,
		"group.leave":        `{"group_id":"demo:group:00000000000000000000000000000000"}`,
	}
	for method, raw := range requests {
		if _, err = live.Handle(t.Context(), method, json.RawMessage(raw)); err == nil {
			t.Fatalf("live %s unexpectedly succeeded", method)
		}
	}
	var liveGroupCount int
	if err = live.db.QueryRow("SELECT count(*) FROM chats WHERE is_group=1").Scan(&liveGroupCount); err != nil {
		t.Fatal(err)
	}
	if liveGroupCount != 0 {
		t.Fatalf("live mode persisted %d new groups", liveGroupCount)
	}

	demo, err := Open(filepath.Join(t.TempDir(), "demo-profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer demo.Close()
	demoCapabilities := demo.capabilities()
	advertised := map[string]bool{}
	for _, method := range demoCapabilities["methods"].([]string) {
		advertised[method] = true
	}
	for method := range requests {
		if !advertised[method] {
			t.Fatalf("demo capabilities do not advertise implemented method %s", method)
		}
	}
	demoFeatures := demoCapabilities["features"].(map[string]map[string]any)
	if demoFeatures["groups-management"]["available"] != true {
		t.Fatalf("demo group-management feature is not advertised: %#v", demoFeatures["groups-management"])
	}
}

func mustJSONGroup(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestDemoGroupManagementActionsPersistAndEnforceMembership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo-profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	created, err := a.Handle(t.Context(), "group.create", json.RawMessage(`{"title":"Weekend plans","description":"Initial","member_ids":["demo:participant:maya"]}`))
	if err != nil {
		t.Fatal(err)
	}
	groupID := decodeGroupResult(t, created)["group_id"].(string)

	updated, err := a.Handle(t.Context(), "group.update", mustJSONGroup(t, map[string]any{"group_id": groupID, "title": "Updated plans", "description": "Updated synthetic description"}))
	if err != nil {
		t.Fatalf("group.update: %v", err)
	}
	updateResult := decodeGroupResult(t, updated)
	updatedChat, ok := updateResult["chat"].(map[string]any)
	if !ok || updatedChat["title"] != "Updated plans" || updateResult["provenance"] != "demo_synthetic" {
		t.Fatalf("updated group projection is incomplete: %#v", updateResult)
	}

	added, err := a.Handle(t.Context(), "group.participants", mustJSONGroup(t, map[string]any{"group_id": groupID, "action": "add", "participant_id": "demo:participant:alex"}))
	if err != nil {
		t.Fatalf("group.participants add: %v", err)
	}
	groupDetails := decodeGroupResult(t, added)["group_details"].(map[string]any)
	if groupDetails["participant_count"] != float64(3) {
		t.Fatalf("participant add was not reflected: %#v", groupDetails)
	}

	_, err = a.Handle(t.Context(), "group.participants", mustJSONGroup(t, map[string]any{"group_id": groupID, "action": "promote", "participant_id": "demo:participant:alex"}))
	if err != nil {
		t.Fatalf("group.participants promote: %v", err)
	}
	_, err = a.Handle(t.Context(), "group.participants", mustJSONGroup(t, map[string]any{"group_id": groupID, "action": "remove", "participant_id": "demo:participant:maya"}))
	if err != nil {
		t.Fatalf("group.participants remove: %v", err)
	}
	if _, err = a.Handle(t.Context(), "group.participants", mustJSONGroup(t, map[string]any{"group_id": groupID, "action": "remove", "participant_id": "demo:account"})); err == nil {
		t.Fatal("group owner must not be removable through the participant action")
	}

	left, err := a.Handle(t.Context(), "group.leave", mustJSONGroup(t, map[string]any{"group_id": groupID}))
	if err != nil {
		t.Fatalf("group.leave: %v", err)
	}
	leftResult := decodeGroupResult(t, left)
	leftDetails := leftResult["group_details"].(map[string]any)
	if leftResult["membership_state"] != "left" || leftDetails["can_manage"] != false {
		t.Fatalf("left membership was not projected: %#v", leftResult)
	}
	if _, err = a.Handle(t.Context(), "group.update", mustJSONGroup(t, map[string]any{"group_id": groupID, "title": "Should not update"})); err == nil {
		t.Fatal("a left member must not manage the local group")
	}
	joined, err := a.Handle(t.Context(), "group.join", mustJSONGroup(t, map[string]any{"group_id": groupID}))
	if err != nil {
		t.Fatalf("group.join: %v", err)
	}
	joinedResult := decodeGroupResult(t, joined)
	if joinedResult["membership_state"] != "joined" {
		t.Fatalf("local rejoin was not projected: %#v", joinedResult)
	}

	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	info, err := a.Handle(t.Context(), "chat.info", mustJSONGroup(t, map[string]any{"chat_id": groupID}))
	if err != nil {
		t.Fatal(err)
	}
	persisted := decodeGroupResult(t, info)
	persistedDetails := persisted["group_details"].(map[string]any)
	if persisted["title"] != "Updated plans" || persistedDetails["description"] != "Updated synthetic description" || persistedDetails["membership_state"] != "joined" || persistedDetails["participant_count"] != float64(2) {
		t.Fatalf("group edits, participant changes, or membership did not persist: %#v", persisted)
	}
}
