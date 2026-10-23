package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChatInfoCachedCountsAndLinks(t *testing.T) {
	a, dir := hubApp(t)
	hubMedia(t, a, "hub:a", "image", "image", "BODY_MUST_NOT_LEAK", 1)
	hubMedia(t, a, "hub:a", "protected", "unsupported", "PRIVATE", 2)
	hubMedia(t, a, "hub:a", "deleted", "image", "PRIVATE", 3)
	hubMedia(t, a, "hub:b", "other", "audio", "PRIVATE", 4)
	for _, q := range []string{`INSERT INTO message_state(chat_id,message_id,deleted) VALUES('hub:a','deleted',1)`, `INSERT INTO chat_organization(chat_id,archived,pinned,muted,unread_mark,favorite,list_ids) VALUES('hub:a',1,1,1,1,1,'["work"]')`, `INSERT INTO communities(id,title) VALUES('parent','Cached Parent')`, `INSERT INTO community_members(chat_id,community_id,is_announcement) VALUES('hub:a','parent',0)`} {
		if _, err := a.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()
	var err error
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(&hubNoTransport{t})
	v, err := budgetCall(a, "chat.info", map[string]any{"chat_id": "hub:a"})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(map[string]any)
	if r["id"] != "hub:a" || r["message_count"] != int64(1) || r["media_count"] != int64(1) || r["provenance"] != "local_cached" || r["organization_provenance"] != "local_only" || r["community_id"] != "parent" {
		t.Fatalf("bad cached info: %#v", r)
	}
	if r["archived"] != true || r["favorite"] != true {
		t.Fatal("flags lost")
	}
	b, _ := json.Marshal(r)
	for _, s := range []string{"BODY_MUST_NOT_LEAK", "PRIVATE", "participants", "avatar", "about", "presence"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("unexpected field/body %s", s)
		}
	}
	fields := r["available_fields"].([]string)
	if len(fields) == 0 {
		t.Fatal("no field disclosure")
	}
	if _, err := budgetCall(a, "media.list", map[string]any{"chat_id": "hub:a"}); err != nil {
		t.Fatal(err)
	}
	v, err = budgetCall(a, "chat.info", map[string]any{"chat_id": "hub:b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(map[string]any)["community_id"]; ok {
		t.Fatal("fabricated parent")
	}
}
func TestChatInfoFailClosed(t *testing.T) {
	a, _ := hubApp(t)
	for _, raw := range []string{`null`, `[]`, `{}`, `{"chat_id":""}`, `{"chat_id":"missing"}`, `{"chat_id":" hub:a"}`, `{"chat_id":"hub:a","body":true}`, `{"chat_id":4}`} {
		if _, err := a.Handle(t.Context(), "chat.info", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestChatInfoPresenceProjectionsAreBoundedSyntheticAndNeverFetchedLive(t *testing.T) {
	demo, err := Open(filepath.Join(t.TempDir(), "demo"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer demo.Close()

	decode := func(v any) map[string]any {
		t.Helper()
		encoded, marshalErr := json.Marshal(v)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var result map[string]any
		if unmarshalErr := json.Unmarshal(encoded, &result); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return result
	}
	presenceOf := func(value any) map[string]any {
		t.Helper()
		projection, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("presence projection missing or untyped: %#v", value)
		}
		if projection["provenance"] != "demo_synthetic" {
			t.Fatalf("presence provenance is not explicit: %#v", projection)
		}
		encoded, err := json.Marshal(projection)
		if err != nil || len(encoded) > 1024 {
			t.Fatalf("presence projection exceeds its bound: bytes=%d err=%v", len(encoded), err)
		}
		return projection
	}
	requestInfo := func(app *App, chatID string) map[string]any {
		t.Helper()
		value, err := app.Handle(t.Context(), "chat.info", json.RawMessage(`{"chat_id":"`+chatID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		return decode(value)
	}

	started := time.Now().UnixMilli()
	contact := requestInfo(demo, "demo:alex")
	contactDetails, ok := contact["contact_details"].(map[string]any)
	if !ok || contact["details_provenance"] != "demo_synthetic" {
		t.Fatalf("missing synthetic contact details: %#v", contact)
	}
	contactPresence := presenceOf(contactDetails["presence"])
	if contactPresence["state"] != "online" {
		t.Fatalf("unexpected contact presence state: %#v", contactPresence)
	}
	contactExpiry, ok := contactPresence["expires_at_ms"].(float64)
	if !ok || contactExpiry <= float64(started) || contactExpiry > float64(time.Now().Add(time.Minute).UnixMilli()) {
		t.Fatalf("contact presence expiry is absent or unbounded: %#v", contactPresence)
	}

	group := requestInfo(demo, "demo:studio")
	groupDetails, ok := group["group_details"].(map[string]any)
	participants, participantsOK := groupDetails["participants"].([]any)
	if !ok || !participantsOK || len(participants) != 3 || group["details_provenance"] != "demo_synthetic" {
		t.Fatalf("missing bounded synthetic group participants: %#v", group)
	}
	wantStates := map[string]string{"Alex Morgan": "online", "Maya Chen": "typing", "Sam Rivera": "offline"}
	for _, participantValue := range participants {
		participant, ok := participantValue.(map[string]any)
		if !ok {
			t.Fatalf("untyped synthetic participant: %#v", participantValue)
		}
		name, ok := participant["display_name"].(string)
		if !ok || wantStates[name] == "" {
			t.Fatalf("unexpected synthetic participant identity: %#v", participant)
		}
		presence := presenceOf(participant["presence"])
		if presence["state"] != wantStates[name] {
			t.Fatalf("unexpected participant presence: name=%q presence=%#v", name, presence)
		}
		delete(wantStates, name)
		if name == "Sam Rivera" {
			lastSeen, ok := presence["last_seen_at_ms"].(float64)
			if !ok || lastSeen < float64(started-15*60*1000) || lastSeen > float64(time.Now().UnixMilli()) {
				t.Fatalf("offline last-seen timestamp is missing or unbounded: %#v", presence)
			}
			continue
		}
		expires, ok := presence["expires_at_ms"].(float64)
		if !ok || expires <= float64(started) || expires > float64(time.Now().Add(time.Minute).UnixMilli()) {
			t.Fatalf("transient presence expiry is absent or unbounded: %#v", presence)
		}
	}
	if len(wantStates) != 0 {
		t.Fatalf("synthetic participants were incomplete: %#v", wantStates)
	}
	encodedGroup, err := json.Marshal(groupDetails)
	if err != nil || len(encodedGroup) > 4096 {
		t.Fatalf("synthetic group projection exceeds the 4 KiB bound: bytes=%d err=%v", len(encodedGroup), err)
	}

	live, err := Open(filepath.Join(t.TempDir(), "live"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err = live.db.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES('demo:alex','Alex Morgan','AM','#526a63',0)"); err != nil {
		t.Fatal(err)
	}
	liveInfo := requestInfo(live, "demo:alex")
	if _, exists := liveInfo["contact_details"]; exists {
		t.Fatal("synthetic presence-bearing contact details leaked into live mode")
	}
	if _, exists := liveInfo["presence"]; exists {
		t.Fatal("live chat.info fetched or projected presence without explicit subscription")
	}
}

func TestChatInfoDemoDetailsAreSyntheticAndNeverLeakIntoLiveMode(t *testing.T) {
	demo, err := Open(filepath.Join(t.TempDir(), "demo"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer demo.Close()

	decode := func(v any) map[string]any {
		t.Helper()
		encoded, marshalErr := json.Marshal(v)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var result map[string]any
		if unmarshalErr := json.Unmarshal(encoded, &result); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return result
	}

	contactValue, err := demo.Handle(t.Context(), "chat.info", json.RawMessage(`{"chat_id":"demo:alex"}`))
	if err != nil {
		t.Fatal(err)
	}
	contact := decode(contactValue)
	contactDetails, ok := contact["contact_details"].(map[string]any)
	if !ok || contact["details_provenance"] != "demo_synthetic" || contactDetails["display_name"] != "Alex Morgan" || contactDetails["phone"] != "+1-202-555-0147" || contactDetails["about"] != "Synthetic demo contact for local UI tests." {
		t.Fatalf("missing synthetic contact details: %#v", contact)
	}

	groupValue, err := demo.Handle(t.Context(), "chat.info", json.RawMessage(`{"chat_id":"demo:studio"}`))
	if err != nil {
		t.Fatal(err)
	}
	group := decode(groupValue)
	groupDetails, ok := group["group_details"].(map[string]any)
	participants, participantsOK := groupDetails["participants"].([]any)
	if !ok || group["details_provenance"] != "demo_synthetic" || group["is_group"] != true || groupDetails["description"] != "Synthetic group for local UI tests." || groupDetails["participant_count"] != float64(3) || !participantsOK || len(participants) != 3 {
		t.Fatalf("missing synthetic group details: %#v", group)
	}

	live, err := Open(filepath.Join(t.TempDir(), "live"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err = live.db.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES('demo:alex','Alex Morgan','AM','#526a63',0)"); err != nil {
		t.Fatal(err)
	}
	liveValue, err := live.Handle(t.Context(), "chat.info", json.RawMessage(`{"chat_id":"demo:alex"}`))
	if err != nil {
		t.Fatal(err)
	}
	liveInfo := decode(liveValue)
	if _, exists := liveInfo["contact_details"]; exists {
		t.Fatal("synthetic contact data leaked into live mode")
	}
	if _, exists := liveInfo["group_details"]; exists {
		t.Fatal("synthetic group data leaked into live mode")
	}
	if _, exists := liveInfo["details_provenance"]; exists {
		t.Fatal("synthetic provenance leaked into live mode")
	}
	if _, err = live.db.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES('demo:studio','Design Studio','DS','#626f8a',1)"); err != nil {
		t.Fatal(err)
	}
	liveGroupValue, err := live.Handle(t.Context(), "chat.info", json.RawMessage(`{"chat_id":"demo:studio"}`))
	if err != nil {
		t.Fatal(err)
	}
	liveGroup := decode(liveGroupValue)
	if _, exists := liveGroup["group_details"]; exists {
		t.Fatal("synthetic group participants leaked into live mode")
	}
	if _, exists := liveGroup["details_provenance"]; exists {
		t.Fatal("synthetic group provenance leaked into live mode")
	}
}
