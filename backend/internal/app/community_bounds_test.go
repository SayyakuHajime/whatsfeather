package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatListCommunityMetadataRespectsHydratedFrameBudgetAt1000ChatLimit(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	groups := make([]CommunityGroupInfo, 0, 1000)
	for i := 0; i < 500; i++ {
		parent := fmt.Sprintf("120363000000002%04d@g.us", i)
		child := strings.TrimSuffix(parent, "@g.us") + "-1@g.us"
		groups = append(groups,
			CommunityGroupInfo{ID: parent, Title: strings.Repeat("<", 128), IsParent: true},
			CommunityGroupInfo{ID: child, Title: strings.Repeat("<", 128), LinkedParentID: parent},
		)
	}
	fake := &communityFake{groups: groups}
	a.SetTransport(fake)
	a.SetStatus("connected", "synthetic connected test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}
	cursor := ""
	seen := make(map[string]bool, 1000)
	for pageNumber := 0; pageNumber < 20; pageNumber++ {
		result := invoke(t, a, "chat.list", map[string]any{"limit": 1000, "cursor": cursor})
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if len(encoded) > pageResultBudget {
			t.Fatalf("chat.list page exceeds hydrated frame budget: %d bytes", len(encoded))
		}
		for _, raw := range result["chats"].([]any) {
			chat := raw.(map[string]any)
			id := chat["id"].(string)
			if seen[id] {
				t.Fatalf("chat-list cursor repeated %s", id)
			}
			seen[id] = true
			if chat["community_id"] == nil || len(chat["community_title"].(string)) != 128 {
				t.Fatalf("community metadata missing from loaded chat %s", id)
			}
		}
		if result["has_more"] != true {
			break
		}
		cursor = result["next_cursor"].(string)
		if cursor == "" {
			t.Fatal("chat.list omitted cursor for a truncated response")
		}
	}
	if len(seen) != 1000 {
		t.Fatalf("budgeted chat list returned %d chats across pages, want 1000", len(seen))
	}
}

func TestCommunityListPaginatesWithinFrameBudgetWithoutPrivateProjections(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	groups := make([]CommunityGroupInfo, 0, 202)
	for i := 0; i < 101; i++ {
		parent := fmt.Sprintf("120363000000001%03d@g.us", i)
		groups = append(groups,
			CommunityGroupInfo{ID: parent, Title: strings.Repeat("<", 128), IsParent: true},
			CommunityGroupInfo{ID: parent[:len(parent)-5] + "-1@g.us", Title: strings.Repeat("<", 128), LinkedParentID: parent},
		)
	}
	fake := &communityFake{groups: groups}
	a.SetTransport(fake)
	a.SetStatus("connected", "synthetic test", "")
	if err = refreshCommunityForTest(t, a); err != nil {
		t.Fatal(err)
	}

	cursor := ""
	seen := make(map[string]bool)
	for pageNumber := 0; pageNumber < 3; pageNumber++ {
		result := invoke(t, a, "community.list", map[string]any{"limit": 1000, "cursor": cursor})
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if len(encoded)+128 > 256*1024 {
			t.Fatalf("community page exceeds RPC frame allowance: %d bytes", len(encoded)+128)
		}
		if strings.Contains(string(encoded), "participants") || strings.Contains(string(encoded), "credential") {
			t.Fatalf("private group/credential projection leaked: %s", encoded)
		}
		folders := result["communities"].([]any)
		for _, rawFolder := range folders {
			folder := rawFolder.(map[string]any)
			for _, rawGroup := range folder["groups"].([]any) {
				group := rawGroup.(map[string]any)
				id := group["id"].(string)
				if seen[id] {
					t.Fatalf("cursor repeated member chat %s", id)
				}
				seen[id] = true
				if group["community_id"] != folder["id"] || group["community_title"] != folder["title"] {
					t.Fatalf("folder identity lost while paging: %#v", group)
				}
			}
		}
		if result["has_more"] != true {
			break
		}
		cursor = result["next_cursor"].(string)
		if cursor == "" {
			t.Fatal("paged response omitted its continuation cursor")
		}
	}
	if len(seen) != 202 {
		t.Fatalf("paged community directory exposed %d groups, want 202 parent/member chats", len(seen))
	}
}
