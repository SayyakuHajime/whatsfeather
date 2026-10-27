package profilevault

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRejectsSymlinkCipherBeforeKeyringOrEngine(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "recognized-vault")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := []byte("SYNTHETIC_OUTSIDE_DATA")
	if err := os.WriteFile(filepath.Join(outside, "marker"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(vaultManifest{
		Format: manifestFormat, Version: manifestVersion,
		VaultID: "00112233445566778899aabbccddeeff", State: manifestReadyState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, manifestFileName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, cipherDirName)); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	engineMarker := filepath.Join(base, "engine-was-called")
	engine := writeExecutable(t, filepath.Join(base, "gocryptfs"), "#!/bin/sh\nprintf called > \"$ENGINE_MARKER\"\nexit 19\n")
	t.Setenv("ENGINE_MARKER", engineMarker)
	syntheticKey, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	defer wipe(syntheticKey)
	keyring := &memoryKeyring{key: syntheticKey}

	_, err = Open(context.Background(), Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic", ProfileID: "profile.synthetic",
		Keyring: keyring, GocryptfsPath: engine,
	})
	if !errors.Is(err, ErrIncompleteVault) {
		t.Fatalf("Open error = %v, want ErrIncompleteVault", err)
	}
	if keyring.lookupCalls != 0 || keyring.storeCalls != 0 {
		t.Fatalf("symlink cipher reached keyring: lookups=%d stores=%d", keyring.lookupCalls, keyring.storeCalls)
	}
	if _, err := os.Stat(engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink cipher reached engine: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outside, "marker"))
	if err != nil || string(got) != string(marker) {
		t.Fatalf("symlink target was modified: %q err=%v", got, err)
	}
}
