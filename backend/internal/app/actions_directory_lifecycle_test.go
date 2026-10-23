package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type gatedContactDirectory struct {
	started chan struct{}
	release <-chan struct{}
}

func (d *gatedContactDirectory) Connect(context.Context) error { return nil }
func (d *gatedContactDirectory) Disconnect()                   {}
func (d *gatedContactDirectory) Account(context.Context) (Account, error) {
	return Account{Available: true, Phone: "19995550123", JID: "19995550123@s.whatsapp.net", State: "connected"}, nil
}
func (d *gatedContactDirectory) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (d *gatedContactDirectory) wait() {
	d.started <- struct{}{}
	<-d.release
}
func (d *gatedContactDirectory) ResolveContact(_ context.Context, phone string) (Contact, bool, error) {
	d.wait()
	return Contact{JID: "15551234567@s.whatsapp.net", Name: "Synthetic contact", Phone: phone, Verified: true}, true, nil
}
func (d *gatedContactDirectory) ListContacts(context.Context) ([]Contact, error) {
	d.wait()
	return []Contact{{JID: "15551234567@s.whatsapp.net", Name: "Synthetic contact"}}, nil
}

func TestContactDirectoryRPCsHoldLifecycleOpenUntilReturn(t *testing.T) {
	for _, method := range []string{"chat.open", "contact.list"} {
		t.Run(method, func(t *testing.T) {
			a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
			if err != nil {
				t.Fatal(err)
			}
			releaseCh := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
			defer release()
			directory := &gatedContactDirectory{started: make(chan struct{}, 1), release: releaseCh}
			a.SetTransport(directory)
			a.SetStatus("connected", "synthetic fixture", "")

			params := json.RawMessage(`{}`)
			if method == "chat.open" {
				params = json.RawMessage(`{"recipient":"+15551234567"}`)
			}
			requestDone := make(chan error, 1)
			go func() {
				_, requestErr := a.Handle(context.Background(), method, params)
				requestDone <- requestErr
			}()
			select {
			case <-directory.started:
			case <-time.After(3 * time.Second):
				release()
				_ = a.Close()
				t.Fatal("contact directory operation did not reach the gated transport")
			}

			closeDone := make(chan error, 1)
			go func() { closeDone <- a.Close() }()
			select {
			case closeErr := <-closeDone:
				release()
				<-requestDone
				if closeErr != nil {
					t.Fatalf("close returned an error: %v", closeErr)
				}
				t.Fatal("Close returned while the contact directory RPC was still active")
			case <-time.After(50 * time.Millisecond):
			}

			release()
			select {
			case requestErr := <-requestDone:
				if requestErr == nil {
					t.Fatal("RPC should fail rather than mutate a closing profile")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("contact directory RPC did not finish after release")
			}
			select {
			case closeErr := <-closeDone:
				if closeErr != nil {
					t.Fatalf("close returned an error: %v", closeErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Close did not finish after the contact directory RPC returned")
			}
		})
	}
}
