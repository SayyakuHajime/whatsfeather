package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestLocalDisconnectClosesStoreHandleButPreservesLinkedDevice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database := "file:" + filepath.ToSlash(filepath.Join(dir, "device.db")) + "?_pragma=foreign_keys(1)"
	store, err := sqlstore.New(ctx, "sqlite", database, nil)
	if err != nil {
		t.Fatal(err)
	}
	device, err := store.GetFirstDevice(ctx)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	wantJID, err := types.ParseJID("15550147@s.whatsapp.net")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	device.ID = &wantJID
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte("synthetic test device; never connected"),
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err = device.Save(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(device, nil)
	adapter := New(nil, dir)
	clientCtx, cancel := context.WithCancel(ctx)
	adapter.client = client
	adapter.store = store
	adapter.cancel = cancel

	adapter.Disconnect()
	if clientCtx.Err() != context.Canceled {
		t.Fatalf("local disconnect did not cancel adapter context: %v", clientCtx.Err())
	}
	if adapter.current(client) || adapter.client != nil || adapter.store != nil || adapter.cancel != nil {
		t.Fatal("local disconnect retained active adapter handles")
	}

	reopened, err := sqlstore.New(ctx, "sqlite", database, nil)
	if err != nil {
		t.Fatalf("device store did not reopen after local disconnect: %v", err)
	}
	defer reopened.Close()
	retained, err := reopened.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retained.ID == nil || retained.ID.String() != wantJID.String() {
		t.Fatalf("local disconnect removed or changed linked-device identity: got %v, want %v", retained.ID, wantJID)
	}
}

func TestMessageContentProjectsIncomingContactAndContactsArray(t *testing.T) {
	one := messageContent(&events.Message{Message: &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
		DisplayName: proto.String("Untrusted protocol label"),
		Vcard:       proto.String("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Ada Lovelace\r\nTEL;TYPE=CELL;waid=15551234567:+15551234567\r\nEND:VCARD\r\n"),
	}}})
	if one.kind != "contact" || len(one.contacts) != 1 || one.contacts[0].Name != "Ada Lovelace" ||
		len(one.contacts[0].Phones) != 1 || one.contacts[0].Phones[0] != "+15551234567" {
		t.Fatalf("single contact was not projected from the vCard: %#v", one)
	}

	array := messageContent(&events.Message{Message: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
		Contacts: []*waE2E.ContactMessage{
			{DisplayName: proto.String("First"), Vcard: proto.String("BEGIN:VCARD\nFN:First\nEND:VCARD\n")},
			{DisplayName: proto.String("Second"), Vcard: proto.String("BEGIN:VCARD\nN:Second;Grace;;;\nEND:VCARD\n")},
		},
	}}})
	if array.kind != "contact" || len(array.contacts) != 2 || array.contacts[0].Name != "First" || array.contacts[1].Name != "Grace Second" {
		t.Fatalf("contacts array was not projected atomically: %#v", array)
	}

	invalid := messageContent(&events.Message{Message: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{
		Contacts: []*waE2E.ContactMessage{
			{Vcard: proto.String("BEGIN:VCARD\nFN:Valid\nEND:VCARD\n")},
			{Vcard: proto.String("BEGIN:VCARD\nFN;ENCODING=QUOTED-PRINTABLE:invalid\nEND:VCARD\n")},
		},
	}}})
	if invalid.kind != "unsupported" || len(invalid.contacts) != 0 || strings.Contains(invalid.body, "invalid") {
		t.Fatalf("malformed contact array leaked partial or raw data: %#v", invalid)
	}
}

func TestIncomingContactRecordPersistsProjectionWithoutRawVCard(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := types.ParseJID("synthetic-contact@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "incoming-contact", Timestamp: time.Unix(2, 0)},
		Message: &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
			DisplayName: proto.String("Do not trust this label"),
			Vcard:       proto.String("BEGIN:VCARD\nFN:Linus Example\nTEL;waid=15551234567:+1 555 0100\nEND:VCARD\n"),
		}},
	}, "Synthetic contact")
	result, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-contact@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "BEGIN:VCARD") || strings.Contains(string(encoded), "waid") {
		t.Fatalf("message.list exposed raw contact metadata: %s", encoded)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != "contact" || page.Messages[0].ContactCards == nil ||
		len(*page.Messages[0].ContactCards) != 1 || (*page.Messages[0].ContactCards)[0].Name != "Linus Example" {
		t.Fatalf("incoming contact projection was not persisted: %#v", page.Messages)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err = a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-contact@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"name":"Linus Example"`) {
		t.Fatalf("incoming projection did not survive reopen: %s (%v)", encoded, err)
	}
}

type contactEnvelopeProbe struct {
	message *waE2E.Message
	chat    types.JID
	id      string
}

func (p *contactEnvelopeProbe) IsConnected() bool { return true }
func (p *contactEnvelopeProbe) IsLoggedIn() bool  { return true }
func (p *contactEnvelopeProbe) SendMessage(_ context.Context, chat types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	p.chat, p.message = chat, message
	if len(extra) != 1 {
		return whatsmeow.SendResponse{}, nil
	}
	p.id = string(extra[0].ID)
	return whatsmeow.SendResponse{Timestamp: time.Unix(7, 0)}, nil
}

func TestTypedContactSendBuildsCanonicalSingleAndArrayEnvelopes(t *testing.T) {
	probe := &contactEnvelopeProbe{}
	cards := []app.ContactCard{{Name: "Ada, Example", Phones: []string{"+1 555 0100"}, Emails: []string{"ada@example.test"}, Organization: "Analytical Engines"}}
	stamp, err := sendContactsWithClient(context.Background(), probe, "15551234567@s.whatsapp.net", cards, "stable-contact-id")
	if err != nil || stamp != time.Unix(7, 0).UnixMilli() {
		t.Fatalf("single typed contact send = %d, %v", stamp, err)
	}
	if probe.message == nil || probe.message.GetContactMessage() == nil || probe.id != "stable-contact-id" || probe.chat.String() != "15551234567@s.whatsapp.net" {
		t.Fatalf("single contact did not use the typed protocol envelope and stable ID: %#v", probe)
	}
	vcard := probe.message.GetContactMessage().GetVcard()
	if strings.Contains(vcard, "waid") || strings.Contains(vcard, "@s.whatsapp.net") || !strings.Contains(vcard, "FN:Ada\\, Example") {
		t.Fatalf("single outgoing vCard was not canonical/minimal: %q", vcard)
	}
	parsed, err := app.ParseVCard([]byte(vcard))
	if err != nil || len(parsed) != 1 || parsed[0].Name != cards[0].Name {
		t.Fatalf("canonical outgoing vCard did not reconstruct the parsed projection: %#v, %v", parsed, err)
	}

	cards = append(cards, app.ContactCard{Name: "Grace Hopper"})
	if _, err = sendContactsWithClient(context.Background(), probe, "15551234567@s.whatsapp.net", cards, "stable-array-id"); err != nil {
		t.Fatal(err)
	}
	if probe.message == nil || probe.message.GetContactsArrayMessage() == nil || len(probe.message.GetContactsArrayMessage().GetContacts()) != 2 || probe.id != "stable-array-id" {
		t.Fatalf("multiple contacts did not use a typed array envelope: %#v", probe)
	}
	for _, item := range probe.message.GetContactsArrayMessage().GetContacts() {
		if strings.Contains(item.GetVcard(), "waid") || strings.Contains(item.GetVcard(), "@s.whatsapp.net") {
			t.Fatalf("array vCard leaked protocol metadata: %q", item.GetVcard())
		}
	}
}

func TestTypedMediaCaptionsAndMetadataPersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	jid, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	imageHash := []byte("image-hash")
	events := []*events.Message{
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "image", Timestamp: time.Unix(1, 0)}, Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("image caption"), Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(42), FileSHA256: imageHash, DirectPath: proto.String("/m/image"), MediaKey: []byte("secret")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "sticker", Timestamp: time.Unix(2, 0)}, Message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(7), DirectPath: proto.String("/m/sticker"), MediaKey: []byte("sticker-secret")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "document", Timestamp: time.Unix(3, 0)}, Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("document caption"), FileName: proto.String("report.pdf"), Title: proto.String("Report"), Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(99), DirectPath: proto.String("/m/document"), MediaKey: []byte("document-secret")}}},
	}
	for _, evt := range events {
		adapter.record(context.Background(), evt, "Known contact")
	}
	check := func() {
		value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		var page struct {
			Messages []struct {
				ID         string `json:"id"`
				Body       string `json:"body"`
				Kind       string `json:"kind"`
				Attachment *struct {
					Name  string `json:"name"`
					Size  int64  `json:"size"`
					MIME  string `json:"mime"`
					State string `json:"state"`
					URL   string `json:"url"`
				} `json:"attachment"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		got := map[string]struct {
			body, kind, name, mime, state, url string
			size                               int64
		}{}
		for _, m := range page.Messages {
			if m.Attachment != nil {
				got[m.ID] = struct {
					body, kind, name, mime, state, url string
					size                               int64
				}{m.Body, m.Kind, m.Attachment.Name, m.Attachment.MIME, m.Attachment.State, m.Attachment.URL, m.Attachment.Size}
			} else {
				got[m.ID] = struct {
					body, kind, name, mime, state, url string
					size                               int64
				}{body: m.Body, kind: m.Kind}
			}
		}
		if got["image"].body != "image caption" || got["image"].kind != "image" || got["image"].mime != "image/jpeg" || got["image"].state != "remote" || got["image"].url != "" || got["image"].size != 42 {
			t.Fatalf("image content lost: %#v", got["image"])
		}
		if got["sticker"].kind != "sticker" || got["sticker"].state != "remote" {
			t.Fatalf("sticker content lost: %#v", got["sticker"])
		}
		if got["document"].body != "document caption" || got["document"].kind != "document" || got["document"].name != "report.pdf" {
			t.Fatalf("document content lost: %#v", got["document"])
		}
	}
	check()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	check()
}

func TestQuotedMessagePreviewUsesTextAndSafeMediaLabels(t *testing.T) {
	cases := []struct {
		name    string
		message *waE2E.Message
		want    string
	}{
		{name: "nil", want: ""},
		{name: "text", message: &waE2E.Message{Conversation: proto.String("quoted text")}, want: "quoted text"},
		{name: "caption", message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("image caption")}}, want: "image caption"},
		{name: "image", message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, want: "Photo"},
		{name: "sticker", message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, want: "Sticker"},
		{name: "video", message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}, want: "Video"},
		{name: "audio", message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, want: "Audio"},
		{name: "location", message: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}, want: "Location"},
		{name: "live-location", message: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{}}, want: "Location"},
		{name: "document-filename", message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("report.pdf")}}, want: "report.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotedMessagePreview(tc.message); got != tc.want {
				t.Fatalf("quotedMessagePreview() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMessageContentPreservesQuoteContextAcrossSupportedProtocolTypes(t *testing.T) {
	contextInfo := func() *waE2E.ContextInfo {
		return &waE2E.ContextInfo{
			StanzaID:      proto.String("quoted-target"),
			Participant:   proto.String("12345@s.whatsapp.net"),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("quoted body")},
		}
	}
	cases := []struct {
		name    string
		message *waE2E.Message
	}{
		{name: "live-location", message: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{ContextInfo: contextInfo()}}},
		{name: "contact", message: &waE2E.Message{ContactMessage: &waE2E.ContactMessage{ContextInfo: contextInfo()}}},
		{name: "contacts-array", message: &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{ContextInfo: contextInfo()}}},
		{name: "poll", message: &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}},
		{name: "poll-v2", message: &waE2E.Message{PollCreationMessageV2: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}},
		{name: "list", message: &waE2E.Message{ListMessage: &waE2E.ListMessage{ContextInfo: contextInfo()}}},
		{name: "interactive", message: &waE2E.Message{InteractiveMessage: &waE2E.InteractiveMessage{ContextInfo: contextInfo()}}},
		{name: "template", message: &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{ContextInfo: contextInfo()}}},
	}
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			_ = a.Close()
		}
	}()
	chat, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	wantIDs := make(map[string]struct{})
	for index, tc := range cases {
		content := messageContent(&events.Message{Message: tc.message})
		if content.quotedBody != "quoted body" || content.quotedSender != "12345@s.whatsapp.net" {
			t.Fatalf("quoted context lost for %s: %#v", tc.name, content)
		}
		id := "quote-context-" + tc.name
		wantIDs[id] = struct{}{}
		adapter.record(context.Background(), &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat},
				ID:            id,
				Timestamp:     time.Unix(int64(index+1), 0),
			},
			Message: tc.message,
		}, "Synthetic contact")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []struct {
			ID           string `json:"id"`
			QuotedBody   string `json:"quoted_body"`
			QuotedSender string `json:"quoted_sender"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if _, wanted := wantIDs[message.ID]; !wanted {
			continue
		}
		if message.QuotedBody != "quoted body" || message.QuotedSender != "12345@s.whatsapp.net" {
			t.Fatalf("SQLite reopen/message.list lost quoted context for %s: %#v", message.ID, message)
		}
		delete(wantIDs, message.ID)
	}
	if len(wantIDs) != 0 {
		t.Fatalf("quoted messages missing after SQLite reopen: %#v", wantIDs)
	}
}

func TestQuotedProtectedWrappersRemainUnavailable(t *testing.T) {
	chat, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want string
		quoted     *waE2E.Message
	}{
		{
			name: "view-once",
			want: "[View-once content unavailable]",
			quoted: &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("PROTECTED-SYNTHETIC")}},
			}},
		},
		{
			name: "disappearing",
			want: "[Disappearing message not retained]",
			quoted: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{Conversation: proto.String("PROTECTED-SYNTHETIC")},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()

			id := "reply-" + tc.name
			adapter := New(a, t.TempDir())
			event := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: chat},
					ID:            id,
					Timestamp:     time.Unix(20, 0),
				},
				Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String("Synthetic reply"),
					ContextInfo: &waE2E.ContextInfo{
						StanzaID:      proto.String("quoted-" + tc.name),
						QuotedMessage: tc.quoted,
					},
				}},
			}
			adapter.record(context.Background(), event, "Synthetic contact")

			value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Messages []struct {
					ID         string `json:"id"`
					QuotedBody string `json:"quoted_body"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(encoded, &page); err != nil {
				t.Fatal(err)
			}
			for _, message := range page.Messages {
				if message.ID != id {
					continue
				}
				if message.QuotedBody != tc.want {
					t.Fatalf("quoted_body = %q, want %q", message.QuotedBody, tc.want)
				}
				if strings.Contains(message.QuotedBody, "PROTECTED-SYNTHETIC") {
					t.Fatal("protected quoted content was unwrapped")
				}
				return
			}
			t.Fatal("reply message was not returned by message.list")
		})
	}
}

func TestQuotedNoCaptionImageUsesPhotoPreview(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			_ = a.Close()
		}
	}()

	chat, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	participant, err := types.ParseJID("67890@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	event := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat},
			ID:            "reply-to-photo",
			Timestamp:     time.Unix(10, 0),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("Nice shot"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String("quoted-photo"),
				Participant:   proto.String(participant.String()),
				QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
			},
		}},
	}
	adapter.record(context.Background(), event, "Synthetic contact")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}

	value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []struct {
			ID           string `json:"id"`
			QuotedBody   string `json:"quoted_body"`
			QuotedSender string `json:"quoted_sender"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if message.ID == "reply-to-photo" {
			if message.QuotedBody != "Photo" {
				t.Fatalf("quoted_body = %q, want %q", message.QuotedBody, "Photo")
			}
			if message.QuotedSender != participant.String() {
				t.Fatalf("quoted_sender = %q, want %q", message.QuotedSender, participant.String())
			}
			return
		}
	}
	t.Fatal("reply message was not returned by message.list")
}

type persistedDescriptorTransport struct {
	seen []string
}

func (t *persistedDescriptorTransport) Connect(context.Context) error { return nil }
func (t *persistedDescriptorTransport) Disconnect()                   {}
func (t *persistedDescriptorTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (t *persistedDescriptorTransport) DownloadMedia(_ context.Context, payload []byte, path string) error {
	var outer waE2E.Message
	if err := proto.Unmarshal(payload, &outer); err != nil {
		return err
	}
	var kind string
	var url, direct string
	var key []byte
	switch {
	case outer.ImageMessage != nil:
		kind, url, direct, key = "image", outer.ImageMessage.GetURL(), outer.ImageMessage.GetDirectPath(), outer.ImageMessage.GetMediaKey()
	case outer.StickerMessage != nil:
		kind, url, direct, key = "sticker", outer.StickerMessage.GetURL(), outer.StickerMessage.GetDirectPath(), outer.StickerMessage.GetMediaKey()
	case outer.AudioMessage != nil:
		kind, url, direct, key = "audio", outer.AudioMessage.GetURL(), outer.AudioMessage.GetDirectPath(), outer.AudioMessage.GetMediaKey()
	case outer.VideoMessage != nil:
		kind, url, direct, key = "video", outer.VideoMessage.GetURL(), outer.VideoMessage.GetDirectPath(), outer.VideoMessage.GetMediaKey()
	case outer.DocumentMessage != nil:
		kind, url, direct, key = "document", outer.DocumentMessage.GetURL(), outer.DocumentMessage.GetDirectPath(), outer.DocumentMessage.GetMediaKey()
	default:
		return os.ErrInvalid
	}
	if url == "" || direct == "" || len(key) == 0 {
		return os.ErrInvalid
	}
	t.seen = append(t.seen, kind+":"+url+":"+direct+":"+string(key))
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		return err
	}
	return os.WriteFile(path, imageData.Bytes(), 0600)
}

func TestPersistedMediaDescriptorsAreOuterEnvelopesAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &persistedDescriptorTransport{}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	adapter := New(a, t.TempDir())
	jid, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	media := []*events.Message{
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "outer-image", Timestamp: time.Unix(1, 0)}, Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), FileLength: proto.Uint64(1), URL: proto.String("https://mmg.whatsapp.net/image"), DirectPath: proto.String("/m/image"), MediaKey: []byte("image-private-key")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "outer-sticker", Timestamp: time.Unix(2, 0)}, Message: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{Mimetype: proto.String("image/webp"), FileLength: proto.Uint64(1), URL: proto.String("https://mmg.whatsapp.net/sticker"), DirectPath: proto.String("/m/sticker"), MediaKey: []byte("sticker-private-key")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "outer-audio", Timestamp: time.Unix(3, 0)}, Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg"), FileLength: proto.Uint64(1), URL: proto.String("https://mmg.whatsapp.net/audio"), DirectPath: proto.String("/m/audio"), MediaKey: []byte("audio-private-key")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "outer-video", Timestamp: time.Unix(4, 0)}, Message: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), FileLength: proto.Uint64(1), URL: proto.String("https://mmg.whatsapp.net/video"), DirectPath: proto.String("/m/video"), MediaKey: []byte("video-private-key")}}},
		{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}, ID: "outer-document", Timestamp: time.Unix(5, 0)}, Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: proto.String("application/pdf"), FileLength: proto.Uint64(1), URL: proto.String("https://mmg.whatsapp.net/document"), DirectPath: proto.String("/m/document"), MediaKey: []byte("document-private-key")}}},
	}
	for _, event := range media {
		adapter.record(context.Background(), event, "Synthetic")
	}
	listed, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(listed)
	if strings.Contains(string(encoded), "private-key") || strings.Contains(string(encoded), "mmg.whatsapp.net") || strings.Contains(string(encoded), "/m/") {
		t.Fatalf("private media descriptor leaked over RPC: %s", encoded)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	for _, event := range media {
		if _, err := a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"12345@s.whatsapp.net","id":"`+event.Info.ID+`"}`)); err != nil {
			t.Fatalf("download %s: %v", event.Info.ID, err)
		}
	}
	want := []string{
		"image:https://mmg.whatsapp.net/image:/m/image:image-private-key",
		"sticker:https://mmg.whatsapp.net/sticker:/m/sticker:sticker-private-key",
		"audio:https://mmg.whatsapp.net/audio:/m/audio:audio-private-key",
		"video:https://mmg.whatsapp.net/video:/m/video:video-private-key",
		"document:https://mmg.whatsapp.net/document:/m/document:document-private-key",
	}
	if len(transport.seen) != len(want) {
		t.Fatalf("persisted outer descriptors were not retained: %#v", transport.seen)
	}
	for i := range want {
		if transport.seen[i] != want[i] {
			t.Fatalf("descriptor %d changed or selected the wrong outer field: got %q want %q", i, transport.seen[i], want[i])
		}
	}
}

func TestInboundAudioPTTIdentitySurvivesPersistenceAndDownload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &persistedDescriptorTransport{}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	adapter := New(a, t.TempDir())
	jid, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id  string
		ptt bool
	}{{"audio-plain", false}, {"audio-ptt", true}} {
		message := &waE2E.AudioMessage{
			PTT:        proto.Bool(item.ptt),
			Mimetype:   proto.String("audio/ogg; codecs=opus"),
			FileLength: proto.Uint64(42),
			Seconds:    proto.Uint32(2),
			URL:        proto.String("https://mmg.whatsapp.net/" + item.id),
			DirectPath: proto.String("/m/" + item.id),
			MediaKey:   []byte(item.id + "-private-key"),
			Waveform:   []byte(item.id + "-private-waveform"),
		}
		adapter.record(context.Background(), &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: jid},
				ID:            item.id,
				Timestamp:     time.Unix(1, 0),
			},
			Message: &waE2E.Message{AudioMessage: message},
		}, "Synthetic audio")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")

	read := func() map[string]struct {
		isVoiceNote bool
		present     bool
		kind, mime  string
		duration    int64
		state, url  string
	} {
		t.Helper()
		value, err := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"12345@s.whatsapp.net","limit":10}`))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-key", "private-waveform", "mmg.whatsapp.net", "/m/audio-"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("private audio descriptor leaked over RPC: %s", encoded)
			}
		}
		var page struct {
			Messages []struct {
				ID         string `json:"id"`
				Attachment *struct {
					IsVoiceNote *bool  `json:"is_voice_note"`
					Kind        string `json:"kind"`
					MIME        string `json:"mime"`
					Duration    int64  `json:"duration"`
					State       string `json:"state"`
					URL         string `json:"url"`
				} `json:"attachment"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]struct {
			isVoiceNote bool
			present     bool
			kind, mime  string
			duration    int64
			state, url  string
		})
		for _, message := range page.Messages {
			if message.Attachment == nil {
				continue
			}
			isVoiceNote, present := false, false
			if message.Attachment.IsVoiceNote != nil {
				isVoiceNote, present = *message.Attachment.IsVoiceNote, true
			}
			got[message.ID] = struct {
				isVoiceNote bool
				present     bool
				kind, mime  string
				duration    int64
				state, url  string
			}{isVoiceNote, present, message.Attachment.Kind, message.Attachment.MIME, message.Attachment.Duration, message.Attachment.State, message.Attachment.URL}
		}
		return got
	}
	assertProjection := func(wantState string) {
		t.Helper()
		got := read()
		for _, item := range []struct {
			id  string
			ptt bool
		}{{"audio-plain", false}, {"audio-ptt", true}} {
			actual, ok := got[item.id]
			if !ok || !actual.present || actual.isVoiceNote != item.ptt || actual.kind != "audio" || actual.mime != "audio/ogg; codecs=opus" || actual.duration != 2 || actual.state != wantState {
				t.Fatalf("audio projection %q = %#v (present=%t), want PTT=%t kind=audio MIME=audio/ogg; codecs=opus duration=2 state=%s", item.id, actual, ok, item.ptt, wantState)
			}
			if wantState == "ready" && !strings.HasPrefix(actual.url, "file:///") {
				t.Fatalf("downloaded audio URL = %q, want private local file URL", actual.url)
			}
		}
	}
	assertProjection("remote")
	// The shared fake downloader writes PNG sentinel bytes for every media kind;
	// these assertions prove descriptor/persistence plumbing, not audio decoding.
	for _, id := range []string{"audio-plain", "audio-ptt"} {
		if _, err := a.Handle(context.Background(), "message.download", []byte(`{"chat_id":"12345@s.whatsapp.net","id":"`+id+`"}`)); err != nil {
			t.Fatalf("download %s: %v", id, err)
		}
	}
	assertProjection("ready")
}

func TestTextExtractionRespectsProtectedContent(t *testing.T) {
	tests := []struct {
		event events.Message
		want  string
	}{
		{events.Message{Message: &waE2E.Message{Conversation: proto.String("hello")}}, "hello"},
		{events.Message{Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("long text")}}}, "long text"},
		{events.Message{IsViewOnce: true, Message: &waE2E.Message{Conversation: proto.String("private")}}, "[View-once content unavailable]"},
		{events.Message{IsEphemeral: true, Message: &waE2E.Message{Conversation: proto.String("private")}}, "[Disappearing message not retained]"},
	}
	for _, tc := range tests {
		if got := messageText(&tc.event); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func adapterStatus(t *testing.T, a *app.App) map[string]any {
	t.Helper()
	value, err := a.Handle(context.Background(), "app.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestCanceledAdapterCallbacksPreserveLatestStatusQRAndRevision(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	adapter := New(a, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	jid, err := types.ParseJID("12345@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	message := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: jid},
			ID:            "stale-message",
			Timestamp:     time.Unix(1, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String("stale message")},
	}
	receipt := &events.Receipt{MessageSource: types.MessageSource{Chat: jid}, MessageIDs: []string{"stale-message"}, Type: types.ReceiptTypeRead}
	callbacks := []struct {
		name string
		call func()
	}{
		{"connected", func() { adapter.event(ctx, nil, &events.Connected{}) }},
		{"disconnected", func() { adapter.event(ctx, nil, &events.Disconnected{}) }},
		{"logged out", func() { adapter.event(ctx, nil, &events.LoggedOut{}) }},
		{"record", func() { adapter.record(ctx, message, "Synthetic") }},
		{"receipt", func() { adapter.event(ctx, nil, receipt) }},
	}
	for _, callback := range callbacks {
		t.Run(callback.name, func(t *testing.T) {
			a.SetStatus("connected", "latest session", "latest-qr")
			before := adapterStatus(t, a)
			callback.call()
			after := adapterStatus(t, a)
			if after["state"] != before["state"] || after["message"] != before["message"] || after["qr_png"] != before["qr_png"] || after["revision"] != before["revision"] {
				t.Fatalf("canceled callback overwrote latest status: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestSyntheticQRStatusPathUsesContextBoundary(t *testing.T) {
	a, err := app.Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	adapter := New(a, t.TempDir())
	if !adapter.publishQRStatus(context.Background(), "code", "synthetic-qr") {
		t.Fatal("active synthetic QR status was rejected")
	}
	status := adapterStatus(t, a)
	if status["state"] != "qr" || !strings.HasPrefix(status["qr_png"].(string), "data:image/png;base64,") {
		t.Fatalf("unexpected synthetic QR status: %#v", status)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.SetStatus("latest", "latest session", "latest-qr")
	before := adapterStatus(t, a)
	for _, event := range []string{"code", "timeout", "success"} {
		if adapter.publishQRStatus(ctx, event, "stale-qr") {
			t.Fatalf("canceled QR event %q was accepted", event)
		}
	}
	after := adapterStatus(t, a)
	if after["state"] != before["state"] || after["message"] != before["message"] || after["qr_png"] != before["qr_png"] || after["revision"] != before["revision"] {
		t.Fatalf("canceled QR callback overwrote latest status: before=%#v after=%#v", before, after)
	}
}
