package app

import (
	"context"
	"testing"
)

func TestTypedRedeliveryClearsLegacyUnsupportedPlaceholder(t *testing.T) {
	a, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	old := Message{ID: "legacy-image", ChatID: "tester", Body: "[Unsupported content — open the official app]", Timestamp: 1000, Status: "received"}
	if err = a.Record(ctx, old, "Tester"); err != nil {
		t.Fatal(err)
	}
	image := old
	image.Body = ""
	image.Kind = "image"
	image.Attachment = &Attachment{Kind: "image", MIME: "image/png", State: "remote", Size: 100}
	if err = a.Record(ctx, image, "Tester"); err != nil {
		t.Fatal(err)
	}
	var body, kind string
	if err = a.db.QueryRow("SELECT body,kind FROM messages WHERE chat_id=? AND id=?", image.ChatID, image.ID).Scan(&body, &kind); err != nil {
		t.Fatal(err)
	}
	if body != "" || kind != "image" {
		t.Fatalf("captionless media retains misleading unsupported text: body=%q kind=%q", body, kind)
	}
}
