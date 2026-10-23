package whatsapp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type fakeAudioMediaClient struct {
	connected   bool
	loggedIn    bool
	uploadType  whatsmeow.MediaType
	uploaded    []byte
	uploadCalls int
	sendCalls   int
	upload      whatsmeow.UploadResponse
	message     *waE2E.Message
	target      types.JID
	extra       []whatsmeow.SendRequestExtra
	response    whatsmeow.SendResponse
}

func (f *fakeAudioMediaClient) IsConnected() bool { return f.connected }
func (f *fakeAudioMediaClient) IsLoggedIn() bool  { return f.loggedIn }

func (f *fakeAudioMediaClient) UploadReader(_ context.Context, plaintext io.Reader, tempFile io.ReadWriteSeeker, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploadCalls++
	if tempFile != nil {
		return whatsmeow.UploadResponse{}, os.ErrInvalid
	}
	f.uploadType = appInfo
	data, err := io.ReadAll(plaintext)
	f.uploaded = data
	return f.upload, err
}

func (f *fakeAudioMediaClient) SendMessage(_ context.Context, target types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sendCalls++
	f.target = target
	f.message = proto.Clone(message).(*waE2E.Message)
	f.extra = append([]whatsmeow.SendRequestExtra(nil), extra...)
	return f.response, nil
}

func TestSendMediaAudioUsesAudioUploadAndPreservesPTTIdentity(t *testing.T) {
	media := []byte("synthetic opus bytes")
	for _, tc := range []struct {
		name         string
		voiceNote    bool
		wantMimetype string
	}{
		{name: "ordinary audio", wantMimetype: "audio/ogg; codecs=opus"},
		{name: "voice note", voiceNote: true, wantMimetype: "audio/ogg; codecs=opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.ogg")
			if err := os.WriteFile(path, media, 0600); err != nil {
				t.Fatal(err)
			}
			meta := app.Attachment{Kind: "audio", MIME: tc.wantMimetype, Duration: 5, Size: int64(len(media)), PreparedSize: int64(len(media)), SHA256: testMediaSHA256(media), IsVoiceNote: tc.voiceNote}
			client := &fakeAudioMediaClient{
				connected: true,
				loggedIn:  true,
				upload: whatsmeow.UploadResponse{
					URL: "https://mmg.whatsapp.net/synthetic-audio", DirectPath: "/m/synthetic-audio",
					MediaKey: []byte("synthetic-audio-key"), FileEncSHA256: []byte("synthetic-encrypted-hash"),
					FileSHA256: []byte("synthetic-file-hash"), FileLength: uint64(len(media)),
				},
				response: whatsmeow.SendResponse{Timestamp: time.Unix(456, 0)},
			}
			adapter := &Adapter{dir: t.TempDir()}
			currentChecks := 0
			got, err := adapter.sendMediaWithClient(context.Background(), client, func() bool {
				currentChecks++
				return true
			}, "12345@s.whatsapp.net", path, meta, "", "audio-client-id")
			if err != nil {
				t.Fatal(err)
			}
			if got != client.response.Timestamp.UnixMilli() {
				t.Fatalf("timestamp = %d, want %d", got, client.response.Timestamp.UnixMilli())
			}
			if client.uploadCalls != 1 || client.sendCalls != 1 || client.uploadType != whatsmeow.MediaAudio || !bytes.Equal(client.uploaded, media) {
				t.Fatalf("audio transfer boundary mismatch: uploads=%d sends=%d mediaType=%v bytesMatch=%t", client.uploadCalls, client.sendCalls, client.uploadType, bytes.Equal(client.uploaded, media))
			}
			if client.message == nil || client.message.GetAudioMessage() == nil || client.message.GetImageMessage() != nil || client.message.GetDocumentMessage() != nil {
				t.Fatalf("send path must pass an outer AudioMessage, got %#v", client.message)
			}
			audio := client.message.GetAudioMessage()
			if audio.GetURL() != client.upload.URL || audio.GetDirectPath() != client.upload.DirectPath ||
				!bytes.Equal(audio.GetMediaKey(), client.upload.MediaKey) ||
				!bytes.Equal(audio.GetFileEncSHA256(), client.upload.FileEncSHA256) ||
				!bytes.Equal(audio.GetFileSHA256(), client.upload.FileSHA256) ||
				audio.GetFileLength() != client.upload.FileLength || audio.GetMimetype() != tc.wantMimetype ||
				audio.GetSeconds() != 5 || audio.GetPTT() != tc.voiceNote {
				t.Fatalf("AudioMessage lost upload metadata, duration, or PTT identity: %#v", audio)
			}
			if audio.PTT == nil {
				t.Fatal("AudioMessage must explicitly preserve false versus true PTT state")
			}
			wantTarget, err := types.ParseJID("12345@s.whatsapp.net")
			if err != nil {
				t.Fatal(err)
			}
			if client.target != wantTarget || len(client.extra) != 1 || string(client.extra[0].ID) != "audio-client-id" || client.extra[0].Timeout != 20*time.Second {
				t.Fatalf("send identity/options mismatch: target=%v extra=%#v", client.target, client.extra)
			}
			if currentChecks < 2 {
				t.Fatalf("current-client guard ran %d times, want checks before and after send", currentChecks)
			}
		})
	}
}

func TestSendMediaAudioRejectsCaptionBeforeUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.ogg")
	media := []byte("synthetic opus bytes")
	if err := os.WriteFile(path, media, 0600); err != nil {
		t.Fatal(err)
	}
	client := &fakeAudioMediaClient{connected: true, loggedIn: true}
	adapter := &Adapter{dir: t.TempDir()}
	meta := app.Attachment{Kind: "audio", MIME: "audio/ogg; codecs=opus", Duration: 5, Size: int64(len(media)), PreparedSize: int64(len(media)), SHA256: testMediaSHA256(media), IsVoiceNote: true}
	_, err := adapter.sendMediaWithClient(context.Background(), client, func() bool { return true },
		"12345@s.whatsapp.net", path, meta,
		"caption not supported by AudioMessage", "audio-caption-id")
	if err == nil || !strings.Contains(err.Error(), "audio messages do not support captions") {
		t.Fatalf("audio caption must fail with a precise error, got %v", err)
	}
	if client.uploadCalls != 0 || client.sendCalls != 0 {
		t.Fatalf("caption rejection happened after transfer side effects: uploads=%d sends=%d", client.uploadCalls, client.sendCalls)
	}
}
