package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type callbackStatusSnapshot struct {
	state, info, qr string
	revision        int64
}

func callbackStatus(t *testing.T, a *App) callbackStatusSnapshot {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	return callbackStatusSnapshot{state: a.state, info: a.info, qr: a.qr, revision: a.revision}
}

type supersededContextTransport struct {
	app             *App
	mu              sync.Mutex
	calls           int
	firstContext    chan context.Context
	secondEntered   chan struct{}
	releaseSecond   chan struct{}
	secondPublished chan bool
}

func (t *supersededContextTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if call == 1 {
		t.firstContext <- ctx
		return errors.New("controlled first connect failure")
	}
	if call != 2 {
		return errors.New("unexpected connect attempt")
	}
	close(t.secondEntered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.releaseSecond:
		published := t.app.SetStatusContext(ctx, "qr", "fresh QR", "fresh-qr")
		t.secondPublished <- published
		return nil
	}
}

func (t *supersededContextTransport) Disconnect() {}
func (t *supersededContextTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled")
}

func TestRetryCancelsFailedAttemptBeforeFreshCallbacks(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &supersededContextTransport{
		app:             a,
		firstContext:    make(chan context.Context, 1),
		secondEntered:   make(chan struct{}),
		releaseSecond:   make(chan struct{}),
		secondPublished: make(chan bool, 1),
	}
	a.SetTransport(transport)
	t.Cleanup(func() {
		closeLifecycleChannel(transport.releaseSecond)
		_ = a.Close()
	})

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	var oldContext context.Context
	select {
	case oldContext = <-transport.firstContext:
	case <-time.After(time.Second):
		t.Fatal("first Connect did not capture its context")
	}
	deadline := time.After(time.Second)
	for {
		value, statusErr := lifecycleInvoke(t, a, "app.status")
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		status := value.(map[string]any)
		if status["state"] == "error" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("first Connect did not report error: %#v", status)
		case <-time.After(time.Millisecond):
		}
	}

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	if oldContext.Err() == nil {
		t.Fatal("failed attempt context was not canceled before retry callbacks")
	}
	select {
	case <-transport.secondEntered:
	case <-time.After(time.Second):
		t.Fatal("retry did not enter second Connect")
	}

	before := callbackStatus(t, a)
	if before.state != "connecting" {
		t.Fatalf("retry status=%#v, want connecting before fresh QR", before)
	}
	if transport.app.SetStatusContext(oldContext, "stale", "stale status", "stale-qr") {
		t.Fatal("superseded failed-attempt callback was accepted")
	}
	if got := callbackStatus(t, a); got != before {
		t.Fatalf("superseded callback changed status: got %#v want %#v", got, before)
	}

	close(transport.releaseSecond)
	select {
	case published := <-transport.secondPublished:
		if !published {
			t.Fatal("fresh retry callback was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("second Connect did not publish fresh QR")
	}
	got := callbackStatus(t, a)
	if got.state != "qr" || got.qr != "fresh-qr" {
		t.Fatalf("fresh retry status=%#v, want QR", got)
	}
}

func TestContextStatusMutationBoundaries(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.SetStatus("latest", "latest status", "latest-qr")
	before := callbackStatus(t, a)
	if !a.SetStatusContext(context.Background(), "active", "active status", "active-qr") {
		t.Fatal("active context status mutation was rejected")
	}
	if got := callbackStatus(t, a); got.state != "active" || got.info != "active status" || got.qr != "active-qr" || got.revision != before.revision+1 {
		t.Fatalf("active status mutation=%#v, want one committed revision", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before = callbackStatus(t, a)
	if a.SetStatusContext(ctx, "stale", "stale status", "stale-qr") {
		t.Fatal("canceled status mutation succeeded")
	}
	if got := callbackStatus(t, a); got != before {
		t.Fatalf("canceled status mutation changed state: got %#v want %#v", got, before)
	}
}

func TestCanceledStatusQueuedBehindAppMutexIsRejected(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetStatus("latest", "latest status", "latest-qr")
	before := callbackStatus(t, a)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan bool, 1)
	a.mu.Lock()
	go func() {
		close(started)
		result <- a.SetStatusContext(ctx, "stale", "stale status", "stale-qr")
	}()
	<-started
	cancel()
	a.mu.Unlock()

	select {
	case committed := <-result:
		if committed {
			t.Fatal("queued canceled status mutation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("queued status mutation did not settle")
	}
	if got := callbackStatus(t, a); got != before {
		t.Fatalf("queued canceled status mutation changed state: got %#v want %#v", got, before)
	}
}

func TestCanceledRecordAndReceiptDoNotMutate(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.Record(context.Background(), Message{ID: "receipt", ChatID: "chat", Body: "fixture", Timestamp: 1, Outgoing: true, Status: "accepted"}, "Chat"); err != nil {
		t.Fatal(err)
	}
	before := callbackStatus(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Record(ctx, Message{ID: "stale", ChatID: "chat", Body: "stale", Timestamp: 2, Status: "received"}, "Chat"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Record error=%v, want context.Canceled", err)
	}
	a.Receipt(ctx, "chat", []string{"receipt"}, "read")
	if got := callbackStatus(t, a); got != before {
		t.Fatalf("canceled data callbacks changed revision: got %#v want %#v", got, before)
	}
	var status string
	if err := a.db.QueryRow("SELECT status FROM messages WHERE id=? AND chat_id=?", "receipt", "chat").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("canceled receipt changed stored status to %q", status)
	}
}

func TestContextMutationsRejectAfterClose(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	a.SetStatus("latest", "latest status", "latest-qr")
	before := callbackStatus(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.SetStatusContext(context.Background(), "closed", "closed", "closed-qr") {
		t.Fatal("status mutation succeeded after close")
	}
	if got := callbackStatus(t, a); got != before {
		t.Fatalf("after-close status changed: got %#v want %#v", got, before)
	}
	if err := a.Record(context.Background(), Message{ID: "closed", ChatID: "chat", Body: "closed", Timestamp: 1, Status: "received"}, "Chat"); err == nil {
		t.Fatal("Record succeeded after close")
	}
}
