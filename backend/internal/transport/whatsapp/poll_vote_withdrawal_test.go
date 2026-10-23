package whatsapp

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestPollVoteSDKEmptyWithdrawal(t *testing.T) {
	ctx := context.Background()
	owner := types.NewJID("15550000001", types.DefaultUserServer)
	voter := types.NewJID("15550000002", types.DefaultUserServer)
	chat := types.NewJID("123456", types.GroupServer)
	c := whatsmeow.NewClient(&store.Device{ID: &voter, MsgSecrets: syntheticPollSecrets{sender: owner}}, nil)
	info := types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: owner, IsGroup: true}, ID: "synthetic-poll"}
	message, err := c.BuildPollVote(ctx, &info, []string{})
	if err != nil {
		t.Fatalf("pinned BuildPollVote rejects empty labels: %v", err)
	}
	if len(message.GetPollUpdateMessage().GetVote().GetEncPayload()) == 0 {
		t.Fatal("missing encrypted withdrawal")
	}
	vote, err := c.DecryptPollVote(ctx, &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: voter, IsGroup: true}, ID: "withdrawal"}, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	if len(vote.GetSelectedOptions()) != 0 {
		t.Fatalf("withdrawal has %d selected hashes", len(vote.GetSelectedOptions()))
	}
}
