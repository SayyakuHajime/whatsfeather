package profilevault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Explicit parent-owned acceptance: only a newly generated synthetic identity
// is accessed, then the exact created item is removed and absence read back.
func TestSyntheticOSSecretServiceRoundTrip(t *testing.T) {
	if os.Getenv("PROFILEVAULT_OS_KEYRING_ACCEPTANCE") != "1" {
		t.Skip("explicit opt-in required for synthetic OS-keyring acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	identity, err := newMasterKey()
	if err != nil {
		t.Fatal("synthetic identity generation failed")
	}
	defer wipe(identity)
	key, err := newMasterKey()
	if err != nil {
		t.Fatal("synthetic key generation failed")
	}
	defer wipe(key)
	appID := "org.whatsfeather.synthetic-acceptance." + string(identity)
	profileID := "secret-service-roundtrip"
	ring := &SecretToolKeyring{Executable: "/usr/bin/secret-tool", Timeout: 10 * time.Second}
	clear := func() error {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		cmd := exec.CommandContext(cleanupCtx, "/usr/bin/secret-tool", "clear", "application", appID, "profile", profileID)
		return cmd.Run()
	}
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		if err := clear(); err != nil {
			t.Error("synthetic keyring-item cleanup failed")
		}
	})
	if _, err := ring.Lookup(ctx, appID, profileID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("new synthetic identity was not absent")
	}
	if err := ring.Store(ctx, appID, profileID, key); err != nil {
		t.Fatal("synthetic keyring store failed")
	}
	got, err := ring.Lookup(ctx, appID, profileID)
	defer wipe(got)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("synthetic keyring readback differs")
	}
	if err := clear(); err != nil {
		t.Fatal("synthetic keyring clear failed")
	}
	if _, err := ring.Lookup(ctx, appID, profileID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatal("synthetic keyring item still exists after exact cleanup")
	}
	cleaned = true
	t.Log("OS Secret Service synthetic store/readback/exact-delete/absence accepted; no existing keyring item queried")
}
