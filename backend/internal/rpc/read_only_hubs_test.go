package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
)

func TestPrivateRPCReadOnlyHubs(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := app.Open(filepath.Join(root, "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Record(context.Background(), app.Message{ID: "synthetic-image", ChatID: "synthetic-chat", Body: "caption", Timestamp: 1, Kind: "image", Status: "received", Attachment: &app.Attachment{Name: "fixture", Kind: "image", State: "remote", URL: "https://invalid.example/private"}, MediaPayload: `{"key":"PRIVATE_DESCRIPTOR_SECRET"}`}, "Synthetic title"); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "hub.sock")
	cancel, done := startPollRPC(t, a, socket)
	defer stopPollRPC(t, cancel, done)
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for _, method := range []string{"media.list", "chat.info"} {
		request, _ := json.Marshal(map[string]any{"v": 1, "id": strings.Repeat("\x01", 128), "method": method, "params": map[string]any{"chat_id": "synthetic-chat"}})
		if _, err = conn.Write(append(request, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if len(line) > 262144 {
			t.Fatal("oversized private RPC frame")
		}
		for _, secret := range []string{"PRIVATE_DESCRIPTOR_SECRET", "https://invalid.example/private"} {
			if strings.Contains(string(line), secret) {
				t.Fatal("descriptor leaked")
			}
		}
		var response struct {
			Result map[string]json.RawMessage `json:"result"`
			Error  *app.Error                 `json:"error"`
		}
		if err = json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error != nil {
			t.Fatal(response.Error)
		}
		if string(response.Result["provenance"]) != `"local_cached"` {
			t.Fatal("missing provenance")
		}
	}
	for _, method := range []string{"media.list", "chat.info"} {
		request, _ := json.Marshal(map[string]any{"v": 1, "id": "unknown", "method": method, "params": map[string]any{"chat_id": "unknown"}})
		if _, err = conn.Write(append(request, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Error *app.Error `json:"error"`
		}
		if err = json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error == nil || response.Error.Code != "not_found" {
			t.Fatal("unknown chat accepted")
		}
	}
}
