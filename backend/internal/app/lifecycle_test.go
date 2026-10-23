package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type lifecycleOrderingTransport struct {
	mu                sync.Mutex
	connectCalls      int
	connectEntered    chan int
	disconnectEntered chan struct{}
	releaseDisconnect chan struct{}
}

func (t *lifecycleOrderingTransport) Connect(context.Context) error {
	t.mu.Lock()
	t.connectCalls++
	call := t.connectCalls
	t.mu.Unlock()
	t.connectEntered <- call
	return nil
}

func (t *lifecycleOrderingTransport) Disconnect() {
	t.disconnectEntered <- struct{}{}
	<-t.releaseDisconnect
}

func (t *lifecycleOrderingTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled")
}

func lifecycleInvoke(t *testing.T, a *App, method string) (any, error) {
	t.Helper()
	return a.Handle(context.Background(), method, json.RawMessage(`{}`))
}

func closeLifecycleChannel(ch chan struct{}) {
	defer func() { _ = recover() }()
	close(ch)
}

type gatedLifecycleTransport struct {
	mu                sync.Mutex
	connectCalls      int
	disconnectCalls   int
	connectEntered    chan int
	cancelObserved    chan struct{}
	releaseOldConnect chan struct{}
	disconnectEntered chan struct{}
	disconnectDone    chan struct{}
	releaseDisconnect chan struct{}
	onDisconnect      func()
	afterDisconnect   func()
}

func (t *gatedLifecycleTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	t.connectCalls++
	call := t.connectCalls
	t.mu.Unlock()
	t.connectEntered <- call
	if call == 1 {
		select {
		case <-ctx.Done():
			select {
			case <-t.cancelObserved:
			default:
				close(t.cancelObserved)
			}
			if t.releaseOldConnect != nil {
				<-t.releaseOldConnect
			}
			return errors.New("late canceled connect error")
		case <-t.releaseOldConnect:
			return nil
		}
	}
	return nil
}

func (t *gatedLifecycleTransport) Disconnect() {
	t.mu.Lock()
	t.disconnectCalls++
	t.mu.Unlock()
	t.disconnectEntered <- struct{}{}
	if t.onDisconnect != nil {
		t.onDisconnect()
	}
	<-t.releaseDisconnect
	if t.afterDisconnect != nil {
		t.afterDisconnect()
	}
	if t.disconnectDone != nil {
		t.disconnectDone <- struct{}{}
	}
}

func (t *gatedLifecycleTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled")
}

// qrDuringConnectTransport models an adapter that publishes QR/linking state
// before its generic Connect call returns successfully.
type qrDuringConnectTransport struct {
	app       *App
	published chan struct{}
}

func (t *qrDuringConnectTransport) Connect(context.Context) error {
	t.app.SetStatus("qr", "Scan this QR code", "synthetic-qr")
	close(t.published)
	return nil
}

func (t *qrDuringConnectTransport) Disconnect() {}
func (t *qrDuringConnectTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled")
}

func (t *gatedLifecycleTransport) disconnectCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disconnectCalls
}

func TestCanceledConnectSettlesBeforeTeardownAndNeedsExplicitRetry(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &gatedLifecycleTransport{
		connectEntered:    make(chan int, 2),
		cancelObserved:    make(chan struct{}),
		releaseOldConnect: make(chan struct{}),
		disconnectEntered: make(chan struct{}, 2),
		disconnectDone:    make(chan struct{}, 2),
		releaseDisconnect: make(chan struct{}),
	}
	a.SetTransport(transport)
	t.Cleanup(func() {
		closeLifecycleChannel(transport.releaseOldConnect)
		closeLifecycleChannel(transport.releaseDisconnect)
		_ = a.Close()
	})

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	if call := <-transport.connectEntered; call != 1 {
		t.Fatalf("first Connect call=%d, want 1", call)
	}
	response, err := lifecycleInvoke(t, a, "auth.disconnect")
	if err != nil {
		t.Fatal(err)
	}
	if response.(map[string]any)["disconnected"] != false || response.(map[string]any)["in_progress"] != true {
		t.Fatalf("disconnect response=%#v, want in-progress truth", response)
	}
	select {
	case <-transport.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel Connect")
	}
	select {
	case <-transport.disconnectEntered:
		t.Fatal("teardown started before canceled Connect returned")
	case <-time.After(50 * time.Millisecond):
	}

	_, err = lifecycleInvoke(t, a, "auth.connect")
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "lifecycle_busy" || appErr.Message != "Disconnect is still in progress; retry explicitly" {
		t.Fatalf("busy reconnect error=%v, want exact lifecycle_busy contract", err)
	}
	statusDone := make(chan struct{})
	go func() {
		_, _ = lifecycleInvoke(t, a, "app.status")
		close(statusDone)
	}()
	select {
	case <-statusDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("status blocked behind lifecycle I/O")
	}
	if _, err = lifecycleInvoke(t, a, "auth.disconnect"); err != nil {
		t.Fatal(err)
	}
	if got := transport.disconnectCount(); got != 0 {
		t.Fatalf("repeated disconnect launched teardown early: %d", got)
	}

	closeLifecycleChannel(transport.releaseOldConnect)
	select {
	case <-transport.disconnectEntered:
	case <-time.After(time.Second):
		t.Fatal("teardown did not start after Connect settled")
	}
	closeLifecycleChannel(transport.releaseDisconnect)
	select {
	case <-transport.disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("teardown did not finish")
	}

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal("explicit retry after teardown: ", err)
	}
	select {
	case call := <-transport.connectEntered:
		if call != 2 {
			t.Fatalf("retry Connect call=%d, want 2", call)
		}
		// A real adapter publishes Connected explicitly; a successful generic
		// Connect return is not itself an authentication event.
		a.SetStatus("connected", "synthetic connected event", "")
	case <-time.After(time.Second):
		t.Fatal("explicit retry did not start")
	}
	deadline := time.After(time.Second)
	for {
		status, statusErr := lifecycleInvoke(t, a, "app.status")
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.(map[string]any)["state"] == "connected" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("explicit retry never published connected state: %#v", status)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestConnectSuccessPreservesAdapterPublishedQR(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &qrDuringConnectTransport{app: a, published: make(chan struct{})}
	a.SetTransport(transport)
	t.Cleanup(func() { _ = a.Close() })

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.published:
	case <-time.After(time.Second):
		t.Fatal("Connect did not publish QR state")
	}

	deadline := time.After(time.Second)
	for {
		a.mu.Lock()
		settled := a.lifecycleDone == nil
		a.mu.Unlock()
		if settled {
			status, statusErr := lifecycleInvoke(t, a, "app.status")
			if statusErr != nil {
				t.Fatal(statusErr)
			}
			got := status.(map[string]any)
			if got["state"] != "qr" || got["qr_png"] != "synthetic-qr" {
				t.Fatalf("Connect fabricated status: %#v", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("Connect lifecycle did not settle")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCloseOwnsLifecycleAndClosesDBAfterTeardown(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &gatedLifecycleTransport{
		connectEntered:    make(chan int, 1),
		cancelObserved:    make(chan struct{}),
		disconnectEntered: make(chan struct{}, 1),
		disconnectDone:    make(chan struct{}, 1),
		releaseDisconnect: make(chan struct{}),
	}
	transport.onDisconnect = func() {
		transportErr := a.Record(context.Background(), Message{ID: "close-check-before", ChatID: "close-check", Body: "still open", Timestamp: 1, Status: "received"}, "Close check")
		if transportErr != nil {
			t.Errorf("lifecycle work saw closed database: %v", transportErr)
		}
	}
	transport.afterDisconnect = func() {
		transportErr := a.Record(context.Background(), Message{ID: "close-check-after", ChatID: "close-check", Body: "still open", Timestamp: 2, Status: "received"}, "Close check")
		if transportErr != nil {
			t.Errorf("lifecycle work saw closed database after teardown gate: %v", transportErr)
		}
	}
	a.SetTransport(transport)
	t.Cleanup(func() {
		closeLifecycleChannel(transport.releaseDisconnect)
		_ = a.Close()
	})
	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	<-transport.connectEntered

	firstClose := make(chan error, 1)
	go func() { firstClose <- a.Close() }()
	select {
	case <-transport.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel Connect")
	}
	select {
	case <-transport.disconnectEntered:
	case <-time.After(time.Second):
		t.Fatal("Close did not coordinate teardown")
	}
	secondClose := make(chan error, 1)
	go func() { secondClose <- a.Close() }()
	closeLifecycleChannel(transport.releaseDisconnect)
	for name, result := range map[string]<-chan error{"first Close": firstClose, "second Close": secondClose} {
		select {
		case err = <-result:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	if _, err = lifecycleInvoke(t, a, "auth.connect"); err == nil {
		t.Fatal("auth.connect accepted on closed app")
	} else if appErr, ok := err.(*Error); !ok || appErr.Code != "app_closing" {
		t.Fatalf("closed connect error=%v, want app_closing", err)
	}
	if _, err = lifecycleInvoke(t, a, "auth.disconnect"); err == nil {
		t.Fatal("auth.disconnect accepted on closed app")
	}
	if err = a.Close(); err != nil {
		t.Fatal("repeated Close: ", err)
	}
}

func TestReconnectDoesNotEnterConnectBeforePriorDisconnectFinishes(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &lifecycleOrderingTransport{
		connectEntered:    make(chan int, 2),
		disconnectEntered: make(chan struct{}, 2),
		releaseDisconnect: make(chan struct{}),
	}
	a.SetTransport(transport)
	t.Cleanup(func() {
		closeLifecycleChannel(transport.releaseDisconnect)
		_ = a.Close()
	})

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-transport.connectEntered:
		if call != 1 {
			t.Fatalf("first Connect call=%d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first Connect did not start")
	}
	if _, err = lifecycleInvoke(t, a, "auth.disconnect"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.disconnectEntered:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not start")
	}

	if _, err = lifecycleInvoke(t, a, "auth.connect"); err == nil {
		t.Fatal("reconnect entered while prior teardown was blocked")
	}
	select {
	case call := <-transport.connectEntered:
		t.Fatalf("Connect call %d entered before prior Disconnect finished", call)
	case <-time.After(100 * time.Millisecond):
	}
}
