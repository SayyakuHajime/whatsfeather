package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticMP4(t *testing.T, profile, encoder string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Fatalf("synthetic MP4 fixture requires installed ffmpeg: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-threads", "1", "-f", "lavfi",
		"-i", "testsrc=size=96x64:rate=12:duration=1", "-f", "lavfi", "-i",
		"sine=frequency=440:sample_rate=44100:duration=1", "-shortest", "-t", "1",
	}
	if encoder == "h264" {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-profile:v", profile, "-pix_fmt", "yuv420p")
	} else {
		args = append(args, "-c:v", encoder, "-q:v", "3")
	}
	args = append(args, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "64k", "-movflags", "+faststart", "-y", path)
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic %s fixture: %v: %s", encoder, err, output)
	}
	return path
}

func videoRPC(t *testing.T, a *App, method string, params any) (map[string]any, error) {
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
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded, nil
}

func requireVideoContentError(t *testing.T, err error) {
	t.Helper()
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "invalid_attachment" || !strings.Contains(strings.ToLower(appErr.Message), "video") {
		t.Fatalf("wanted a video-content validation error, got %v", err)
	}
}

func TestVideoPreparationValidatesAndStagesSupportedMP4(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	source := syntheticMP4(t, "baseline", "h264")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := videoRPC(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "video"})
	if err != nil {
		t.Fatalf("prepare valid synthetic H.264/AAC MP4: %v", err)
	}
	if result["kind"] != "video" || result["mime"] != "video/mp4" || result["video_codec"] != "h264" || result["audio_codec"] != "aac" {
		t.Fatalf("video metadata does not match inspected content: %#v", result)
	}
	if result["width"] != float64(96) || result["height"] != float64(64) || result["duration"].(float64) <= 0 {
		t.Fatalf("video dimensions/duration were not validated: %#v", result)
	}
	staged := result["prepared_path"].(string)
	info, err := os.Stat(staged)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("prepared video copy is not a private regular file: %v (%v)", info, err)
	}
	after, err := os.ReadFile(staged)
	if err != nil || string(after) != string(before) {
		t.Fatalf("video preparation changed source bytes: %v", err)
	}
	sourceAfter, err := os.ReadFile(source)
	if err != nil || string(sourceAfter) != string(before) {
		t.Fatalf("video preparation modified the selected source: %v", err)
	}
}

func TestVideoPreparationRejectsExtensionOnlyAndUnsupportedCodec(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	fake := filepath.Join(root, "not-a-video.mp4")
	if err := os.WriteFile(fake, []byte("not an MP4, despite its suffix"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := videoRPC(t, a, "attachment.prepare", map[string]any{"path": fake, "mode": "video"}); err == nil {
		t.Fatal("accepted arbitrary bytes as video based on .mp4 extension")
	} else {
		requireVideoContentError(t, err)
	}
	unsupported := syntheticMP4(t, "", "mpeg4")
	if _, err := videoRPC(t, a, "attachment.prepare", map[string]any{"path": unsupported, "mode": "video"}); err == nil {
		t.Fatal("accepted unsupported MPEG-4 Part 2 video as H.264")
	} else {
		requireVideoContentError(t, err)
	}
}

type uncertainVideoResult struct{}

func (uncertainVideoResult) Error() string        { return "synthetic ambiguous send" }
func (uncertainVideoResult) UnknownOutcome() bool { return true }

type videoSubmissionProbe struct {
	calls   int
	meta    Attachment
	caption string
	id      string
}

func (p *videoSubmissionProbe) SendMedia(_ context.Context, _ string, _ string, meta Attachment, caption, id string) (int64, error) {
	p.calls++
	p.meta, p.caption, p.id = meta, caption, id
	return 0, uncertainVideoResult{}
}

type videoSubmissionTransport struct{ videoSubmissionProbe }

func (videoSubmissionTransport) Connect(context.Context) error { return nil }
func (videoSubmissionTransport) Disconnect()                   {}
func (videoSubmissionTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

func TestVideoAttachmentRPCPersistsCaptionAndDoesNotBlindRetryUnknown(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &videoSubmissionProbe{}
	a.SetTransport(struct {
		Transport
		MediaSender
	}{Transport: videoSubmissionTransport{}, MediaSender: probe})
	a.SetStatus("connected", "synthetic test transport", "")
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: "video-chat", Body: "seed", Timestamp: 1, Status: "received"}, "Fixture"); err != nil {
		t.Fatal(err)
	}
	source := syntheticMP4(t, "baseline", "h264")
	prepared, err := videoRPC(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "video"})
	if err != nil {
		t.Fatalf("prepare video for synthetic send: %v", err)
	}
	params := map[string]any{"chat_id": "video-chat", "prepared_path": prepared["prepared_path"], "mode": "video", "text": "caption stays attached", "client_id": "video-unknown-once"}
	first, err := videoRPC(t, a, "message.attach", params)
	if err != nil {
		t.Fatal(err)
	}
	message := first["message"].(map[string]any)
	attachment := message["attachment"].(map[string]any)
	if message["status"] != "unknown" || message["body"] != "caption stays attached" || attachment["kind"] != "video" {
		t.Fatalf("video caption/state was not persisted: %#v", message)
	}
	if probe.calls != 1 || probe.meta.Kind != "video" || probe.meta.MIME != "video/mp4" || probe.caption != message["body"] || probe.id != "video-unknown-once" {
		t.Fatalf("wrong media send inputs: %#v", probe)
	}
	if _, err = videoRPC(t, a, "message.attach", params); err != nil {
		t.Fatal(err)
	}
	if probe.calls != 1 {
		t.Fatalf("unknown video send was retried blindly: calls=%d", probe.calls)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(filepath.Join(root, "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	listed, err := videoRPC(t, a, "message.list", map[string]any{"chat_id": "video-chat", "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	rows := listed["messages"].([]any)
	found := false
	for _, value := range rows {
		row := value.(map[string]any)
		if row["id"] == "video-unknown-once" {
			attachment := row["attachment"].(map[string]any)
			found = row["status"] == "unknown" && row["body"] == "caption stays attached" && attachment["kind"] == "video"
		}
	}
	if !found {
		t.Fatalf("video message/caption/status did not survive restart: %#v", rows)
	}
}

func TestVideoCapabilityTracksImplementedPathWithoutEnablingOtherMedia(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	result, err := videoRPC(t, a, "app.capabilities", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	features := result["features"].(map[string]any)
	video := features["video"].(map[string]any)
	if video["available"] != true || !strings.Contains(fmt.Sprint(video["reason"]), "H.264") {
		t.Fatalf("video capability does not describe the validated implementation: %#v", video)
	}
	for _, id := range []string{"voice-video-calls", "audio-voice-notes"} {
		if features[id].(map[string]any)["available"] != false {
			t.Fatalf("unrelated feature %q became available", id)
		}
	}
	audio := features["audio-voice-notes"].(map[string]any)
	reason := fmt.Sprint(audio["reason"])
	for _, expected := range []string{"incoming audio descriptors", "paused native playback", "codec/device acceptance", "OGG/Opus", "native file selection, review and send", "microphone voice recording is not implemented"} {
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(expected)) {
			t.Fatalf("audio capability reason omits implemented/unsupported boundary %q: %q", expected, reason)
		}
	}
}
