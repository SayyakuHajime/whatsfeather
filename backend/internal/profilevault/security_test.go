package profilevault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDoesNotCreateManifestWhenOrphanKeyAlreadyExists(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "empty-profile")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	engineMarker := filepath.Join(base, "engine-was-called")
	engine := writeExecutable(t, filepath.Join(base, "gocryptfs"), "#!/bin/sh\nprintf called > \"$ENGINE_MARKER\"\nexit 19\n")
	t.Setenv("ENGINE_MARKER", engineMarker)
	keyring := &memoryKeyring{key: []byte("synthetic-orphan-key")}

	_, err := Open(context.Background(), Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic", ProfileID: "profile.synthetic",
		Keyring: keyring, GocryptfsPath: engine,
	})
	if !errors.Is(err, ErrKeyringConflict) {
		t.Fatalf("Open error = %v, want ErrKeyringConflict", err)
	}
	if _, err := os.Stat(filepath.Join(root, manifestFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan key caused a manifest write: %v", err)
	}
	if keyring.lookupCalls != 1 || keyring.storeCalls != 0 {
		t.Fatalf("keyring calls = lookup %d/store %d, want 1/0", keyring.lookupCalls, keyring.storeCalls)
	}
	if _, err := os.Stat(engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan key reached engine (marker stat error %v)", err)
	}
}
