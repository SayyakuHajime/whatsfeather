package profilevault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSyntheticOSKeyringAndEncryptedProfileTogether(t *testing.T) {
	if os.Getenv("PROFILEVAULT_OS_KEYRING_ACCEPTANCE") != "1" || os.Getenv("PROFILEVAULT_INTEGRATION") != "1" {
		t.Skip("explicit OS-keyring and FUSE opt-in required")
	}
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	if engine == "" {
		t.Fatal("exact vetted synthetic engine path is required")
	}
	identity, err := newMasterKey()
	if err != nil {
		t.Fatal("synthetic identity generation failed")
	}
	defer wipe(identity)
	appID := "org.whatsfeather.synthetic-vault-acceptance." + string(identity)
	profileID := "encrypted-profile-roundtrip"
	ring := &SecretToolKeyring{Executable: "/usr/bin/secret-tool", Timeout: 10 * time.Second}
	root := filepath.Join(t.TempDir(), "encrypted-profile")
	runtimeDir, err := os.MkdirTemp("", "pv-os-it-")
	if err != nil {
		t.Fatal("synthetic runtime creation failed")
	}
	opts := Options{RootDir: root, RuntimeDir: runtimeDir, AppID: appID, ProfileID: profileID,
		Keyring: ring, GocryptfsPath: engine, FuseUnmountPath: "/usr/bin/fusermount3"}
	var vault *Vault
	keyCleared := false
	clear := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "/usr/bin/secret-tool", "clear", "application", appID, "profile", profileID).Run()
	}
	t.Cleanup(func() {
		if vault != nil {
			if err := vault.Close(context.Background()); err != nil {
				t.Error("synthetic vault cleanup failed; key retained for recovery")
				return
			}
		}
		if !keyCleared {
			if err := clear(); err != nil {
				t.Error("synthetic vault key cleanup failed")
			}
		}
		if err := os.Remove(runtimeDir); err != nil {
			t.Error("synthetic runtime cleanup failed")
		}
	})
	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatal("new synthetic OS-keyring-backed vault failed to open")
	}
	marker := []byte("SYNTHETIC_OS_KEYRING_AND_VAULT_ACCEPTANCE")
	writeSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", marker)
	writeSyntheticSQLite(t, filepath.Join(vault.MountPath(), "device.db"), "device", marker)
	if err := os.WriteFile(filepath.Join(vault.MountPath(), "synthetic-media.bin"), marker, 0o600); err != nil {
		t.Fatal("synthetic encrypted media write failed")
	}
	if err := vault.Close(context.Background()); err != nil {
		t.Fatal("synthetic vault close failed")
	}
	vault = nil
	assertCiphertextHidesPlaintext(t, filepath.Join(root, "cipher"), [][]byte{marker})
	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatal("synthetic vault reopen with OS keyring failed")
	}
	assertSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", marker)
	assertSyntheticSQLite(t, filepath.Join(vault.MountPath(), "device.db"), "device", marker)
	media, err := os.ReadFile(filepath.Join(vault.MountPath(), "synthetic-media.bin"))
	if err != nil || !bytes.Equal(media, marker) {
		t.Fatal("synthetic encrypted media readback differs")
	}
	if err := vault.Close(context.Background()); err != nil {
		t.Fatal("synthetic final vault close failed")
	}
	vault = nil
	if err := clear(); err != nil {
		t.Fatal("exact synthetic vault-key removal failed")
	}
	keyCleared = true
	if _, err := ring.Lookup(context.Background(), appID, profileID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("synthetic vault-key absence readback failed")
	}
	if failedVault, err := Open(context.Background(), opts); !errors.Is(err, ErrKeyringUnavailable) || failedVault != nil {
		if failedVault != nil {
			_ = failedVault.Close(context.Background())
		}
		t.Fatal("missing OS key did not fail closed")
	}
	t.Log("actual OS keyring + encrypted history/device/media remount + missing-key rejection accepted on synthetic profile")
}
