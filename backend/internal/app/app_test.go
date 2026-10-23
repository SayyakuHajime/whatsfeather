package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func invoke(t *testing.T, a *App, m string, p any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(p)
	v, e := a.Handle(context.Background(), m, b)
	if e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(v)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

type connectProbe struct{ context chan context.Context }

func (p *connectProbe) Connect(ctx context.Context) error { p.context <- ctx; return nil }
func (p *connectProbe) Disconnect()                       {}
func (p *connectProbe) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled")
}
func TestDisconnectCancelsPendingConnect(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	probe := &connectProbe{make(chan context.Context, 1)}
	a.SetTransport(probe)
	invoke(t, a, "auth.connect", map[string]any{})
	run := <-probe.context
	invoke(t, a, "auth.disconnect", map[string]any{})
	select {
	case <-run.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pending connect not canceled")
	}
}
func TestLegacyMessageSchemaMigratesWithoutLosingHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	a, e := Open(dir, false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.db.Exec(`DROP TABLE messages; CREATE TABLE messages(id TEXT PRIMARY KEY,chat_id TEXT NOT NULL,body TEXT NOT NULL,timestamp INTEGER NOT NULL,outgoing INTEGER NOT NULL,status TEXT NOT NULL,client_id TEXT UNIQUE); INSERT INTO chats VALUES('old','Old','O','#666666',0); INSERT INTO messages VALUES('id','old','preserved',1,0,'received',NULL);`)
	if e != nil {
		t.Fatal(e)
	}
	a.Close()
	a, e = Open(dir, false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if e = a.Record(context.Background(), Message{ID: "id", ChatID: "new", Body: "new", Timestamp: 2, Status: "received"}, "New"); e != nil {
		t.Fatal(e)
	}
	old := invoke(t, a, "message.list", map[string]any{"chat_id": "old"})["messages"].([]any)
	if old[0].(map[string]any)["body"] != "preserved" {
		t.Fatal("history lost")
	}
}
func TestProtocolIDsDoNotMergeDifferentChats(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ctx := context.Background()
	for _, id := range []string{"chat-a", "chat-b"} {
		if e = a.Record(ctx, Message{ID: "shared-id", ChatID: id, Body: id, Timestamp: 1, Status: "received"}, id); e != nil {
			t.Fatal(e)
		}
	}
	for _, id := range []string{"chat-a", "chat-b"} {
		p := invoke(t, a, "message.list", map[string]any{"chat_id": id})
		messages := p["messages"].([]any)
		if len(messages) != 1 || messages[0].(map[string]any)["body"] != id {
			t.Fatalf("cross-chat message collision for %s: %v", id, p)
		}
	}
}
func TestDemoPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, e := Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	chats := invoke(t, a, "chat.list", map[string]any{})["chats"].([]any)
	if len(chats) != 4 {
		t.Fatalf("expected four explicit synthetic chats, got %d", len(chats))
	}
	id := chats[0].(map[string]any)["id"].(string)
	p := map[string]any{"chat_id": id, "text": "  hello 日本語  ", "client_id": "stable"}
	one := invoke(t, a, "message.send", p)["message"].(map[string]any)
	two := invoke(t, a, "message.send", p)["message"].(map[string]any)
	if one["id"] != two["id"] || one["status"] != "demo_sent" || one["body"] != p["text"] {
		t.Fatal("idempotence/content/truth failure")
	}
	invoke(t, a, "draft.set", map[string]any{"chat_id": id, "text": "  draft\n "})
	a.Close()
	a, e = Open(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if invoke(t, a, "draft.get", map[string]any{"chat_id": id})["text"] != "  draft\n " {
		t.Fatal("draft lost on restart")
	}
}
func TestAccountGetIsTruthfulForDemoAndDisconnectedLive(t *testing.T) {
	demo, err := Open(filepath.Join(t.TempDir(), "demo"), true)
	if err != nil {
		t.Fatal(err)
	}
	identity := invoke(t, demo, "account.get", map[string]any{})
	if identity["available"] != true || identity["display_name"] != "Demo Account" || identity["username"] != "demo" || identity["state"] != "demo" {
		t.Fatalf("unexpected demo identity: %#v", identity)
	}
	if identity["avatar_url"] != "" {
		t.Fatal("demo avatar must not imply a cached network fetch")
	}
	if err := demo.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := Open(filepath.Join(t.TempDir(), "live"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	identity = invoke(t, live, "account.get", map[string]any{})
	if identity["available"] != false || identity["username"] != "" || identity["phone"] != "" {
		t.Fatalf("disconnected identity fabricated data: %#v", identity)
	}
}

func TestRejectInvalidAndNoLiveCalls(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	for _, tc := range []struct {
		m string
		p string
	}{{"auth.connect", "{}"}, {"message.list", `{"chat_id":"absent"}`}, {"draft.set", `{"chat_id":"absent","text":"x"}`}, {"message.send", `{"chat_id":"demo:alex","text":"  ","client_id":"x"}`}} {
		if _, e = a.Handle(context.Background(), tc.m, json.RawMessage(tc.p)); e == nil {
			t.Fatalf("accepted %s", tc.m)
		}
	}
}
func TestLiveIsDisconnectedAndEmpty(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	if invoke(t, a, "app.status", map[string]any{})["state"] != "disconnected" {
		t.Fatal("live auto-connected")
	}
	if len(invoke(t, a, "chat.list", map[string]any{})["chats"].([]any)) != 0 {
		t.Fatal("demo contamination")
	}
}

type blockedTransport struct {
	entered chan bool
	release chan bool
}

func (t *blockedTransport) Connect(context.Context) error { return nil }
func (t *blockedTransport) Disconnect()                   {}
func (t *blockedTransport) Send(context.Context, string, string, string) (int64, error) {
	t.entered <- true
	<-t.release
	return 0, errors.New("uncertain")
}
func TestTransportWaitDoesNotBlockStatusAndUnknownIsNotResent(t *testing.T) {
	a, e := Open(filepath.Join(t.TempDir(), "p"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	ctx := context.Background()
	a.Record(ctx, Message{ID: "seed", ChatID: "fixture", Body: "local", Timestamp: 1, Status: "received"}, "Fixture")
	transport := &blockedTransport{make(chan bool, 1), make(chan bool)}
	a.SetTransport(transport)
	a.SetStatus("connected", "test", "")
	result := make(chan any, 1)
	go func() {
		v, _ := a.Handle(ctx, "message.send", json.RawMessage(`{"chat_id":"fixture","text":"hello","client_id":"uncertain"}`))
		result <- v
	}()
	<-transport.entered
	statusDone := make(chan bool, 1)
	go func() { a.Handle(ctx, "app.status", json.RawMessage(`{}`)); statusDone <- true }()
	responsive := false
	select {
	case <-statusDone:
		responsive = true
	case <-time.After(200 * time.Millisecond):
	}
	close(transport.release)
	v := <-result
	if !responsive {
		t.Error("transport wait blocks local status")
	}
	if v.(map[string]any)["message"].(Message).Status != "unknown" {
		t.Fatal("uncertain delivery not preserved")
	}
	retry := invoke(t, a, "message.send", map[string]any{"chat_id": "fixture", "text": "hello", "client_id": "uncertain"})
	if retry["message"].(map[string]any)["status"] != "unknown" {
		t.Fatal("unexpected retry")
	}
}
