package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func syntheticAudioOPUS(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("synthetic audio acceptance requires ffmpeg")
	}
	path := filepath.Join(t.TempDir(), "synthetic-opus.ogg")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-threads", "1", "-f", "lavfi",
		"-i", "anullsrc=channel_layout=mono:sample_rate=48000", "-t", "3",
		"-c:a", "libopus", "-b:a", "16k", "-y", path,
	}
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic OGG/Opus: %v: %s", err, output)
	}
	return path
}

func audioRPC(t *testing.T, a *App, method string, params map[string]any) (map[string]any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), method, raw)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, nil
}

func TestAttachmentPrepareValidatesOggOpusAndExplicitVoiceNoteIdentity(t *testing.T) {
	fixture := syntheticAudioOPUS(t)
	for _, tc := range []struct {
		mode      string
		voiceNote bool
	}{
		{mode: "audio"},
		{mode: "voice-note", voiceNote: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			prepared, err := audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			duration, durationOK := prepared["duration"].(float64)
			path, pathOK := prepared["prepared_path"].(string)
			if prepared["kind"] != "audio" || prepared["mime"] != "audio/ogg; codecs=opus" ||
				prepared["audio_codec"] != "opus" || prepared["is_voice_note"] != tc.voiceNote ||
				prepared["state"] != "ready" || prepared["mode"] != tc.mode ||
				!durationOK || duration < 3 || !pathOK || path == "" {
				t.Fatalf("prepared audio projection is incomplete or mislabeled: %#v", prepared)
			}
			staged, err := os.Stat(path)
			if err != nil || !staged.Mode().IsRegular() || staged.Mode().Perm() != 0600 {
				t.Fatalf("prepared audio is not a private regular file: info=%v err=%v", staged, err)
			}
		})
	}
}

func TestAttachmentPrepareRejectsMalformedAudioWithoutStaging(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fixture := filepath.Join(t.TempDir(), "not-audio.ogg")
	if err = os.WriteFile(fixture, []byte("synthetic not audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": "voice-note"}); err == nil {
		t.Fatal("malformed audio must not be staged as a voice note")
	}
	a.mediaMu.Lock()
	preparedCount := len(a.prepared)
	a.mediaMu.Unlock()
	if preparedCount != 0 {
		t.Fatalf("malformed audio left %d staged entries", preparedCount)
	}
}

func TestMessageAttachRejectsUnpreparedAudioAndCaptions(t *testing.T) {
	fixture := syntheticAudioOPUS(t)
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = audioRPC(t, a, "message.attach", map[string]any{
		"chat_id": "demo:alex", "path": fixture, "mode": "audio", "client_id": "raw-audio-bypass",
	}); err == nil {
		t.Fatal("raw audio mode must not bypass validated private staging")
	}
	prepared, err := audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": "audio"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = audioRPC(t, a, "message.attach", map[string]any{
		"chat_id": "demo:alex", "prepared_path": prepared["prepared_path"], "mode": "audio",
		"text": "audio caption", "client_id": "audio-caption-rejected",
	}); err == nil {
		t.Fatal("audio caption must be rejected before persistence")
	}
}

func TestMessageAttachPersistsExplicitAudioAndVoiceNoteIdentity(t *testing.T) {
	fixture := syntheticAudioOPUS(t)
	for _, tc := range []struct {
		mode      string
		voiceNote bool
	}{
		{mode: "audio"},
		{mode: "voice-note", voiceNote: true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), "profile")
			a, err := Open(profile, true)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			const clientID = "synthetic-audio-attach"
			attached, err := audioRPC(t, a, "message.attach", map[string]any{
				"chat_id": "demo:alex", "prepared_path": prepared["prepared_path"], "mode": tc.mode, "client_id": clientID,
			})
			if err != nil {
				t.Fatal(err)
			}
			message := attached["message"].(map[string]any)
			attachment := message["attachment"].(map[string]any)
			if message["kind"] != "audio" || attachment["kind"] != "audio" || attachment["is_voice_note"] != tc.voiceNote {
				t.Fatalf("audio send projection lost kind/PTT identity: %#v", message)
			}
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			a, err = Open(profile, true)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			listed, err := audioRPC(t, a, "message.list", map[string]any{"chat_id": "demo:alex", "limit": 10})
			if err != nil {
				t.Fatal(err)
			}
			rows, ok := listed["messages"].([]any)
			if !ok {
				t.Fatalf("message.list has unexpected shape: %#v", listed)
			}
			found := false
			for _, value := range rows {
				row, ok := value.(map[string]any)
				if !ok || row["id"] != clientID {
					continue
				}
				attachment, _ := row["attachment"].(map[string]any)
				found = row["kind"] == "audio" && attachment["kind"] == "audio" && attachment["is_voice_note"] == tc.voiceNote
			}
			if !found {
				t.Fatalf("audio/PTT identity did not survive SQLite reopen: %#v", rows)
			}
		})
	}
}

type audioAttachTransport struct{}

func (audioAttachTransport) Connect(context.Context) error { return nil }
func (audioAttachTransport) Disconnect()                   {}
func (audioAttachTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

type audioAttachSender struct {
	calls   int
	path    string
	meta    Attachment
	caption string
	id      string
}

func (s *audioAttachSender) SendMedia(_ context.Context, _ string, path string, meta Attachment, caption, id string) (int64, error) {
	s.calls++
	s.path, s.meta, s.caption, s.id = path, meta, caption, id
	return time.Unix(456, 0).UnixMilli(), nil
}

func TestMessageAttachRejectsPreparedAudioWhoseBytesChanged(t *testing.T) {
	fixture := syntheticAudioOPUS(t)
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sender := &audioAttachSender{}
	a.SetTransport(struct {
		Transport
		MediaSender
	}{Transport: audioAttachTransport{}, MediaSender: sender})
	a.SetStatus("connected", "synthetic media sender", "")
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: "audio-chat", Body: "seed", Timestamp: 1, Status: "received"}, "Fixture"); err != nil {
		t.Fatal(err)
	}
	prepared, err := audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": "voice-note"})
	if err != nil {
		t.Fatal(err)
	}
	preparedPath, ok := prepared["prepared_path"].(string)
	if !ok || preparedPath == "" {
		t.Fatalf("prepared path is missing: %#v", prepared)
	}
	original, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(preparedPath, make([]byte, len(original)), 0600); err != nil {
		t.Fatal(err)
	}

	_, err = audioRPC(t, a, "message.attach", map[string]any{
		"chat_id": "audio-chat", "prepared_path": preparedPath, "mode": "voice-note", "client_id": "mutated-voice-note",
	})
	if err == nil {
		t.Fatal("modified prepared bytes must be rejected before sending")
	}
	appErr, ok := err.(*Error)
	if !ok || appErr.Code != "invalid_attachment" {
		t.Fatalf("modified prepared bytes returned the wrong error: %v", err)
	}
	if sender.calls != 0 {
		t.Fatalf("modified prepared bytes reached transport %d times", sender.calls)
	}
	rows, err := a.queryMessages(context.Background(), "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", "mutated-voice-note")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("modified prepared bytes were persisted as an outgoing message: %#v", rows)
	}
}

func TestMessageAttachSubmitsPreparedVoiceNoteOnceToTransport(t *testing.T) {
	fixture := syntheticAudioOPUS(t)
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sender := &audioAttachSender{}
	a.SetTransport(struct {
		Transport
		MediaSender
	}{Transport: audioAttachTransport{}, MediaSender: sender})
	a.SetStatus("connected", "synthetic media sender", "")
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: "audio-chat", Body: "seed", Timestamp: 1, Status: "received"}, "Fixture"); err != nil {
		t.Fatal(err)
	}
	prepared, err := audioRPC(t, a, "attachment.prepare", map[string]any{"path": fixture, "mode": "voice-note"})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{
		"chat_id": "audio-chat", "prepared_path": prepared["prepared_path"], "mode": "voice-note",
		"client_id": "voice-note-submission",
	}
	result, err := audioRPC(t, a, "message.attach", params)
	if err != nil {
		t.Fatal(err)
	}
	message := result["message"].(map[string]any)
	if sender.calls != 1 || sender.caption != "" || sender.id != "voice-note-submission" ||
		sender.meta.Kind != "audio" || sender.meta.AudioCodec != "opus" || !sender.meta.IsVoiceNote ||
		sender.meta.MIME != audioMIME || sender.meta.Duration < 3 || message["status"] != "accepted" {
		t.Fatalf("app did not submit an accurately typed voice note: calls=%d sender=%#v message=%#v", sender.calls, sender, message)
	}
	staged, err := os.Stat(sender.path)
	if err != nil || !staged.Mode().IsRegular() || staged.Mode().Perm() != 0600 {
		t.Fatalf("transport path is not a private staged file: info=%v err=%v", staged, err)
	}
	if _, err = audioRPC(t, a, "message.attach", params); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("idempotent duplicate caused another send: calls=%d", sender.calls)
	}
}
