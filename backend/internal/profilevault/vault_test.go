package profilevault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRejectsLegacyPlaintextBeforeKeyringOrEngine(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "profile")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := []byte("SQLite format 3\x00synthetic legacy profile data")
	if err := os.WriteFile(filepath.Join(root, "history.db"), plain, 0o600); err != nil {
		t.Fatal(err)
	}
	engineMarker := filepath.Join(base, "engine-was-called")
	engine := writeExecutable(t, filepath.Join(base, "gocryptfs"), "#!/bin/sh\nprintf called > \"$ENGINE_MARKER\"\nexit 19\n")
	t.Setenv("ENGINE_MARKER", engineMarker)
	keyring := &memoryKeyring{}

	_, err := Open(context.Background(), Options{
		RootDir:       root,
		RuntimeDir:    t.TempDir(),
		AppID:         "app.synthetic",
		ProfileID:     "profile.synthetic",
		Keyring:       keyring,
		GocryptfsPath: engine,
	})
	if !errors.Is(err, ErrLegacyPlaintext) {
		t.Fatalf("Open error = %v, want ErrLegacyPlaintext", err)
	}
	if keyring.lookupCalls != 0 || keyring.storeCalls != 0 {
		t.Fatalf("legacy root reached keyring: lookups=%d stores=%d", keyring.lookupCalls, keyring.storeCalls)
	}
	if _, err := os.Stat(engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy root reached gocryptfs (marker stat error %v)", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "history.db"))
	if err != nil || string(got) != string(plain) {
		t.Fatalf("legacy bytes changed: got %q, read error %v", got, err)
	}
}

func writeExecutable(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type memoryKeyring struct {
	key         []byte
	lookupCalls int
	storeCalls  int
	lookupErr   error
	storeErr    error
}

func (k *memoryKeyring) Lookup(_ context.Context, _, _ string) ([]byte, error) {
	k.lookupCalls++
	if k.lookupErr != nil {
		return nil, k.lookupErr
	}
	if k.key == nil {
		return nil, ErrKeyNotFound
	}
	return append([]byte(nil), k.key...), nil
}

func (k *memoryKeyring) Store(_ context.Context, _, _ string, key []byte) error {
	k.storeCalls++
	if k.storeErr != nil {
		return k.storeErr
	}
	k.key = append([]byte(nil), key...)
	return nil
}
