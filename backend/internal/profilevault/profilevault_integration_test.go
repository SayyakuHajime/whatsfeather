package profilevault

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestGocryptfsIntegrationProfileLifecycle(t *testing.T) {
	if os.Getenv("PROFILEVAULT_INTEGRATION") != "1" {
		t.Skip("opt in with PROFILEVAULT_INTEGRATION=1 and exact helper paths")
	}
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	unmount := os.Getenv("PROFILEVAULT_FUSERMOUNT")
	if engine == "" || unmount == "" {
		t.Fatal("opt-in integration requires PROFILEVAULT_GCRYPTFS and PROFILEVAULT_FUSERMOUNT")
	}
	if err := validateExecutable(engine); err != nil {
		t.Fatalf("invalid configured synthetic gocryptfs helper: %v", err)
	}
	if err := validateExecutable(unmount); err != nil {
		t.Fatalf("invalid configured fusermount3 helper: %v", err)
	}
	fuseInfo, err := os.Stat("/dev/fuse")
	if err != nil || fuseInfo.Mode()&os.ModeDevice == 0 || fuseInfo.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("opt-in integration requires accessible /dev/fuse: %v", err)
	}

	root := filepath.Join(t.TempDir(), "profile-vault")
	runtimeBase, err := os.MkdirTemp("", "pv-it-")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(runtimeBase, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyring := &memoryKeyring{}
	opts := Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic.integration", ProfileID: "profile.synthetic.integration",
		Keyring: keyring, GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second,
	}
	var vault *Vault
	var blocker *exec.Cmd
	t.Cleanup(func() {
		if blocker != nil && blocker.Process != nil {
			_ = blocker.Process.Kill()
			_ = blocker.Wait()
		}
		if vault != nil {
			if err := vault.Close(context.Background()); err != nil {
				t.Errorf("test cleanup could not close owned vault: %v", err)
			}
		}
		_ = os.Remove(runtimeDir)
		_ = os.Remove(runtimeBase)
	})

	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("open synthetic vault: %v", err)
	}
	mountRef, mounted, err := findMount(vault.MountPath())
	if err != nil || !mounted || mountRef.fsType != "fuse.gocryptfs" || mountRef.owner != os.Geteuid() || mountRef.allowOther {
		t.Fatalf("mount is not private same-owner fuse.gocryptfs: mounted=%v ref=%+v err=%v", mounted, mountRef, err)
	}
	if vault.mountDirInfo.Mode().Perm() != 0 {
		t.Fatalf("underlying mountpoint is not inaccessible after daemon death: permissions=%#o", vault.mountDirInfo.Mode().Perm())
	}
	if vault.process.cmd.SysProcAttr == nil || vault.process.cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatal("gocryptfs child is not configured with a parent-death signal")
	}

	historyMarker := []byte("SYNTHETIC_PROFILEVAULT_HISTORY_MARKER_8a31")
	deviceMarker := []byte("SYNTHETIC_PROFILEVAULT_DEVICE_MARKER_b43f")
	mediaMarker := []byte("SYNTHETIC_PROFILEVAULT_MEDIA_MARKER_02cd")
	writeSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", historyMarker)
	writeSyntheticSQLite(t, filepath.Join(vault.MountPath(), "device.db"), "device", deviceMarker)
	mediaDir := filepath.Join(vault.MountPath(), "media")
	if err := os.Mkdir(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(mediaDir, "synthetic-media.bin")
	if err := os.WriteFile(mediaPath, bytes.Repeat(mediaMarker, 17), 0o600); err != nil {
		t.Fatal(err)
	}

	// A process whose cwd is in the mount makes a normal unmount busy. Close
	// must preserve process and owner lock so callers can retry after handles close.
	ready := filepath.Join(runtimeBase, "cwd-ready")
	blocker = exec.Command("/bin/sh", "-c", `cd "$1" || exit 71; : > "$2"; exec sleep 60`, "profilevault-blocker", vault.MountPath(), ready)
	if err := blocker.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, 3*time.Second)
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	closeErr := vault.Close(closeCtx)
	cancel()
	if !errors.Is(closeErr, ErrUnmountFailed) {
		t.Fatalf("busy Close error = %v, want ErrUnmountFailed", closeErr)
	}
	beforeLookups := keyring.lookupCalls
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrVaultBusy) {
		t.Fatalf("second Open during busy close = %v, want ErrVaultBusy", err)
	}
	if keyring.lookupCalls != beforeLookups {
		t.Fatal("second Open touched keyring before acquiring the owner lock")
	}
	_ = blocker.Process.Kill()
	_ = blocker.Wait()
	blocker = nil
	if err := vault.Close(context.Background()); err != nil {
		t.Fatalf("retry Close after releasing mount cwd: %v", err)
	}
	if err := vault.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close: %v", err)
	}

	assertCiphertextHidesPlaintext(t, filepath.Join(root, cipherDirName), [][]byte{historyMarker, deviceMarker, mediaMarker})
	keyCopy := append([]byte(nil), keyring.key...)
	wrongKey, err := newMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	keyring.key = wrongKey
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrMountFailed) {
		t.Fatalf("wrong key Open error = %v, want ErrMountFailed", err)
	}
	if entries, err := os.ReadDir(runtimeDir); err != nil || len(entries) != 0 {
		t.Fatalf("wrong key left a decrypted mount behind: entries=%v err=%v", entryNames(entries), err)
	}
	keyring.key = keyCopy
	wipe(wrongKey)

	configPath := filepath.Join(root, cipherDirName, "gocryptfs.conf")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configMode := configInfo.Mode().Perm()
	tampered := append([]byte(nil), config...)
	if len(tampered) == 0 {
		t.Fatal("synthetic gocryptfs config is unexpectedly empty")
	}
	tampered[len(tampered)-1] ^= 0x7f
	if err := os.Chmod(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrMountFailed) {
		t.Fatalf("tampered gocryptfs metadata Open error = %v, want ErrMountFailed", err)
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, configMode); err != nil {
		t.Fatal(err)
	}

	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("reopen synthetic vault: %v", err)
	}
	assertSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", historyMarker)
	assertSyntheticSQLite(t, filepath.Join(vault.MountPath(), "device.db"), "device", deviceMarker)
	if got, err := os.ReadFile(filepath.Join(vault.MountPath(), "media", "synthetic-media.bin")); err != nil || !bytes.Equal(got, bytes.Repeat(mediaMarker, 17)) {
		t.Fatalf("media did not survive close/remount: len=%d err=%v", len(got), err)
	}
	deadMountPath := vault.MountPath()
	if err := vault.process.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vault.process.done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned gocryptfs process did not terminate")
	}
	if _, err := os.ReadFile(filepath.Join(deadMountPath, "history.db")); err == nil {
		t.Fatal("dead gocryptfs mount exposed profile data or a plaintext fallback")
	}
	if err := vault.Close(context.Background()); err != nil {
		_ = normalUnmount(context.Background(), unmount, deadMountPath)
		t.Fatalf("Close could not clean up the owned dead FUSE mount: %v", err)
	}
	vault = nil
	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("open after dead-mount cleanup: %v", err)
	}
	if err := vault.Close(context.Background()); err != nil {
		t.Fatalf("final Close: %v", err)
	}
	vault = nil
	if err := os.Remove(runtimeDir); err != nil {
		t.Fatalf("runtime directory was not empty after close: %v", err)
	}
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(runtimeBase); err != nil {
		t.Fatalf("synthetic runtime root cleanup: %v", err)
	}
}

func writeSyntheticSQLite(t *testing.T, path, table string, value []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	if _, err := db.Exec(`CREATE TABLE ` + quoted + ` (value BLOB NOT NULL)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO `+quoted+` (value) VALUES (?)`, value); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSyntheticSQLite(t *testing.T, path, table string, want []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	var got []byte
	if err := db.QueryRow(`SELECT value FROM ` + quoted).Scan(&got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("synthetic %s database value mismatch: got %q err=%v", table, got, err)
	}
}

func assertCiphertextHidesPlaintext(t *testing.T, cipherDir string, markers [][]byte) {
	t.Helper()
	var total int64
	err := filepath.WalkDir(cipherDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected cipher entry type")
		}
		total += info.Size()
		if total > 16<<20 || info.Size() > 4<<20 {
			return fmt.Errorf("synthetic backing scan exceeded its file/aggregate cap")
		}
		if strings.Contains(path, "history.db") || strings.Contains(path, "device.db") || strings.Contains(path, "synthetic-media.bin") {
			t.Errorf("plaintext profile filename visible in cipher path: %s", filepath.Base(path))
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(content, []byte("SQLite format 3")) {
			t.Errorf("SQLite header visible in cipher file %s", filepath.Base(path))
		}
		for _, marker := range markers {
			if bytes.Contains(content, marker) {
				t.Errorf("synthetic plaintext marker visible in cipher file %s", filepath.Base(path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for synthetic test helper readiness file")
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
