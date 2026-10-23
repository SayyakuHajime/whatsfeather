package whatsapp

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestActionBuildersSerializeProtocolReplyReactionEditRevokeAndForward(t *testing.T) {
	builders := []struct {
		name  string
		build func() (*waE2E.Message, error)
	}{
		{"reply", func() (*waE2E.Message, error) {
			return buildReplyMessage("12345-67890@g.us", "quoted-1", false, "12345@s.whatsapp.net", "answer", "quoted text")
		}},
		{"reaction", func() (*waE2E.Message, error) {
			return buildReactionMessage("12345-67890@g.us", "quoted-1", false, "12345@s.whatsapp.net", "🔥")
		}},
		{"edit", func() (*waE2E.Message, error) { return buildEditMessage("12345@s.whatsapp.net", "mine-1", "edited") }},
		{"revoke", func() (*waE2E.Message, error) { return buildRevokeMessage("12345@s.whatsapp.net", "mine-1") }},
		{"forward", func() (*waE2E.Message, error) { return buildForwardMessage("12345@s.whatsapp.net", "forwarded text") }},
	}
	for _, test := range builders {
		t.Run(test.name, func(t *testing.T) {
			message, err := test.build()
			if err != nil {
				t.Fatal(err)
			}
			wire, err := proto.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			var decoded waE2E.Message
			if err = proto.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			switch test.name {
			case "reply":
				info := decoded.GetExtendedTextMessage().GetContextInfo()
				if info.GetStanzaID() != "quoted-1" || info.GetParticipant() != "12345@s.whatsapp.net" || info.GetQuotedMessage().GetConversation() != "quoted text" {
					t.Fatalf("serialized reply context missing: %s", &decoded)
				}
			case "reaction":
				key := decoded.GetReactionMessage().GetKey()
				if decoded.GetReactionMessage().GetText() != "🔥" || key.GetID() != "quoted-1" || key.GetRemoteJID() != "12345-67890@g.us" || key.GetFromMe() {
					t.Fatalf("serialized reaction key/text wrong: %s", &decoded)
				}
			case "edit":
				protocol := decoded.GetProtocolMessage()
				if protocol.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || protocol.GetKey().GetID() != "mine-1" || protocol.GetEditedMessage().GetConversation() != "edited" {
					t.Fatalf("serialized edit protocol wrong: %s", &decoded)
				}
			case "revoke":
				protocol := decoded.GetProtocolMessage()
				if protocol.GetType() != waE2E.ProtocolMessage_REVOKE || protocol.GetKey().GetID() != "mine-1" || !protocol.GetKey().GetFromMe() {
					t.Fatalf("serialized revoke protocol wrong: %s", &decoded)
				}
			case "forward":
				info := decoded.GetExtendedTextMessage().GetContextInfo()
				if decoded.GetExtendedTextMessage().GetText() != "forwarded text" || !info.GetIsForwarded() || info.GetForwardingScore() != 1 {
					t.Fatalf("serialized forward context wrong: %s", &decoded)
				}
			}
		})
	}
}
