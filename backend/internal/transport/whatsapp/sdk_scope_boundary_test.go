package whatsapp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const scopedConnectPrivacyBoundaryError = "scoped live-test Connect disabled: pinned whatsmeow has no complete enforceable pre-event scope boundary; SDK-side persistence can occur before adapter event filtering"

func TestScopedConnectFailsClosedBeforeDeviceStoreAccess(t *testing.T) {
	t.Run("does not create a profile or device database", func(t *testing.T) {
		profileDir := filepath.Join(t.TempDir(), "not-created")
		adapter := NewWithLiveTestScope(nil, profileDir, &LiveTestScope{})

		err := adapter.Connect(context.Background())
		if err == nil || err.Error() != scopedConnectPrivacyBoundaryError {
			t.Fatalf("scoped Connect error = %v, want exact fail-closed privacy-boundary error", err)
		}
		if _, statErr := os.Lstat(profileDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("scoped Connect touched profile directory: Lstat error = %v", statErr)
		}
		if _, statErr := os.Lstat(filepath.Join(profileDir, "device.db")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("scoped Connect touched device database path: Lstat error = %v", statErr)
		}
	})

	t.Run("fails before inspecting an existing device database path", func(t *testing.T) {
		profileDir := t.TempDir()
		databasePath := filepath.Join(profileDir, "device.db")
		if err := os.Symlink("sentinel-target", databasePath); err != nil {
			t.Fatal(err)
		}
		adapter := NewWithLiveTestScope(nil, profileDir, &LiveTestScope{})

		err := adapter.Connect(context.Background())
		if err == nil || err.Error() != scopedConnectPrivacyBoundaryError {
			t.Fatalf("scoped Connect error = %v, want exact fail-closed privacy-boundary error", err)
		}
		if target, readErr := os.Readlink(databasePath); readErr != nil || target != "sentinel-target" {
			t.Fatalf("scoped Connect changed existing device database path: target=%q err=%v", target, readErr)
		}
	})
}

func TestOrdinaryLiveConnectStillHonorsCanceledContextBeforeDeviceStore(t *testing.T) {
	profileDir := filepath.Join(t.TempDir(), "ordinary-profile")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := New(nil, profileDir).Connect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ordinary live Connect error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(profileDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ordinary canceled Connect touched profile directory: Lstat error = %v", statErr)
	}
}
