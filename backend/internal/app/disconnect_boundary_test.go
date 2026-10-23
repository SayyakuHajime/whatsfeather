package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type disconnectBoundaryTransport struct {
	mu              sync.Mutex
	disconnectCalls int
}

func (*disconnectBoundaryTransport) Connect(context.Context) error { return nil }
func (*disconnectBoundaryTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("disabled in synthetic boundary test")
}
func (t *disconnectBoundaryTransport) Disconnect() {
	t.mu.Lock()
	t.disconnectCalls++
	t.mu.Unlock()
}
func (t *disconnectBoundaryTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.disconnectCalls
}

func TestLocalDisconnectIsNotRemoteUnlinkOrLogout(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &disconnectBoundaryTransport{}
	a.SetTransport(transport)

	if _, err := a.db.Exec(`INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,?,0)`, "synthetic:linked", "Synthetic linked chat", "SL", "#123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?,?,?,?)`, "synthetic-message", "synthetic:linked", "Synthetic local history", 1, 0, "received"); err != nil {
		t.Fatal(err)
	}

	value, err := a.Handle(context.Background(), "app.capabilities", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	capabilities := value.(map[string]any)
	methods := capabilities["methods"].([]string)
	features := capabilities["features"].(map[string]map[string]any)
	if !containsString(methods, "auth.disconnect") || containsString(methods, "auth.unlink") || containsString(methods, "auth.logout") {
		t.Fatalf("auth methods blur local disconnect and remote unlink/logout: %v", methods)
	}
	localDisconnect, localPresent := features["local-disconnect"]
	localAvailable, localAvailabilityValid := localDisconnect["available"].(bool)
	if !localPresent || !localAvailabilityValid || !localAvailable {
		t.Fatalf("local disconnect unavailable or unreported: %#v", localDisconnect)
	}
	remoteUnlink, remotePresent := features["remote-device-unlink"]
	remoteAvailable, remoteAvailabilityValid := remoteUnlink["available"].(bool)
	remoteReason, remoteReasonValid := remoteUnlink["reason"].(string)
	if !remotePresent || !remoteAvailabilityValid || remoteAvailable || !remoteReasonValid ||
		!strings.Contains(strings.ToLower(remoteReason), "not implemented") {
		t.Fatalf("remote unlink must be explicitly unavailable: %#v", remoteUnlink)
	}

	a.mu.Lock()
	initialState, initialRevision := a.state, a.revision
	a.mu.Unlock()
	for _, method := range []string{"auth.unlink", "auth.logout"} {
		if _, err := a.Handle(context.Background(), method, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s unexpectedly succeeded", method)
		} else {
			var appErr *Error
			if !errors.As(err, &appErr) || appErr.Code != "unsupported" ||
				!strings.Contains(strings.ToLower(appErr.Message), "remote unlink/logout") {
				t.Fatalf("%s error=%v, want explicit unsupported remote unlink/logout", method, err)
			}
		}
	}
	if transport.count() != 0 {
		t.Fatalf("rejected remote operations touched transport %d times", transport.count())
	}
	a.mu.Lock()
	stateAfterRejected, revisionAfterRejected := a.state, a.revision
	a.mu.Unlock()
	if stateAfterRejected != initialState || revisionAfterRejected != initialRevision {
		t.Fatalf("rejected remote operations changed app state: %q/%d -> %q/%d", initialState, initialRevision, stateAfterRejected, revisionAfterRejected)
	}

	if _, err := a.Handle(context.Background(), "auth.disconnect", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for transport.count() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("local auth.disconnect did not call transport Disconnect exactly once; calls=%d", transport.count())
		}
		time.Sleep(time.Millisecond)
	}
	for {
		a.mu.Lock()
		state := a.state
		a.mu.Unlock()
		if state == "disconnected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local disconnect did not settle to disconnected; state=%q", state)
		}
		time.Sleep(time.Millisecond)
	}
	var body string
	if err := a.db.QueryRow(`SELECT body FROM messages WHERE chat_id=? AND id=?`, "synthetic:linked", "synthetic-message").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "Synthetic local history" {
		t.Fatalf("local disconnect changed cached history: %q", body)
	}
}
