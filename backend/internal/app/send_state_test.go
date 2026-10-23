package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sendLifecycleTransport struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
	sent    int64
	err     error
}

func (t *sendLifecycleTransport) Connect(context.Context) error { return nil }
func (t *sendLifecycleTransport) Disconnect()                   {}
func (t *sendLifecycleTransport) Send(ctx context.Context, _, _, _ string) (int64, error) {
	t.calls.Add(1)
	t.entered <- struct{}{}
	select {
	case <-t.release:
		return t.sent, t.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (t *sendLifecycleTransport) releaseSend() { t.once.Do(func() { close(t.release) }) }

func waitSendEntered(t *testing.T, tr *sendLifecycleTransport) {
	t.Helper()
	select {
	case <-tr.entered:
	case <-time.After(time.Second):
		t.Fatal("synthetic transport did not enter Send")
	}
}

func storedMessage(t *testing.T, a *App, id, chatID string) Message {
	t.Helper()
	var m Message
	if err := a.db.QueryRow("SELECT id,chat_id,body,timestamp,outgoing,status FROM messages WHERE id=? AND chat_id=?", id, chatID).
		Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status); err != nil {
		t.Fatal(err)
	}
	return m
}

func sendResult(t *testing.T, result <-chan struct {
	value any
	err   error
}) (any, error) {
	t.Helper()
	select {
	case result := <-result:
		return result.value, result.err
	case <-time.After(time.Second):
		t.Fatal("message.send did not complete")
		return nil, nil
	}
}

func startSend(a *App, chatID, text, clientID string) <-chan struct {
	value any
	err   error
} {
	result := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		value, err := a.Handle(context.Background(), "message.send", rawMessageSend(chatID, text, clientID))
		result <- struct {
			value any
			err   error
		}{value, err}
	}()
	return result
}

func rawMessageSend(chatID, text, clientID string) []byte {
	return []byte(`{"chat_id":"` + chatID + `","text":"` + text + `","client_id":"` + clientID + `"}`)
}

func TestMessageSendKeepsSameIDInOtherChatUntouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	tr := &sendLifecycleTransport{entered: make(chan struct{}, 1), release: make(chan struct{}), err: errors.New("synthetic ambiguous send")}
	closed := false
	t.Cleanup(func() {
		tr.releaseSend()
		if !closed {
			a.Close()
		}
	})
	a.SetTransport(tr)
	a.SetStatus("connected", "synthetic test", "")
	ctx := context.Background()
	if err := a.Record(ctx, Message{ID: "fixture", ChatID: "target", Body: "existing", Timestamp: 10, Status: "received"}, "Target"); err != nil {
		t.Fatal(err)
	}
	const outgoingID = "same-protocol-id"
	sentinel := Message{ID: outgoingID, ChatID: "other", Body: "sentinel body", Timestamp: 987654, Outgoing: false, Status: "delivered"}
	if err := a.Record(ctx, sentinel, "Other"); err != nil {
		t.Fatal(err)
	}

	result := startSend(a, "target", "outgoing body", outgoingID)
	waitSendEntered(t, tr)
	whileBlocked := storedMessage(t, a, outgoingID, "other")
	if whileBlocked != sentinel {
		t.Fatalf("other chat changed while Send was blocked: got %#v, want %#v", whileBlocked, sentinel)
	}
	if got := storedMessage(t, a, outgoingID, "target"); got.Status != "sending" {
		t.Fatalf("target was not marked sending: %#v", got)
	}

	tr.releaseSend()
	value, err := sendResult(t, result)
	if err != nil {
		t.Fatal(err)
	}
	message := value.(map[string]any)["message"].(Message)
	if message.ChatID != "target" || message.ID != outgoingID || message.Status != "unknown" {
		t.Fatalf("wrong returned message: %#v", message)
	}
	if got := storedMessage(t, a, outgoingID, "other"); got != sentinel {
		t.Fatalf("other chat changed after completion: got %#v, want %#v", got, sentinel)
	}

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := storedMessage(t, reopened, outgoingID, "target"); got.ChatID != "target" || got.Status != "unknown" {
		t.Fatalf("reopened target identity/status mismatch: %#v", got)
	}
	if got := storedMessage(t, reopened, outgoingID, "other"); got != sentinel {
		t.Fatalf("reopened other identity changed: got %#v, want %#v", got, sentinel)
	}
}

func TestMessageSendReceiptBeforeCompletionIsMonotonic(t *testing.T) {
	tests := []struct {
		name       string
		receipt    string
		ambiguous  bool
		outOfOrder bool
	}{
		{name: "delivered success", receipt: "delivered"},
		{name: "read success", receipt: "read"},
		{name: "delivered ambiguous", receipt: "delivered", ambiguous: true},
		{name: "read ambiguous", receipt: "read", ambiguous: true},
		{name: "read then delivered", receipt: "read", ambiguous: true, outOfOrder: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			a, err := Open(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			tr := &sendLifecycleTransport{entered: make(chan struct{}, 1), release: make(chan struct{}), sent: 123456}
			if test.ambiguous {
				tr.err = errors.New("synthetic ambiguous send")
			}
			closed := false
			t.Cleanup(func() {
				tr.releaseSend()
				if !closed {
					a.Close()
				}
			})
			a.SetTransport(tr)
			a.SetStatus("connected", "synthetic test", "")
			ctx := context.Background()
			if err := a.Record(ctx, Message{ID: "target-fixture", ChatID: "target", Body: "old", Timestamp: 10, Status: "received"}, "Target"); err != nil {
				t.Fatal(err)
			}
			if err := a.Record(ctx, Message{ID: "receipt-id", ChatID: "other", Body: "other", Timestamp: 20, Outgoing: true, Status: "accepted"}, "Other"); err != nil {
				t.Fatal(err)
			}

			result := startSend(a, "target", "hello", "receipt-id")
			waitSendEntered(t, tr)
			a.Receipt(ctx, "other", []string{"receipt-id"}, "read")
			if got := storedMessage(t, a, "receipt-id", "target"); got.Status != "sending" {
				t.Fatalf("other-chat receipt upgraded target: %#v", got)
			}
			a.Receipt(ctx, "target", []string{"receipt-id"}, test.receipt)
			if test.outOfOrder {
				a.Receipt(ctx, "target", []string{"receipt-id"}, "delivered")
			}
			if got := storedMessage(t, a, "receipt-id", "target"); got.Status != test.receipt {
				t.Fatalf("receipt was not durably recorded before completion: %#v", got)
			}

			tr.releaseSend()
			value, err := sendResult(t, result)
			if err != nil {
				t.Fatal(err)
			}
			message := value.(map[string]any)["message"].(Message)
			if message.Status != test.receipt || message.ChatID != "target" || message.ID != "receipt-id" {
				t.Fatalf("completion downgraded or misidentified message: %#v", message)
			}
			if got := storedMessage(t, a, "receipt-id", "target"); got.Status != test.receipt {
				t.Fatalf("durable status downgraded: %#v", got)
			}
			if got := storedMessage(t, a, "receipt-id", "other"); got.Status != "read" {
				t.Fatalf("other chat receipt was not isolated: %#v", got)
			}
			if tr.calls.Load() != 1 {
				t.Fatalf("expected one transport call, got %d", tr.calls.Load())
			}
			retry := startSend(a, "target", "hello", "receipt-id")
			value, err = sendResult(t, retry)
			if err != nil {
				t.Fatal(err)
			}
			if value.(map[string]any)["message"].(Message).Status != test.receipt || tr.calls.Load() != 1 {
				t.Fatalf("repeated client ID was resent or lost receipt: %#v, calls=%d", value, tr.calls.Load())
			}

			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			closed = true
			reopened, err := Open(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := storedMessage(t, reopened, "receipt-id", "target"); got.Status != test.receipt || got.ChatID != "target" {
				t.Fatalf("reopened target identity/status mismatch: %#v", got)
			}
			if got := storedMessage(t, reopened, "receipt-id", "other"); got.Status != "read" || got.Body != "other" || got.Timestamp != 20 {
				t.Fatalf("reopened other identity/status mismatch: %#v", got)
			}
		})
	}
}

func TestMessageSendUsesAcceptedOrUnknownWithoutReceipt(t *testing.T) {
	tests := []struct {
		name      string
		transport error
		want      string
	}{
		{name: "success", want: "accepted"},
		{name: "ambiguous", transport: errors.New("synthetic ambiguous send"), want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
			if err != nil {
				t.Fatal(err)
			}
			tr := &sendLifecycleTransport{entered: make(chan struct{}, 1), release: make(chan struct{}), sent: 123}
			tr.err = test.transport
			a.SetTransport(tr)
			a.SetStatus("connected", "synthetic test", "")
			ctx := context.Background()
			if err := a.Record(ctx, Message{ID: "fixture", ChatID: "target", Body: "old", Timestamp: 1, Status: "received"}, "Target"); err != nil {
				t.Fatal(err)
			}
			result := startSend(a, "target", "hello", "client-"+test.name)
			waitSendEntered(t, tr)
			tr.releaseSend()
			value, err := sendResult(t, result)
			if err != nil {
				t.Fatal(err)
			}
			if got := value.(map[string]any)["message"].(Message).Status; got != test.want {
				t.Fatalf("status=%q, want %q", got, test.want)
			}
			if tr.calls.Load() != 1 {
				t.Fatalf("transport calls=%d, want 1", tr.calls.Load())
			}
			a.Close()
		})
	}
}
