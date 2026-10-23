package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestCommunityProjectionUsesPinnedSDKParentAndLinkedParentFields(t *testing.T) {
	parentID := types.NewJID("120363000000000071", types.GroupServer)
	joined := []*types.GroupInfo{
		{JID: parentID, GroupName: types.GroupName{Name: "Verified Community"}, GroupParent: types.GroupParent{IsParent: true}},
		{JID: types.NewJID("120363000000000071-1", types.GroupServer), GroupName: types.GroupName{Name: "Member Group"}, GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: parentID}},
		{JID: types.NewJID("120363000000000071-2", types.GroupServer), GroupName: types.GroupName{Name: "Announcement Group"}, GroupAnnounce: types.GroupAnnounce{IsAnnounce: true}, GroupIsDefaultSub: types.GroupIsDefaultSub{IsDefaultSubGroup: true}, GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: parentID}},
	}
	projected := communityGroupsFromSDK(joined)
	if len(projected) != 3 {
		t.Fatalf("projected %d groups, want 3", len(projected))
	}
	if !projected[0].IsParent || projected[0].Title != "Verified Community" {
		t.Fatalf("parent metadata not projected: %#v", projected[0])
	}
	if projected[1].LinkedParentID != parentID.String() || projected[1].Title != "Member Group" || projected[1].IsAnnouncementKnown {
		t.Fatalf("linked-parent or unknown-announcement metadata projected incorrectly: %#v", projected[1])
	}
	if !projected[2].IsAnnouncement || !projected[2].IsAnnouncementKnown || projected[2].LinkedParentID != parentID.String() {
		t.Fatalf("announcement metadata not projected: %#v", projected[2])
	}
}
