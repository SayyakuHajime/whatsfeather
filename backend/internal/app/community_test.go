package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type communityTestGroup struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Groups []Chat `json:"groups"`
}

type communityTestPage struct {
	Communities []communityTestGroup `json:"communities"`
	Source      string               `json:"source"`
	HasMore     bool                 `json:"has_more"`
}

type communityNoDirectoryTransport struct{}

func (communityNoDirectoryTransport) Connect(context.Context) error { return nil }
func (communityNoDirectoryTransport) Disconnect()                   {}
func (communityNoDirectoryTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

type communityFake struct {
	mu      sync.Mutex
	groups  []CommunityGroupInfo
	err     error
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (*communityFake) Connect(context.Context) error                               { return nil }
func (*communityFake) Disconnect()                                                 {}
func (*communityFake) Send(context.Context, string, string, string) (int64, error) { return 0, nil }

func (f *communityFake) GetJoinedGroups(ctx context.Context) ([]CommunityGroupInfo, error) {
	f.mu.Lock()
	f.calls++
	groups, err, entered, release := append([]CommunityGroupInfo(nil), f.groups...), f.err, f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release // Return even when canceled: App must reject at the commit boundary.
	}
	return groups, err
}

func (f *communityFake) set(groups []CommunityGroupInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups, f.err = append([]CommunityGroupInfo(nil), groups...), err
	f.entered, f.release = nil, nil
}

func refreshCommunityForTest(t *testing.T, a *App) error {
	t.Helper()
	_, err := a.Handle(context.Background(), "community.refresh", json.RawMessage(`{}`))
	return err
}

func communityGroupsForTest() []CommunityGroupInfo {
	parentA := "120363000000000001@g.us"
	parentB := "120363000000000002@g.us"
	return []CommunityGroupInfo{
		{ID: parentA, Title: "Garden Circle", IsParent: true},
		{ID: "120363000000000001-1@g.us", Title: "Seed Swap", LinkedParentID: parentA},
		{ID: "120363000000000001-2@g.us", Title: "Announcements", LinkedParentID: parentA, IsAnnouncement: true, IsAnnouncementKnown: true},
		// Duplicate protocol rows must not duplicate the visible chat.
		{ID: "120363000000000001-1@g.us", Title: "Seed Swap", LinkedParentID: parentA},
		// A child whose parent is absent from this authoritative snapshot is not assigned.
		{ID: "120363000000000003-1@g.us", Title: "Unverified Child", LinkedParentID: "120363000000000003@g.us"},
		// A verified parent also represents its own actual parent chat exactly once.
		{ID: parentB, Title: "Self Parent", IsParent: true, LinkedParentID: parentB},
	}
}

func communityCacheForTest(t *testing.T, a *App) []byte {
	t.Helper()
	page := invoke(t, a, "community.list", map[string]any{"limit": 100})
	delete(page, "connected") // connection state is intentionally live, not part of cached contents
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestCommunitySnapshotUsesVerifiedParentJIDWhenTitleIsEmpty(t *testing.T) {
	parentID, childID := "120363000000000088@g.us", "120363000000000088-1@g.us"
	snapshot, err := projectCommunitySnapshot([]CommunityGroupInfo{
		{ID: parentID, Title: "", IsParent: true},
		{ID: childID, Title: "Member", LinkedParentID: parentID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.members) != 2 {
		t.Fatalf("verified parent and child membership count = %d, want 2: %#v", len(snapshot.members), snapshot.members)
	}
	foundChild := false
	for _, member := range snapshot.members {
		if member.chatID == childID && member.communityID == parentID {
			foundChild = true
		}
	}
	if !foundChild {
		t.Fatalf("verified JID link was discarded because its metadata title is empty: %#v", snapshot.members)
	}
}

func TestCommunitySchemaMigrationPreservesMembershipAndChatHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	parentID, childID := "120363000000000089@g.us", "120363000000000089-1@g.us"
	if err = a.Record(context.Background(), Message{ID: "migration-history", ChatID: childID, Body: "preserve me", Timestamp: 10, Status: "received"}, "Member"); err != nil {
		t.Fatal(err)
	}
	fake := &communityFake{groups: []CommunityGroupInfo{{ID: parentID, Title: "Migration Folder", IsParent: true}, {ID: childID, Title: "Member", LinkedParentID: parentID}}}
	a.SetTransport(fake)
	a.SetStatus("connected", "test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("ALTER TABLE community_members DROP COLUMN chat_title"); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page := invoke(t, reopened, "community.list", map[string]any{})
	if page["source"] != "live_cache" {
		t.Fatalf("cache source lost on migration: %#v", page)
	}
	chats := invoke(t, reopened, "chat.list", map[string]any{})["chats"].([]any)
	var found bool
	for _, raw := range chats {
		chat := raw.(map[string]any)
		if chat["id"] == childID {
			found = chat["community_id"] == parentID && chat["community_title"] == "Migration Folder"
		}
	}
	if !found {
		t.Fatal("membership did not survive community schema migration")
	}
	messages := invoke(t, reopened, "message.list", map[string]any{"chat_id": childID})["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["id"] != "migration-history" {
		t.Fatalf("chat history lost across migration: %#v", messages)
	}
}

func TestCommunityCapabilitiesDistinguishOfflineAndLiveRefreshedCache(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	readReason := func() string {
		caps := invoke(t, a, "app.capabilities", map[string]any{})
		methods := caps["methods"].([]any)
		methodSet := make(map[string]bool, len(methods))
		for _, rawMethod := range methods {
			methodSet[rawMethod.(string)] = true
		}
		if !methodSet["community.list"] || !methodSet["community.refresh"] || methodSet["community.info"] || methodSet["group.create"] {
			t.Fatalf("capabilities advertised an incomplete or unsupported community route: %v", methods)
		}
		features := caps["features"].(map[string]any)
		feature := features["community-folders"].(map[string]any)
		if feature["available"] != true {
			t.Fatalf("community-folders feature unavailable: %#v", feature)
		}
		return feature["reason"].(string)
	}
	if reason := readReason(); !strings.Contains(reason, "No cached") || !strings.Contains(reason, "offline") {
		t.Fatalf("empty offline cache state not distinguished: %q", reason)
	}
	a.SetTransport(communityNoDirectoryTransport{})
	a.SetStatus("connected", "test", "")
	if reason := readReason(); !strings.Contains(reason, "transport does not support refresh") {
		t.Fatalf("transport-gated refresh state not distinguished: %q", reason)
	}
	fake := &communityFake{groups: []CommunityGroupInfo{{ID: "120363000000000099@g.us", Title: "Live Cache", IsParent: true}}}
	a.SetTransport(fake)
	a.SetStatus("connected", "test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	if reason := readReason(); !strings.Contains(reason, "live-refreshed") {
		t.Fatalf("live-refreshed cache state not distinguished: %q", reason)
	}
	a.SetStatus("disconnected", "offline", "")
	if reason := readReason(); !strings.Contains(reason, "offline cached") {
		t.Fatalf("offline cached state not distinguished: %q", reason)
	}
}

func TestCommunityRefreshProjectsOnlyVerifiedParentLinksAndPreservesChatIDs(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	childID := "120363000000000001-1@g.us"
	announcementID := "120363000000000001-2@g.us"
	looseStoredID := "120363000000000004-1@g.us"
	looseMissingID := "120363000000000004-2@g.us"
	if err = a.Record(context.Background(), Message{ID: "history", ChatID: childID, Body: "kept", Timestamp: 1, Status: "received"}, "Seed Swap"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "announcement-history", ChatID: announcementID, Body: "welcome", Timestamp: 2, Status: "received"}, "Announcements"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "community-history", ChatID: "120363000000000001@g.us", Body: "welcome to the community", Timestamp: 3, Status: "received"}, "Garden Circle"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "loose-history", ChatID: looseStoredID, Body: "leave me alone", Timestamp: 4, Status: "received"}, "Stored loose title"); err != nil {
		t.Fatal(err)
	}
	groups := append(communityGroupsForTest(),
		CommunityGroupInfo{ID: looseStoredID, Title: "SDK loose title"},
		CommunityGroupInfo{ID: looseMissingID, Title: "Do not add loose group"},
	)
	fake := &communityFake{groups: groups}
	a.SetTransport(fake)
	a.SetStatus("connected", "synthetic connected test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(invoke(t, a, "community.list", map[string]any{"limit": 100}))
	var got communityTestPage
	if err = json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "live_cache" || len(got.Communities) != 2 {
		t.Fatalf("unexpected authoritative projection: %s", encoded)
	}
	var garden *communityTestGroup
	for i := range got.Communities {
		if got.Communities[i].ID == "120363000000000001@g.us" {
			garden = &got.Communities[i]
		}
	}
	if garden == nil || garden.Title != "Garden Circle" || len(garden.Groups) != 3 {
		t.Fatalf("missing/deduplicated folder children: %s", encoded)
	}
	seenChild, seenAnnouncement, seenParent := false, false, false
	for _, group := range garden.Groups {
		if group.ID == childID && group.Title == "Seed Swap" && group.CommunityID == garden.ID && group.CommunityTitle == garden.Title {
			seenChild = true
		}
		if group.ID == "120363000000000001-2@g.us" && group.IsCommunityAnnouncement != nil && *group.IsCommunityAnnouncement {
			seenAnnouncement = true
		}
		if group.ID == garden.ID && group.CommunityID == garden.ID && group.CommunityTitle == garden.Title {
			seenParent = true
		}
	}
	if !seenChild || !seenAnnouncement || !seenParent {
		t.Fatalf("group/announcement/parent identity metadata missing: %#v", garden.Groups)
	}
	chats := invoke(t, a, "chat.list", map[string]any{"query": "Garden Circle"})["chats"].([]any)
	var found bool
	for _, raw := range chats {
		c := raw.(map[string]any)
		if c["id"] == childID {
			found = true
			if c["community_id"] != garden.ID {
				t.Fatalf("chat lost its verified parent identity: %#v", c)
			}
		}
	}
	if !found {
		t.Fatal("community-title search did not find loaded member group")
	}
	allChats := invoke(t, a, "chat.list", map[string]any{})["chats"].([]any)
	announcementChatCount, parentChatCount := 0, 0
	looseStoredSeen, looseMissingSeen := false, false
	for _, raw := range allChats {
		c := raw.(map[string]any)
		if c["id"] == announcementID {
			announcementChatCount++
			if c["community_id"] != garden.ID {
				t.Fatalf("announcement chat lost parent membership: %#v", c)
			}
		}
		if c["id"] == garden.ID {
			parentChatCount++
			if c["community_id"] != garden.ID {
				t.Fatalf("parent chat lost its verified self-parent membership: %#v", c)
			}
		}
		if c["id"] == "120363000000000003-1@g.us" && c["community_id"] != nil {
			t.Fatalf("unverified parent assignment leaked into chat projection: %#v", c)
		}
		if c["id"] == "120363000000000002@g.us" && c["community_id"] != "120363000000000002@g.us" {
			t.Fatalf("verified self-parent membership missing: %#v", c)
		}
		if c["id"] == looseStoredID {
			looseStoredSeen = true
			if c["title"] != "Stored loose title" || c["community_id"] != nil {
				t.Fatalf("existing loose group changed during community refresh: %#v", c)
			}
		}
		if c["id"] == looseMissingID {
			looseMissingSeen = true
		}
	}
	if announcementChatCount != 1 || parentChatCount != 1 {
		t.Fatalf("announcement or parent chat was lost or duplicated: announcement=%d parent=%d", announcementChatCount, parentChatCount)
	}
	if !looseStoredSeen || looseMissingSeen {
		t.Fatalf("loose group projection was modified or invented: stored_seen=%t missing_seen=%t", looseStoredSeen, looseMissingSeen)
	}
	parentMessages := invoke(t, a, "message.list", map[string]any{"chat_id": "120363000000000001@g.us"})["messages"].([]any)
	if len(parentMessages) != 1 || parentMessages[0].(map[string]any)["id"] != "community-history" {
		t.Fatalf("parent history lost its real chat ID: %#v", parentMessages)
	}
	announcementMessages := invoke(t, a, "message.list", map[string]any{"chat_id": announcementID})["messages"].([]any)
	if len(announcementMessages) != 1 || announcementMessages[0].(map[string]any)["id"] != "announcement-history" {
		t.Fatalf("announcement history lost its real chat ID: %#v", announcementMessages)
	}
}

func TestCommunitySnapshotReparentsAndUnlinksRemovedMemberships(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fake := &communityFake{groups: []CommunityGroupInfo{{ID: "120363000000000010@g.us", Title: "Old Parent", IsParent: true}, {ID: "120363000000000010-1@g.us", Title: "Movable", LinkedParentID: "120363000000000010@g.us"}}}
	a.SetTransport(fake)
	a.SetStatus("connected", "test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	fake.set([]CommunityGroupInfo{{ID: "120363000000000020@g.us", Title: "New Parent", IsParent: true}, {ID: "120363000000000010-1@g.us", Title: "Movable", LinkedParentID: "120363000000000020@g.us"}}, nil)
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	chats := invoke(t, a, "chat.list", map[string]any{})["chats"].([]any)
	var moved bool
	for _, raw := range chats {
		c := raw.(map[string]any)
		if c["id"] == "120363000000000010-1@g.us" {
			moved = true
			if c["community_id"] != "120363000000000020@g.us" {
				t.Fatalf("stale/reparented membership: %#v", c)
			}
		}
	}
	if !moved {
		t.Fatal("reparented chat disappeared")
	}
	pageBytes, _ := json.Marshal(invoke(t, a, "community.list", map[string]any{"limit": 100}))
	var decoded communityTestPage
	if err = json.Unmarshal(pageBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, folder := range decoded.Communities {
		for _, group := range folder.Groups {
			if group.ID == "120363000000000010-1@g.us" && folder.ID != "120363000000000020@g.us" {
				t.Fatalf("removed membership remained in %s", folder.ID)
			}
		}
	}
}

func TestCommunityRefreshFailuresCancellationAndStaleSessionPreserveSnapshot(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fake := &communityFake{groups: []CommunityGroupInfo{{ID: "120363000000000030@g.us", Title: "Stable", IsParent: true}}}
	a.SetTransport(fake)
	a.SetStatus("connected", "test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	before := communityCacheForTest(t, a)
	fake.set([]CommunityGroupInfo{{ID: "120363000000000040@g.us", Title: "Must Not Replace", IsParent: true}}, errors.New("synthetic fetch failure"))
	if err = refreshCommunityForTest(t, a); err == nil {
		t.Fatal("fetch error accepted")
	}
	after := communityCacheForTest(t, a)
	if string(before) != string(after) {
		t.Fatalf("failed fetch changed cached snapshot: before=%s after=%s", before, after)
	}

	fake.set([]CommunityGroupInfo{{ID: "120363000000000040@g.us", Title: "Canceled", IsParent: true}}, nil)
	fake.mu.Lock()
	fake.entered, fake.release = make(chan struct{}, 1), make(chan struct{})
	entered, release := fake.entered, fake.release
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { _, callErr := a.Handle(ctx, "community.refresh", json.RawMessage(`{}`)); canceled <- callErr }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not enter transport")
	}
	cancel()
	close(release)
	if err = <-canceled; err == nil {
		t.Fatal("canceled refresh committed")
	}
	after = communityCacheForTest(t, a)
	if string(before) != string(after) {
		t.Fatalf("canceled fetch changed cached snapshot: %s", after)
	}

	fake.set([]CommunityGroupInfo{{ID: "120363000000000050@g.us", Title: "Stale", IsParent: true}}, nil)
	fake.mu.Lock()
	fake.entered, fake.release = make(chan struct{}, 1), make(chan struct{})
	entered, release = fake.entered, fake.release
	fake.mu.Unlock()
	stale := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(context.Background(), "community.refresh", json.RawMessage(`{}`))
		stale <- callErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not enter transport")
	}
	a.SetStatus("disconnected", "session changed", "")
	close(release)
	if err = <-stale; err == nil {
		t.Fatal("stale session refresh committed")
	}
	after = communityCacheForTest(t, a)
	if string(before) != string(after) {
		t.Fatalf("stale fetch changed cached snapshot: %s", after)
	}
}

func TestCommunityRefreshRequiresConnectedLiveDirectoryAndHasNoStartupFetch(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fake := &communityFake{}
	a.SetTransport(fake)
	if err = refreshCommunityForTest(t, a); err == nil {
		t.Fatal("disconnected refresh accepted")
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("transport fetched before connected explicit refresh: %d", calls)
	}
	a.SetStatus("connected", "test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	calls = fake.calls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("unexpected fetch count: %d", calls)
	}
}

func TestCommunityRefreshParticipatesInCloseBarrier(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	fake := &communityFake{
		groups:  []CommunityGroupInfo{{ID: "120363000000000060@g.us", Title: "Pending", IsParent: true}},
		entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	a.SetTransport(fake)
	a.SetStatus("connected", "test", "")
	refreshDone := make(chan error, 1)
	go func() {
		_, callErr := a.Handle(context.Background(), "community.refresh", json.RawMessage(`{}`))
		refreshDone <- callErr
	}()
	select {
	case <-fake.entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not enter transport")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- a.Close() }()
	deadline := time.After(time.Second)
	for {
		a.mu.Lock()
		closing := a.closing
		a.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Close did not acquire teardown ownership")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case closeErr := <-closeDone:
		t.Fatalf("Close passed an active community operation: %v", closeErr)
	case <-time.After(25 * time.Millisecond):
	}
	close(fake.release)
	if callErr := <-refreshDone; callErr == nil {
		t.Fatal("refresh should not commit after Close owns shutdown")
	}
	if closeErr := <-closeDone; closeErr != nil {
		t.Fatal(closeErr)
	}
}
