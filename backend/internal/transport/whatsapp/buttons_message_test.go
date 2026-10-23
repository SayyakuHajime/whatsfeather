package whatsapp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func protocolDisplayButton(label, id, params string) *waE2E.ButtonsMessage_Button {
	buttonType := waE2E.ButtonsMessage_Button_NATIVE_FLOW
	return &waE2E.ButtonsMessage_Button{
		ButtonID:   proto.String(id),
		ButtonText: &waE2E.ButtonsMessage_Button_ButtonText{DisplayText: proto.String(label)},
		Type:       &buttonType,
		NativeFlowInfo: &waE2E.ButtonsMessage_Button_NativeFlowInfo{
			Name:       proto.String("private-flow-name"),
			ParamsJSON: proto.String(params),
		},
	}
}

func validProtocolButtons() *waE2E.ButtonsMessage {
	return &waE2E.ButtonsMessage{
		Header:      &waE2E.ButtonsMessage_Text{Text: "<b>Header</b>"},
		ContentText: proto.String("<script>Literal body</script>"),
		FooterText:  proto.String("<i>Footer</i>"),
		Buttons: []*waE2E.ButtonsMessage_Button{
			protocolDisplayButton("<img src=x>", "button-id-secret", `{"url":"https://private.invalid/action"}`),
			protocolDisplayButton("A & B", "another-button-id", `{"secret":"params-secret"}`),
		},
		ContextInfo: &waE2E.ContextInfo{
			Participant: proto.String("context-secret"),
			QuotedMessage: &waE2E.Message{
				Conversation: proto.String("quoted-context-secret"),
			},
		},
	}
}

func reflectedButtonsProjection(t *testing.T, content extractedContent) reflect.Value {
	t.Helper()
	field := reflect.ValueOf(content).FieldByName("buttons")
	if !field.IsValid() || field.Kind() != reflect.Pointer || field.IsNil() {
		t.Fatalf("incoming ButtonsMessage lacks its typed read-only projection")
	}
	return field.Elem()
}

func reflectedText(t *testing.T, projection reflect.Value, name string) string {
	t.Helper()
	field := projection.FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.String {
		t.Fatalf("ButtonsMessage projection lacks %s text", name)
	}
	return field.String()
}

func reflectedLabels(t *testing.T, projection reflect.Value) []string {
	t.Helper()
	field := projection.FieldByName("Buttons")
	if !field.IsValid() || field.Kind() != reflect.Slice || field.Type().Elem().Kind() != reflect.String {
		t.Fatalf("ButtonsMessage projection lacks display-label slice")
	}
	labels := make([]string, field.Len())
	for i := range labels {
		labels[i] = field.Index(i).String()
	}
	return labels
}

func TestIncomingButtonsMessageUsesTypedDisplayOnlyProjection(t *testing.T) {
	protocolButtons := validProtocolButtons()
	content := messageContent(&events.Message{Message: &waE2E.Message{ButtonsMessage: protocolButtons}})
	if content.kind != "buttons" || content.body != "<script>Literal body</script>" {
		t.Fatalf("incoming ButtonsMessage was not mapped to typed content: %#v", content)
	}
	projection := reflectedButtonsProjection(t, content)
	if reflectedText(t, projection, "Header") != "<b>Header</b>" ||
		reflectedText(t, projection, "Body") != "<script>Literal body</script>" ||
		reflectedText(t, projection, "Footer") != "<i>Footer</i>" ||
		!reflect.DeepEqual(reflectedLabels(t, projection), []string{"<img src=x>", "A & B"}) {
		t.Fatalf("ButtonsMessage display fields were not projected exactly")
	}
	for name, message := range map[string]*waE2E.ButtonsMessage{
		"absent-header":    {ContentText: proto.String("Body only"), Buttons: []*waE2E.ButtonsMessage_Button{protocolDisplayButton("Go", "secret", "")}},
		"text-header-only": {Header: &waE2E.ButtonsMessage_Text{Text: "Header only"}, Buttons: []*waE2E.ButtonsMessage_Button{protocolDisplayButton("Go", "secret", "")}},
	} {
		t.Run(name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: &waE2E.Message{ButtonsMessage: message}})
			if content.kind != "buttons" {
				t.Fatalf("supported %s ButtonsMessage was rejected: %#v", name, content)
			}
			reflectedButtonsProjection(t, content)
		})
	}
}

func protocolResponseButton(label, id string) *waE2E.ButtonsMessage_Button {
	buttonType := waE2E.ButtonsMessage_Button_RESPONSE
	return &waE2E.ButtonsMessage_Button{
		ButtonID:   proto.String(id),
		ButtonText: &waE2E.ButtonsMessage_Button_ButtonText{DisplayText: proto.String(label)},
		Type:       &buttonType,
	}
}

func TestIncomingButtonsResponseActionsStayPrivateAndFailClosedForOtherTypes(t *testing.T) {
	protocolButtons := &waE2E.ButtonsMessage{
		ContentText: proto.String("Choose"),
		Buttons: []*waE2E.ButtonsMessage_Button{
			protocolResponseButton("First", "private-first"),
			protocolResponseButton("Second", "private-second"),
		},
	}
	content := messageContent(&events.Message{Message: &waE2E.Message{ButtonsMessage: protocolButtons}})
	if content.kind != "buttons" || len(content.buttonActionRows) != 2 ||
		content.buttonActionRows[0].ButtonIndex != 0 || content.buttonActionRows[0].ButtonID != "private-first" ||
		content.buttonActionRows[1].ButtonIndex != 1 || content.buttonActionRows[1].ButtonID != "private-second" {
		t.Fatalf("response button mappings = %#v", content.buttonActionRows)
	}
	projectionJSON, err := json.Marshal(content.buttons)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projectionJSON), "private-") || strings.Contains(string(projectionJSON), "button_id") {
		t.Fatalf("display projection contains private action data: %s", projectionJSON)
	}

	for name, buttons := range map[string]*waE2E.ButtonsMessage{
		"native-flow": validProtocolButtons(),
		"mixed-types": {ContentText: proto.String("Choose"), Buttons: []*waE2E.ButtonsMessage_Button{
			protocolResponseButton("First", "private-first"), protocolDisplayButton("Open", "private-flow", "{}"),
		}},
		"missing-id": {ContentText: proto.String("Choose"), Buttons: []*waE2E.ButtonsMessage_Button{protocolResponseButton("First", "")}},
	} {
		t.Run(name, func(t *testing.T) {
			got := messageContent(&events.Message{Message: &waE2E.Message{ButtonsMessage: buttons}})
			if got.kind != "buttons" || len(got.buttonActionRows) != 0 {
				t.Fatalf("unsupported/incomplete button mapping did not fail closed: %#v", got)
			}
		})
	}
}

func TestIncomingButtonReplyMappingPersistsPrivatelyAndHydratesAvailability(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, err := types.ParseJID("synthetic-button-replies@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	protocolButtons := &waE2E.ButtonsMessage{
		ContentText: proto.String("Choose"),
		Buttons: []*waE2E.ButtonsMessage_Button{
			protocolResponseButton("First", "private-button-first"),
			protocolResponseButton("Second", "private-button-second"),
		},
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "response-buttons", Timestamp: time.Unix(1, 0)},
		Message: &waE2E.Message{ButtonsMessage: protocolButtons},
	}, "Synthetic buttons")
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-button-replies@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-button-") || !strings.Contains(string(encoded), `"button_reply_available":true`) {
		t.Fatalf("hydrated ButtonsMessage leaked IDs or omitted reply availability: %s", encoded)
	}
}

func TestIncomingButtonsProjectionPersistsWithoutProtocolActions(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	chat, err := types.ParseJID("synthetic-buttons@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "valid-buttons", Timestamp: time.Unix(1, 0)},
		Message: &waE2E.Message{ButtonsMessage: validProtocolButtons()},
	}, "Synthetic buttons")
	invalid := validProtocolButtons()
	invalid.Buttons[0].ButtonText.DisplayText = proto.String(string([]byte{0xff}))
	adapter.record(context.Background(), &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "invalid-buttons", Timestamp: time.Unix(2, 0)},
		Message: &waE2E.Message{ButtonsMessage: invalid},
	}, "Synthetic buttons")
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-buttons@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"button-id-secret", "another-button-id", "private-flow-name", "params-secret", "private.invalid", "context-secret", "quoted-context-secret", "NativeFlowInfo", "ParamsJSON"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("message.list leaked ButtonsMessage protocol data %q: %s", forbidden, encoded)
		}
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != "valid-buttons" || page.Messages[0].Kind != "buttons" {
		t.Fatalf("valid ButtonsMessage was not returned as typed content: %s", encoded)
	}
	projection := reflect.ValueOf(page.Messages[0]).FieldByName("Buttons")
	if !projection.IsValid() || projection.IsNil() {
		t.Fatalf("message.list did not hydrate typed ButtonsMessage content: %s", encoded)
	}
	if page.Messages[1].ID != "invalid-buttons" || page.Messages[1].Kind != "unsupported" || page.Messages[1].Body != "[Unsupported content — open the official app]" {
		t.Fatalf("invalid ButtonsMessage did not fail closed atomically: %s", encoded)
	}
}

func TestInvalidButtonsMessageFailsClosed(t *testing.T) {
	const fallback = "[Unsupported content — open the official app]"
	valid := validProtocolButtons()
	clone := func() *waE2E.ButtonsMessage { return proto.Clone(valid).(*waE2E.ButtonsMessage) }
	cases := []struct {
		name    string
		message *waE2E.ButtonsMessage
	}{
		{"no-buttons", func() *waE2E.ButtonsMessage { m := clone(); m.Buttons = nil; return m }()},
		{"too-many-buttons", func() *waE2E.ButtonsMessage {
			m := clone()
			m.Buttons = []*waE2E.ButtonsMessage_Button{m.Buttons[0], m.Buttons[1], m.Buttons[0], m.Buttons[1]}
			return m
		}()},
		{"nil-button", func() *waE2E.ButtonsMessage { m := clone(); m.Buttons = []*waE2E.ButtonsMessage_Button{nil}; return m }()},
		{"nil-text-header", func() *waE2E.ButtonsMessage { m := clone(); m.Header = (*waE2E.ButtonsMessage_Text)(nil); return m }()},
		{"unknown-header-type", func() *waE2E.ButtonsMessage {
			m := clone()
			m.HeaderType = waE2E.ButtonsMessage_HeaderType(99).Enum()
			return m
		}()},
		{"missing-button-text", func() *waE2E.ButtonsMessage {
			m := clone()
			m.Buttons = []*waE2E.ButtonsMessage_Button{{ButtonID: proto.String("secret")}}
			return m
		}()},
		{"empty-button-text", func() *waE2E.ButtonsMessage {
			m := clone()
			m.Buttons = []*waE2E.ButtonsMessage_Button{protocolDisplayButton("  ", "secret", "")}
			return m
		}()},
		{"invalid-utf8", func() *waE2E.ButtonsMessage {
			m := clone()
			m.ContentText = proto.String(string([]byte{0xff}))
			return m
		}()},
		{"control-character", func() *waE2E.ButtonsMessage {
			m := clone()
			m.FooterText = proto.String("line one\nline two")
			return m
		}()},
		{"overlong-label", func() *waE2E.ButtonsMessage {
			m := clone()
			m.Buttons[0].ButtonText.DisplayText = proto.String(strings.Repeat("x", 257))
			return m
		}()},
		{"overlong-body", func() *waE2E.ButtonsMessage {
			m := clone()
			m.ContentText = proto.String(strings.Repeat("x", 1025))
			return m
		}()},
		{"empty-header-and-body", &waE2E.ButtonsMessage{Buttons: []*waE2E.ButtonsMessage_Button{protocolDisplayButton("One", "secret", "")}}},
		{"document-header", &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_DocumentMessage{DocumentMessage: &waE2E.DocumentMessage{}}, ContentText: proto.String("Body"), Buttons: valid.Buttons}},
		{"image-header", &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_ImageMessage{ImageMessage: &waE2E.ImageMessage{}}, ContentText: proto.String("Body"), Buttons: valid.Buttons}},
		{"video-header", &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_VideoMessage{VideoMessage: &waE2E.VideoMessage{}}, ContentText: proto.String("Body"), Buttons: valid.Buttons}},
		{"location-header", &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_LocationMessage{LocationMessage: &waE2E.LocationMessage{}}, ContentText: proto.String("Body"), Buttons: valid.Buttons}},
		{"explicit-media-header-type", func() *waE2E.ButtonsMessage {
			m := clone()
			headerType := waE2E.ButtonsMessage_IMAGE
			m.HeaderType = &headerType
			return m
		}()},
		{"oversized-serialized-projection", &waE2E.ButtonsMessage{Header: &waE2E.ButtonsMessage_Text{Text: strings.Repeat("🌿", 256)}, ContentText: proto.String(strings.Repeat("🌿", 800)), FooterText: proto.String(strings.Repeat("🌿", 128)), Buttons: []*waE2E.ButtonsMessage_Button{protocolDisplayButton(strings.Repeat("🌿", 100), "secret1", ""), protocolDisplayButton(strings.Repeat("🌿", 100), "secret2", ""), protocolDisplayButton(strings.Repeat("🌿", 100), "secret3", "")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := messageContent(&events.Message{Message: &waE2E.Message{ButtonsMessage: tc.message}})
			if content.kind != "unsupported" || content.body != fallback {
				t.Fatalf("invalid ButtonsMessage did not fail closed: %#v", content)
			}
		})
	}
}
