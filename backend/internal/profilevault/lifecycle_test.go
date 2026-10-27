package profilevault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationStartedIntentIsDurablyStoredInTheExistingVaultManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	rootFile, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootFile.Close()
	if err := verifyRootPath(root, int(rootFile.Fd())); err != nil {
		info, _ := os.Stat(root)
		t.Fatalf("fixture root does not satisfy the vault ownership path: %v mode=%v", err, info.Mode())
	}
	manifest := vaultManifest{Format: manifestFormat, Version: manifestVersion, VaultID: strings.Repeat("a", 32), State: manifestReadyState}
	if err := writeManifestAt(int(rootFile.Fd()), manifest, true); err != nil {
		t.Fatal(err)
	}
	vault := &Vault{rootPath: root, rootFile: rootFile}
	publisher, ok := any(vault).(interface{ MarkMigrationStarted() error })
	if !ok {
		t.Fatal("Vault has no durable migration-intent publication API")
	}
	if err := publisher.MarkMigrationStarted(); err != nil {
		t.Fatalf("publish destination-bound migration intent: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, manifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil || decoded["migration_started"] != true {
		t.Fatalf("vault manifest does not retain the durable migration intent: %s err=%v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != manifestFileName {
		t.Fatalf("migration metadata created an extra unrecognized vault-root entry: entries=%v err=%v", entries, err)
	}
}

func TestOpenInitializesOnceAndReusesStoredKeyAfterMountFailure(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "vault")
	calls := filepath.Join(base, "engine-calls")
	engine := writeExecutable(t, filepath.Join(base, "gocryptfs"), `#!/bin/sh
printf '%s\n' "$1" >> "$ENGINE_CALLS"
IFS= read -r pass || exit 31
case "$1" in
  -init)
    printf '%s' "$pass" | sha256sum | cut -d ' ' -f1 > "$4/init-key.sha256"
    printf 'synthetic-config' > "$4/gocryptfs.conf"
    exit 0 ;;
  *) exit 0 ;;
esac
`)
	t.Setenv("ENGINE_CALLS", calls)
	keyring := &memoryKeyring{}
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		RootDir:       root,
		RuntimeDir:    runtimeDir,
		AppID:         "app.synthetic",
		ProfileID:     "profile.synthetic",
		Keyring:       keyring,
		GocryptfsPath: engine,
	}

	for attempt := 0; attempt < 2; attempt++ {
		_, err := Open(context.Background(), opts)
		if !errors.Is(err, ErrMountFailed) {
			t.Fatalf("Open attempt %d error = %v, want ErrMountFailed", attempt+1, err)
		}
	}
	if keyring.storeCalls != 1 || keyring.lookupCalls != 3 {
		t.Fatalf("keyring calls = lookup %d/store %d, want 3/1", keyring.lookupCalls, keyring.storeCalls)
	}
	engineCalls, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(engineCalls), "-init\n") != 1 || strings.Count(string(engineCalls), "-fg\n") != 2 {
		t.Fatalf("engine lifecycle calls = %q, want one init and two mount attempts", engineCalls)
	}
	keyHash := sha256.Sum256(keyring.key)
	gotHash, err := os.ReadFile(filepath.Join(root, "cipher", "init-key.sha256"))
	if err != nil || strings.TrimSpace(string(gotHash)) != hex.EncodeToString(keyHash[:]) {
		t.Fatalf("gocryptfs init did not receive the stored key (hash=%q, err=%v)", gotHash, err)
	}
	manifest, err := os.ReadFile(filepath.Join(root, manifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifest), string(keyring.key)) {
		t.Fatal("master key appeared in the versioned manifest")
	}
}
