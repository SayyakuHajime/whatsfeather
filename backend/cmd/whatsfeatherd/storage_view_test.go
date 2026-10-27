package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
)

func TestEncryptedStorageViewCannotClaimAnotherAppsProfileRoot(t *testing.T) {
	base := shortBootstrapTestDir(t)
	appRoot := filepath.Join(base, "app-profile")
	ciphertextRoot := filepath.Join(base, "ciphertext")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "wf-pv-test")
	for _, dir := range []string{appRoot, ciphertextRoot, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := app.Open(appRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SetEncryptedStorageView(ciphertextRoot, runtimeDir, mount, 42); err == nil {
		t.Fatal("encrypted storage view accepted a mount that does not contain this app's database")
	}
	value, err := store.Handle(context.Background(), "app.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(map[string]any)
	if _, exists := status["storage_view"]; exists {
		t.Fatalf("rejected foreign profile was still reported as encrypted: %#v", status["storage_view"])
	}
}

func TestLiveBootstrapStatusConfirmsOnlyItsEncryptedMountedProfile(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "ciphertext")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "wf-pv-synthetic")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		return &fakeVault{mountPath: mount, mountID: 42}, nil
	}
	deps.openApp = app.Open
	transport := &recordingTransport{}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		return transport
	}
	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir,
		socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := runtime.Close(context.Background()); closeErr != nil {
			t.Errorf("close synthetic runtime: %v", closeErr)
		}
	})
	if transport.connectCalls != 0 {
		t.Fatalf("storage-view setup auto-connected %d times", transport.connectCalls)
	}
	value, err := runtime.app.Handle(context.Background(), "app.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := value.(map[string]any)
	view, ok := status["storage_view"].(map[string]any)
	if !ok {
		t.Fatalf("live status omitted its encrypted storage view: %#v", status)
	}
	want := map[string]string{
		"mode":            "encrypted",
		"ciphertext_root": root,
		"runtime_dir":     runtimeDir,
		"profile_root":    mount,
		"mount_id":        "42",
	}
	for field, expected := range want {
		if got, _ := view[field].(string); got != expected {
			t.Errorf("storage_view.%s=%q, want %q", field, got, expected)
		}
	}
	if view["version"] != 1 {
		t.Errorf("storage_view.version=%v, want 1", view["version"])
	}
	if viewID, _ := view["view_id"].(string); len(viewID) != 32 {
		t.Errorf("storage_view.view_id=%q, want an ephemeral 32-character identifier", viewID)
	}
}
