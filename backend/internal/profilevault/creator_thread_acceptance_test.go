package profilevault

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Pdeathsig is tied to the Linux creating OS thread, not the Go process.
// A vault returned to another goroutine must outlive that thread.
func TestVaultOutlivesCreatingGoThread(t *testing.T) {
	if os.Getenv("PROFILEVAULT_INTEGRATION") != "1" {
		t.Skip("explicit FUSE integration opt-in required")
	}
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	if engine == "" {
		t.Fatal("exact synthetic helper path required")
	}
	runtimeDir, err := os.MkdirTemp("", "pv-thread-it-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(runtimeDir)
	opts := Options{RootDir: filepath.Join(t.TempDir(), "profile"), RuntimeDir: runtimeDir,
		AppID: "synthetic.thread.acceptance", ProfileID: "synthetic-thread",
		Keyring: &memoryKeyring{}, GocryptfsPath: engine, FuseUnmountPath: "/usr/bin/fusermount3"}
	type result struct {
		vault *Vault
		err   error
	}
	ready := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		// Deliberately return without UnlockOSThread: Go retires this OS
		// thread while the process remains alive, as its documented API permits.
		vault, err := Open(context.Background(), opts)
		ready <- result{vault: vault, err: err}
	}()
	got := <-ready
	if got.err != nil {
		t.Fatal("synthetic vault failed to initialize")
	}
	t.Cleanup(func() {
		if err := got.vault.Close(context.Background()); err != nil {
			t.Error("synthetic creating-thread test cleanup failed")
		}
	})
	select {
	case <-got.vault.process.done:
		t.Fatal("vault helper died with creating OS thread while owner process is still alive")
	case <-time.After(time.Second):
	}
	if err := os.WriteFile(filepath.Join(got.vault.MountPath(), "synthetic-thread-marker"), []byte("SYNTHETIC_ONLY"), 0o600); err != nil {
		t.Fatal("vault became unusable after creating thread exited")
	}
}
