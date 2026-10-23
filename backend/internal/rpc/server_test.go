package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateServerVersionAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a, e := app.Open(filepath.Join(dir, "data"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(dir, "s.sock")
	s, e := Listen(path, a)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	c, e := net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte(`{"v":9,"id":"bad","method":"app.status","params":{}}` + "\n"))
	line, e := bufio.NewReader(c).ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	json.Unmarshal(line, &v)
	if v["error"] == nil {
		t.Fatal("version accepted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}
