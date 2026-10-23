package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type reactionEventTransport struct {
	fail  bool
	calls int
}

func (*reactionEventTransport) Connect(context.Context) error { return nil }
func (*reactionEventTransport) Disconnect()                   {}
func (*reactionEventTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (*reactionEventTransport) SendReply(context.Context, string, string, bool, string, string, string, string) (int64, error) {
	return 0, nil
}
func (p *reactionEventTransport) SendReaction(context.Context, string, string, bool, string, string, string) (int64, error) {
	p.calls++
	if p.fail {
		return 0, errors.New("synthetic ambiguous reaction result")
	}
	return 1234, nil
}
func (*reactionEventTransport) EditMessage(context.Context, string, string, string, string) (int64, error) {
	return 0, nil
}
func (*reactionEventTransport) RevokeMessage(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (*reactionEventTransport) ForwardText(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (*reactionEventTransport) Account(context.Context) (app.Account, error) {
	return app.Account{Available: true, JID: "19995550123@s.whatsapp.net", State: "connected"}, nil
}

func TestIncomingSelfReactionEventReconcilesUnresolvedAction(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, _ := types.ParseJID("12345-67890@g.us")
	targetSender, _ := types.ParseJID("12345@s.whatsapp.net")
	self, _ := types.ParseJID("19995550123@s.whatsapp.net")
	if err = a.Record(context.Background(), app.Message{ID: "target", ChatID: chat.String(), Body: "synthetic", Kind: "text", Timestamp: 1000, Status: "received", SenderID: targetSender.String()}, "Synthetic group"); err != nil {
		t.Fatal(err)
	}
	probe := &reactionEventTransport{fail: true}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	first := []byte(`{"chat_id":"12345-67890@g.us","target_id":"target","reaction":"🔥","client_id":"adapter-reaction-unknown"}`)
	_, err = a.Handle(context.Background(), "message.react", first)
	var appErr *app.Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || probe.calls != 1 {
		t.Fatalf("first reaction err=%v calls=%d, want unknown after one dispatch", err, probe.calls)
	}

	adapter := New(a, t.TempDir())
	eventTime := time.Now().Add(24 * time.Hour)
	source := types.MessageSource{Chat: chat, Sender: self}
	adapter.event(context.Background(), nil, &events.Message{
		Info: types.MessageInfo{MessageSource: source, ID: "self-reaction-echo", Timestamp: eventTime},
		Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key:  &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String("target"), FromMe: proto.Bool(false), Participant: proto.String(targetSender.String())},
			Text: proto.String("💚"),
		}},
	})
	_, err = a.Handle(context.Background(), "message.react", first)
	if !errors.As(err, &appErr) || appErr.Code != "reconciled" {
		t.Fatalf("same action after server event returned %v, want reconciled", err)
	}
	probe.fail = false
	fresh := []byte(`{"chat_id":"12345-67890@g.us","target_id":"target","reaction":"🔥","client_id":"adapter-reaction-after-reconcile"}`)
	if _, err = a.Handle(context.Background(), "message.react", fresh); err != nil {
		t.Fatalf("new action after reaction event failed: %v", err)
	}
	if probe.calls != 2 {
		t.Fatalf("reaction calls=%d, want original dispatch plus one explicit post-reconciliation action", probe.calls)
	}
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345-67890@g.us"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if message.ID == "target" {
			if message.Reactions == nil || len(*message.Reactions) != 1 || (*message.Reactions)[0].Emoji != "🔥" || (*message.Reactions)[0].SenderID != self.String() || (*message.Reactions)[0].Provenance != "accepted" {
				t.Fatalf("accepted reaction did not replace the remote snapshot in projection: %#v", message.Reactions)
			}
			return
		}
	}
	t.Fatal("target message missing from synthetic message.list projection")
}

func TestIncomingReactionEditAndRevokePersistFromProtocolEvents(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, _ := types.ParseJID("12345-67890@g.us")
	actor, _ := types.ParseJID("12345@s.whatsapp.net")
	if err = a.Record(context.Background(), app.Message{ID: "target", ChatID: chat.String(), Body: "original", Kind: "text", Timestamp: 1000, Status: "received", SenderID: actor.String()}, "Synthetic group"); err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	source := types.MessageSource{Chat: chat, Sender: actor}
	adapter.event(context.Background(), nil, &events.Message{Info: types.MessageInfo{MessageSource: source, ID: "reaction-event", Timestamp: time.UnixMilli(2000)}, Message: &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String("target"), FromMe: proto.Bool(false), Participant: proto.String(actor.String())}, Text: proto.String("💚")}}})
	adapter.event(context.Background(), nil, &events.Message{Info: types.MessageInfo{MessageSource: source, ID: "target", Timestamp: time.UnixMilli(3000)}, IsEdit: true, Message: &waE2E.Message{Conversation: proto.String("edited by original sender")}})
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345-67890@g.us"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	var target *app.Message
	for i := range page.Messages {
		if page.Messages[i].ID == "target" {
			target = &page.Messages[i]
		}
	}
	if target == nil || target.Body != "edited by original sender" || !target.Edited || target.Reactions == nil || len(*target.Reactions) != 1 || (*target.Reactions)[0].Emoji != "💚" || (*target.Reactions)[0].Provenance != "remote" {
		t.Fatalf("protocol updates were not projected: %#v", target)
	}
	typ := waE2E.ProtocolMessage_REVOKE
	adapter.event(context.Background(), nil, &events.Message{Info: types.MessageInfo{MessageSource: source, ID: "revoke-event", Timestamp: time.UnixMilli(4000)}, Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: &typ, Key: &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String("target"), FromMe: proto.Bool(false), Participant: proto.String(actor.String())}}}})
	result, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345-67890@g.us"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(result)
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if message.ID == "target" && (!message.Deleted || message.MutationProvenance != "remote") {
			t.Fatalf("revoke not persisted: %#v", message)
		}
	}
}
