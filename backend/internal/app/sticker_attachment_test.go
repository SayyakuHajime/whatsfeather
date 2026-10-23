package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/image/webp"
)

func stickerFixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireStickerValidationError(t *testing.T, err error) {
	t.Helper()
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "invalid_attachment" || !strings.Contains(strings.ToLower(appErr.Message), "sticker") {
		t.Fatalf("wanted a clear sticker validation error, got %v", err)
	}
}

func TestPrepareStaticTransparentWebPStickerStagesPrivateBytes(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	source := stickerFixture(t, "sticker-static.webp")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "sticker"})
	if prepared["kind"] != "sticker" || prepared["mode"] != "sticker" || prepared["mime"] != "image/webp" || prepared["width"] != float64(512) || prepared["height"] != float64(512) {
		t.Fatalf("prepared sticker metadata is wrong: %#v", prepared)
	}
	if prepared["compressed"] == true || prepared["size"] != float64(len(before)) || prepared["prepared_size"] != float64(len(before)) {
		t.Fatalf("sticker bytes must stay original and uncompressed: %#v", prepared)
	}
	staged := prepared["prepared_path"].(string)
	info, err := os.Stat(staged)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("staged sticker is not a private regular file: %v (%v)", info, err)
	}
	after, err := os.ReadFile(staged)
	if err != nil || string(after) != string(before) {
		t.Fatalf("prepared sticker bytes changed: %v", err)
	}
	sourceAfter, err := os.ReadFile(source)
	if err != nil || string(sourceAfter) != string(before) {
		t.Fatalf("preparation modified the selected source: %v", err)
	}
}

func TestPrepareStickerRejectsNonStaticOrInvalidStickerData(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	pngPath := filepath.Join(t.TempDir(), "png-as-sticker.webp")
	pngFile, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(pngFile, image.NewNRGBA(image.Rect(0, 0, 512, 512))); err != nil {
		pngFile.Close()
		t.Fatal(err)
	}
	if err = pngFile.Close(); err != nil {
		t.Fatal(err)
	}
	malformedPath := filepath.Join(t.TempDir(), "malformed.webp")
	if err = os.WriteFile(malformedPath, []byte("RIFF\x04\x00\x00\x00WEBP"), 0600); err != nil {
		t.Fatal(err)
	}
	oversizedPath := filepath.Join(t.TempDir(), "oversized.webp")
	if err = os.WriteFile(oversizedPath, make([]byte, 100001), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
	}{
		{name: "PNG is not WebP", path: pngPath},
		{name: "malformed WebP", path: malformedPath},
		{name: "oversized static sticker", path: oversizedPath},
		{name: "wrong dimensions", path: stickerFixture(t, "sticker-wrong-size.webp")},
		{name: "opaque background", path: stickerFixture(t, "sticker-opaque.webp")},
		{name: "animated WebP", path: stickerFixture(t, "sticker-animated.webp")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Handle(context.Background(), "attachment.prepare", mustJSON(t, map[string]any{"path": tc.path, "mode": "sticker"}))
			requireStickerValidationError(t, err)
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type stickerSendProbe struct {
	calls   int
	meta    Attachment
	caption string
	client  string
	bytes   []byte
}

func (p *stickerSendProbe) Connect(context.Context) error { return nil }
func (p *stickerSendProbe) Disconnect()                   {}
func (p *stickerSendProbe) Send(context.Context, string, string, string) (int64, error) {
	return 1, nil
}
func (p *stickerSendProbe) SendMedia(_ context.Context, _ string, path string, meta Attachment, caption, clientID string) (int64, error) {
	p.calls++
	p.meta, p.caption, p.client = meta, caption, clientID
	var err error
	p.bytes, err = os.ReadFile(path)
	return 123, err
}

func newStickerSendApp(t *testing.T, probe *stickerSendProbe) *App {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic sticker transport", "")
	if err = a.Record(context.Background(), Message{ID: "seed", ChatID: "sticker-chat", Body: "seed", Timestamp: 1, Status: "received"}, "Fixture"); err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestStickerAttachmentRPCUsesTypedSendOnceAndPersistsSticker(t *testing.T) {
	probe := &stickerSendProbe{}
	a := newStickerSendApp(t, probe)
	source := stickerFixture(t, "sticker-static.webp")
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "sticker"})
	params := map[string]any{"chat_id": "sticker-chat", "prepared_path": prepared["prepared_path"], "mode": "sticker", "text": "", "client_id": "sticker-send-once"}
	first := invoke(t, a, "message.attach", params)["message"].(map[string]any)
	second := invoke(t, a, "message.attach", params)["message"].(map[string]any)
	attachment := first["attachment"].(map[string]any)
	if first["status"] != "accepted" || first["kind"] != "sticker" || attachment["kind"] != "sticker" || attachment["mime"] != "image/webp" {
		t.Fatalf("sticker send projection is wrong: %#v", first)
	}
	if second["id"] != first["id"] || probe.calls != 1 {
		t.Fatalf("duplicate sticker RPC resent or changed identity: first=%#v second=%#v calls=%d", first, second, probe.calls)
	}
	if probe.meta.Kind != "sticker" || probe.meta.MIME != "image/webp" || probe.meta.Width != 512 || probe.meta.Height != 512 || probe.caption != "" || probe.client != "sticker-send-once" || string(probe.bytes) != string(original) {
		t.Fatalf("wrong sticker bytes/metadata reached the transport: %#v, bytes=%d", probe, len(probe.bytes))
	}
}

func TestStickerAttachmentFailsClosedForCaptionAndDirectNonWebP(t *testing.T) {
	probe := &stickerSendProbe{}
	a := newStickerSendApp(t, probe)
	source := stickerFixture(t, "sticker-static.webp")
	prepared := invoke(t, a, "attachment.prepare", map[string]any{"path": source, "mode": "sticker"})
	_, err := a.Handle(context.Background(), "message.attach", mustJSON(t, map[string]any{
		"chat_id": "sticker-chat", "prepared_path": prepared["prepared_path"], "mode": "sticker", "text": "caption", "client_id": "sticker-caption-rejected",
	}))
	requireStickerValidationError(t, err)
	pngPath := filepath.Join(t.TempDir(), "not-a-sticker.png")
	pngFile, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(pngFile, image.NewNRGBA(image.Rect(0, 0, 512, 512))); err != nil {
		pngFile.Close()
		t.Fatal(err)
	}
	if err = pngFile.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "message.attach", mustJSON(t, map[string]any{
		"chat_id": "sticker-chat", "path": pngPath, "mode": "sticker", "text": "", "client_id": "sticker-png-rejected",
	}))
	requireStickerValidationError(t, err)
	if probe.calls != 0 {
		t.Fatalf("invalid sticker input reached transport %d times", probe.calls)
	}
}

func TestInspectStickerValidatesAndReturnsTypedMetadata(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	meta := invoke(t, a, "attachment.inspect", map[string]any{"path": stickerFixture(t, "sticker-static.webp"), "mode": "sticker"})
	if meta["kind"] != "sticker" || meta["mode"] != "sticker" || meta["mime"] != "image/webp" || meta["width"] != float64(512) || meta["height"] != float64(512) {
		t.Fatalf("sticker inspect metadata is wrong: %#v", meta)
	}
	_, err = a.Handle(context.Background(), "attachment.inspect", mustJSON(t, map[string]any{"path": stickerFixture(t, "sticker-animated.webp"), "mode": "sticker"}))
	requireStickerValidationError(t, err)
}

func TestStickerCapabilityDescribesStaticWebPSupportAndLimits(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	features := invoke(t, a, "app.capabilities", map[string]any{})["features"].(map[string]any)
	sticker := features["stickers"].(map[string]any)
	reason := strings.ToLower(sticker["reason"].(string))
	if sticker["available"] != true || !strings.Contains(reason, "static") || !strings.Contains(reason, "webp") || !strings.Contains(reason, "animated") || !strings.Contains(reason, "pack") {
		t.Fatalf("sticker capability must describe static WebP support and unsupported limits: %#v", sticker)
	}
}

func TestStickerFixtureIsTransparentStaticWebP(t *testing.T) {
	data, err := os.ReadFile(stickerFixture(t, "sticker-static.webp"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode static WebP fixture: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 512 || bounds.Dy() != 512 {
		t.Fatalf("fixture dimensions are %v, want 512x512", bounds)
	}
	transparent := false
	for y := 0; y < 512 && !transparent; y++ {
		for x := 0; x < 512; x++ {
			_, _, _, alpha := decoded.At(x, y).RGBA()
			if alpha < 0xffff {
				transparent = true
				break
			}
		}
	}
	if !transparent {
		t.Fatal("fixture needs transparent pixels")
	}
}
