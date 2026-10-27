package profilevault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type successfulButMismatchedKeyring struct {
	key         []byte
	lookupCalls int
	storeCalls  int
}

func (k *successfulButMismatchedKeyring) Lookup(_ context.Context, _, _ string) ([]byte, error) {
	k.lookupCalls++
	if k.key == nil {
		return nil, ErrKeyNotFound
	}
	return append([]byte(nil), k.key...), nil
}

func (k *successfulButMismatchedKeyring) Store(_ context.Context, _, _ string, _ []byte) error {
	k.storeCalls++
	k.key = []byte("synthetic-wrong-persisted-key")
	return nil
}

func TestOpenVerifiesStoredKeyBeforeInvokingEncryptionEngine(t *testing.T) {
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
	keyring := &successfulButMismatchedKeyring{}

	_, err := Open(context.Background(), Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic", ProfileID: "profile.synthetic",
		Keyring: keyring, GocryptfsPath: engine,
	})
	if !errors.Is(err, ErrKeyringStateUnknown) {
		t.Fatalf("Open error = %v, want ErrKeyringStateUnknown", err)
	}
	if keyring.lookupCalls != 2 || keyring.storeCalls != 1 {
		t.Fatalf("keyring calls = lookup %d/store %d, want 2/1", keyring.lookupCalls, keyring.storeCalls)
	}
	if _, err := os.Stat(engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("engine ran before keyring value was verified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, cipherDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified key caused a cipher directory: %v", err)
	}
}
