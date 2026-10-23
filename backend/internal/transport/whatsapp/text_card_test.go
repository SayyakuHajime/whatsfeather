package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func safeInteractive() *waE2E.InteractiveMessage {
	return &waE2E.InteractiveMessage{
		Header: &waE2E.InteractiveMessage_Header{Title: proto.String("Title"), Subtitle: proto.String("Subtitle")}, Body: &waE2E.InteractiveMessage_Body{Text: proto.String("Body")}, Footer: &waE2E.InteractiveMessage_Footer{Text: proto.String("Footer")},
		InteractiveMessage: &waE2E.InteractiveMessage_NativeFlowMessage_{NativeFlowMessage: &waE2E.InteractiveMessage_NativeFlowMessage{MessageParamsJSON: proto.String("SECRET_PARAMS"), Buttons: []*waE2E.InteractiveMessage_NativeFlowMessage_NativeFlowButton{{Name: proto.String("SECRET_NAME"), ButtonParamsJSON: proto.String("SECRET_BUTTON")}}}},
		ContextInfo:        &waE2E.ContextInfo{Participant: proto.String("Synthetic sender"), QuotedMessage: &waE2E.Message{Conversation: proto.String("Quoted preview")}}}
}
func TestInteractiveTextCard(t *testing.T) {
	c := messageContent(&events.Message{Message: &waE2E.Message{InteractiveMessage: safeInteractive()}})
	if c.kind != "interactive" {
		t.Fatalf("lost interactive kind: %#v", c)
	}
	f := reflect.ValueOf(c).FieldByName("interactive")
	if !f.IsValid() || f.IsNil() {
		t.Fatal("missing safe projection")
	}
	if c.quotedBody != "Quoted preview" || c.quotedSender != "Synthetic sender" {
		t.Fatal("quote context lost")
	}
}
func safeTemplate() *waE2E.TemplateMessage {
	return &waE2E.TemplateMessage{TemplateID: proto.String("SECRET_TEMPLATE"), HydratedTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{
		Title: &waE2E.TemplateMessage_HydratedFourRowTemplate_HydratedTitleText{HydratedTitleText: "Title"}, HydratedContentText: proto.String("Body"), HydratedFooterText: proto.String("Footer"), TemplateID: proto.String("SECRET_INNER"), MaskLinkedDevices: proto.Bool(true),
		HydratedButtons: []*waE2E.HydratedTemplateButton{
			{HydratedButton: &waE2E.HydratedTemplateButton_QuickReplyButton{QuickReplyButton: &waE2E.HydratedTemplateButton_HydratedQuickReplyButton{DisplayText: proto.String("Reply label"), ID: proto.String("SECRET_ID")}}},
			{HydratedButton: &waE2E.HydratedTemplateButton_UrlButton{UrlButton: &waE2E.HydratedTemplateButton_HydratedURLButton{DisplayText: proto.String("URL label"), URL: proto.String("SECRET_URL"), ConsentedUsersURL: proto.String("SECRET_CONSENT")}}},
			{HydratedButton: &waE2E.HydratedTemplateButton_CallButton{CallButton: &waE2E.HydratedTemplateButton_HydratedCallButton{DisplayText: proto.String("Call label"), PhoneNumber: proto.String("SECRET_PHONE")}}},
		}}}
}
func TestTemplateTextCard(t *testing.T) {
	m := safeTemplate()
	m.ContextInfo = safeInteractive().ContextInfo
	c := messageContent(&events.Message{Message: &waE2E.Message{TemplateMessage: m}})
	if c.kind != "template" {
		t.Fatalf("lost template kind: %#v", c)
	}
	f := reflect.ValueOf(c).FieldByName("template")
	if !f.IsValid() || f.IsNil() {
		t.Fatal("missing template projection")
	}
	if c.quotedBody != "Quoted preview" || c.quotedSender != "Synthetic sender" {
		t.Fatal("template quote path changed")
	}
}
func TestTextCardsAdapterSQLiteRPC(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, e := app.Open(profile, false)
	if e != nil {
		t.Fatal(e)
	}
	chat := types.NewJID("synthetic-text-cards", types.DefaultUserServer)
	adapter := New(a, t.TempDir())
	for i, m := range []*waE2E.Message{{InteractiveMessage: safeInteractive()}, {TemplateMessage: safeTemplate()}} {
		adapter.record(context.Background(), &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: []string{"interactive", "template"}[i], Timestamp: time.Unix(int64(i+1), 0)}, Message: m}, "Synthetic")
	}
	a.Close()
	a, e = app.Open(profile, false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-text-cards@s.whatsapp.net","limit":10}`))
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(b)
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if e = json.Unmarshal(encoded, &page); e != nil {
		t.Fatal(e)
	}
	if len(page.Messages) != 2 || page.Messages[0].Interactive == nil || page.Messages[1].Template == nil {
		t.Fatal("typed cards lost", string(encoded))
	}
	want := app.TextCardProjection{Title: "Title", Subtitle: "Subtitle", Body: "Body", Footer: "Footer"}
	if !reflect.DeepEqual(*page.Messages[0].Interactive, want) {
		t.Fatal("interactive fields changed")
	}
	want.Subtitle = ""
	want.Labels = []string{"Reply label", "URL label", "Call label"}
	if !reflect.DeepEqual(*page.Messages[1].Template, want) {
		t.Fatal("template fields changed")
	}
	if page.Messages[0].QuotedBody != "Quoted preview" {
		t.Fatal("stored quote lost")
	}
	a.Close()
	disk, e := os.ReadFile(filepath.Join(profile, "history.db"))
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"SECRET_PARAMS", "SECRET_NAME", "SECRET_BUTTON", "SECRET_TEMPLATE", "SECRET_INNER", "SECRET_ID", "SECRET_URL", "SECRET_CONSENT", "SECRET_PHONE"} {
		if strings.Contains(string(encoded), secret) || bytes.Contains(disk, []byte(secret)) {
			t.Fatal("action metadata leaked", secret)
		}
	}
}
func TestTextCardRejectsUnknownButtonPayload(t *testing.T) {
	m := safeTemplate()
	m.HydratedTemplate.HydratedButtons[0].GetQuickReplyButton().ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if c := messageContent(&events.Message{Message: &waE2E.Message{TemplateMessage: m}}); c.kind != "unsupported" {
		t.Fatal("unknown button payload accepted")
	}
}
func TestTextCardUnsupportedVariants(t *testing.T) {
	interactive := []struct {
		name   string
		change func(*waE2E.InteractiveMessage)
	}{
		{"header-media", func(m *waE2E.InteractiveMessage) { m.Header.Media = &waE2E.InteractiveMessage_Header_ImageMessage{} }},
		{"attachment-header", func(m *waE2E.InteractiveMessage) { m.Header.HasMediaAttachment = proto.Bool(true) }},
		{"attachment-footer", func(m *waE2E.InteractiveMessage) { m.Footer.HasMediaAttachment = proto.Bool(true) }},
		{"footer-media", func(m *waE2E.InteractiveMessage) { m.Footer.Media = &waE2E.InteractiveMessage_Footer_AudioMessage{} }},
		{"header-bloks", func(m *waE2E.InteractiveMessage) { m.Header.BloksWidget = &waE2E.InteractiveMessage_BloksWidget{} }},
		{"bloks", func(m *waE2E.InteractiveMessage) { m.BloksWidget = &waE2E.InteractiveMessage_BloksWidget{} }},
		{"carousel", func(m *waE2E.InteractiveMessage) { m.InteractiveMessage = &waE2E.InteractiveMessage_CarouselMessage_{} }},
		{"collection", func(m *waE2E.InteractiveMessage) {
			m.InteractiveMessage = &waE2E.InteractiveMessage_CollectionMessage_{}
		}},
		{"shop", func(m *waE2E.InteractiveMessage) {
			m.InteractiveMessage = &waE2E.InteractiveMessage_ShopStorefrontMessage{}
		}},
		{"nil-wrapper", func(m *waE2E.InteractiveMessage) {
			m.InteractiveMessage = (*waE2E.InteractiveMessage_NativeFlowMessage_)(nil)
		}},
		{"missing-native", func(m *waE2E.InteractiveMessage) {
			m.InteractiveMessage = &waE2E.InteractiveMessage_NativeFlowMessage_{}
		}},
		{"unknown", func(m *waE2E.InteractiveMessage) { m.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }},
		{"utf8", func(m *waE2E.InteractiveMessage) { m.Body.Text = proto.String(string([]byte{255})) }},
		{"control", func(m *waE2E.InteractiveMessage) { m.Header.Subtitle = proto.String("a\u0085b") }},
		{"oversize", func(m *waE2E.InteractiveMessage) { m.Footer.Text = proto.String(strings.Repeat("x", 4097)) }},
		{"json-size", func(m *waE2E.InteractiveMessage) { m.Body.Text = proto.String(strings.Repeat("<", 1000)) }},
		{"empty", func(m *waE2E.InteractiveMessage) { m.Header = nil; m.Footer = nil; m.Body.Text = proto.String("  ") }},
	}
	for _, tc := range interactive {
		t.Run("interactive-"+tc.name, func(t *testing.T) {
			m := safeInteractive()
			tc.change(m)
			c := messageContent(&events.Message{Message: &waE2E.Message{InteractiveMessage: m}})
			if c.kind != "unsupported" || c.interactive != nil || c.body != unsupportedContent().body {
				t.Fatalf("not unsupported: %#v", c)
			}
			nested := &waE2E.TemplateMessage{Format: &waE2E.TemplateMessage_InteractiveMessageTemplate{InteractiveMessageTemplate: m}}
			if c = messageContent(&events.Message{Message: &waE2E.Message{TemplateMessage: nested}}); c.kind != "unsupported" || c.template != nil {
				t.Fatal("unsafe nested interactive accepted")
			}
		})
	}
	templates := []struct {
		name   string
		change func(*waE2E.TemplateMessage)
	}{
		{"document", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_DocumentMessage{}
		}},
		{"image", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_ImageMessage{}
		}},
		{"video", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_VideoMessage{}
		}},
		{"location", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.Title = &waE2E.TemplateMessage_HydratedFourRowTemplate_LocationMessage{}
		}},
		{"missing-title", func(m *waE2E.TemplateMessage) { m.HydratedTemplate.Title = nil }},
		{"nil-title", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.Title = (*waE2E.TemplateMessage_HydratedFourRowTemplate_HydratedTitleText)(nil)
		}},
		{"legacy", func(m *waE2E.TemplateMessage) {
			m.Format = &waE2E.TemplateMessage_FourRowTemplate_{FourRowTemplate: &waE2E.TemplateMessage_FourRowTemplate{}}
		}},
		{"nil-format", func(m *waE2E.TemplateMessage) { m.Format = (*waE2E.TemplateMessage_HydratedFourRowTemplate_)(nil) }},
		{"missing-format", func(m *waE2E.TemplateMessage) { m.HydratedTemplate = nil }},
		{"nil-button", func(m *waE2E.TemplateMessage) { m.HydratedTemplate.HydratedButtons[0] = nil }},
		{"missing-button", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.HydratedButtons[0] = &waE2E.HydratedTemplateButton{}
		}},
		{"nil-button-wrapper", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.HydratedButtons[0].HydratedButton = (*waE2E.HydratedTemplateButton_UrlButton)(nil)
		}},
		{"missing-button-payload", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.HydratedButtons[0].HydratedButton = &waE2E.HydratedTemplateButton_CallButton{}
		}},
		{"missing-label", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.HydratedButtons[0].GetQuickReplyButton().DisplayText = nil
		}},
		{"blank-label", func(m *waE2E.TemplateMessage) {
			m.HydratedTemplate.HydratedButtons[0].GetQuickReplyButton().DisplayText = proto.String("  ")
		}},
		{"five-buttons", func(m *waE2E.TemplateMessage) {
			b := m.HydratedTemplate.HydratedButtons
			m.HydratedTemplate.HydratedButtons = append(b, b[0], b[0])
		}},
		{"unknown", func(m *waE2E.TemplateMessage) { m.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }},
	}
	for _, tc := range templates {
		t.Run("template-"+tc.name, func(t *testing.T) {
			m := safeTemplate()
			tc.change(m)
			c := messageContent(&events.Message{Message: &waE2E.Message{TemplateMessage: m}})
			if c.kind != "unsupported" || c.template != nil {
				t.Fatalf("not unsupported: %#v", c)
			}
		})
	}
}
func TestTextCardAllowedContainers(t *testing.T) {
	m := safeInteractive()
	m.InteractiveMessage = nil
	if c := messageContent(&events.Message{Message: &waE2E.Message{InteractiveMessage: m}}); c.kind != "interactive" {
		t.Fatal("plain container rejected")
	}
	h := safeTemplate().HydratedTemplate
	h.HydratedButtons = append(h.HydratedButtons, h.HydratedButtons[0])
	for _, m := range []*waE2E.TemplateMessage{{Format: &waE2E.TemplateMessage_HydratedFourRowTemplate_{HydratedFourRowTemplate: h}}, {Format: &waE2E.TemplateMessage_InteractiveMessageTemplate{InteractiveMessageTemplate: m}}} {
		if c := messageContent(&events.Message{Message: &waE2E.Message{TemplateMessage: m}}); c.kind != "template" || c.template == nil {
			t.Fatal("safe template rejected")
		}
	}
	if projectedInteractiveContent(nil).kind != "unsupported" || projectedTemplateContent(nil).kind != "unsupported" {
		t.Fatal("nil input accepted")
	}
}
