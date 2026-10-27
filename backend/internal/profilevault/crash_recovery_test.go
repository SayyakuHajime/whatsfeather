package profilevault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGocryptfsCrashReopenRecoversVerifiedOwnedMount(t *testing.T) {
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
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("opt-in integration requires /dev/fuse: %v", err)
	}

	base, err := os.MkdirTemp("", "pv-crash-reopen-")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "profile")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyringPath := filepath.Join(base, "test-only-keyring")
	readyPath := filepath.Join(base, "child-ready.json")
	child := exec.Command(os.Args[0], "-test.run=^TestGocryptfsCrashReopenHelper$")
	child.Env = append(os.Environ(),
		"PROFILEVAULT_CRASH_REOPEN_HELPER=1",
		"PROFILEVAULT_CRASH_REOPEN_ROOT="+root,
		"PROFILEVAULT_CRASH_REOPEN_RUNTIME="+runtimeDir,
		"PROFILEVAULT_CRASH_REOPEN_KEYRING="+keyringPath,
		"PROFILEVAULT_CRASH_REOPEN_READY="+readyPath,
		"PROFILEVAULT_GCRYPTFS="+engine,
		"PROFILEVAULT_FUSERMOUNT="+unmount,
	)
	if err := child.Start(); err != nil {
		_ = os.RemoveAll(base)
		t.Fatal(err)
	}
	childWaited := false
	var ready crashReopenReady
	var vault *Vault
	var blocker *exec.Cmd
	var lease runtimeLease
	var leaseInfo os.FileInfo
	var leaseBytes []byte
	var leasePath string
	t.Cleanup(func() {
		safeToRemoveBase := true
		if blocker != nil && blocker.Process != nil {
			_ = blocker.Process.Kill()
			_ = blocker.Wait()
		}
		if !childWaited && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		if vault != nil {
			if err := vault.Close(context.Background()); err != nil {
				t.Errorf("cleanup could not close recovered synthetic vault: %v", err)
				safeToRemoveBase = false
			}
		}
		if ready.MountPath != "" {
			ref, mounted, findErr := findMount(ready.MountPath)
			if findErr != nil {
				safeToRemoveBase = false
			} else if mounted {
				runtimeFD, openErr := openDirectory(runtimeDir, false)
				if openErr != nil {
					safeToRemoveBase = false
				} else {
					current, currentInfo, readErr := readLeaseAt(runtimeFD, filepath.Base(ready.MountPath)+leaseSuffix, runtimeDir)
					currentBytes, marshalErr := json.Marshal(current)
					leaseMatches := readErr == nil && marshalErr == nil && leaseBytes != nil && current == lease &&
						bytes.Equal(leaseBytes, currentBytes) && sameFileIdentity(currentInfo, leaseInfo)
					if leaseMatches && ref.id == ready.MountID && ref.id == current.MountID &&
						ref.fsType == "fuse.gocryptfs" && ref.owner == os.Geteuid() && !ref.allowOther &&
						ref.source == filepath.Join(root, cipherDirName) {
						if err := normalUnmount(context.Background(), unmount, ready.MountPath); err != nil {
							safeToRemoveBase = false
						}
					} else {
						safeToRemoveBase = false
					}
					_, stillMounted, mountErr := findMount(ready.MountPath)
					if mountErr != nil || stillMounted {
						safeToRemoveBase = false
					} else if leaseMatches {
						if removeLeaseTarget(runtimeDir, current, true) != nil ||
							removeLeaseAt(runtimeFD, filepath.Base(ready.MountPath)+leaseSuffix, runtimeDir, current, currentInfo) != nil {
							safeToRemoveBase = false
						}
					}
					_ = syscall.Close(runtimeFD)
				}
			}
		}
		if entries, err := os.ReadDir(runtimeDir); err != nil || len(entries) != 0 {
			safeToRemoveBase = false
		}
		if safeToRemoveBase {
			_ = os.RemoveAll(base)
		} else {
			t.Errorf("preserving synthetic crash-test resources because mount ownership or cleanup was not verified")
		}
	})

	waitForFile(t, readyPath, 20*time.Second)
	data, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &ready); err != nil || ready.MountPath == "" || ready.MountID <= 0 {
		t.Fatalf("child mount handoff is invalid: ready=%+v err=%v", ready, err)
	}
	if filepath.Dir(ready.MountPath) != runtimeDir {
		t.Fatal("child mount target is outside the private runtime directory")
	}
	if ref, mounted, err := findMount(ready.MountPath); err != nil || !mounted || ref.id != ready.MountID ||
		ref.fsType != "fuse.gocryptfs" || ref.owner != os.Geteuid() || ref.allowOther ||
		ref.source != filepath.Join(root, cipherDirName) || !ref.statfsValid {
		t.Fatalf("child mount identity changed before crash: mounted=%v ref=%+v err=%v", mounted, ref, err)
	}
	leasePath = filepath.Join(runtimeDir, filepath.Base(ready.MountPath)+leaseSuffix)
	leaseBytes, err = os.ReadFile(leasePath)
	if err != nil {
		t.Fatalf("read durable runtime lease before crash: %v", err)
	}
	runtimeFD, err := openDirectory(runtimeDir, false)
	if err != nil {
		t.Fatal(err)
	}
	lease, leaseInfo, err = readLeaseAt(runtimeFD, filepath.Base(leasePath), runtimeDir)
	_ = syscall.Close(runtimeFD)
	if err != nil || lease.State != leaseMounted || lease.MountID != ready.MountID || lease.OwnerPID != child.Process.Pid || lease.EnginePID <= 0 {
		t.Fatalf("durable lease does not bind the live synthetic owner and mount: lease=%+v err=%v", lease, err)
	}
	testKey, err := os.ReadFile(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(leaseBytes, testKey) {
		t.Fatal("synthetic master key appeared in the durable runtime lease")
	}

	blockerReady := filepath.Join(base, "blocker-ready")
	blocker = exec.Command("/bin/sh", "-c", `cd "$1" || exit 71; : > "$2"; exec sleep 60`, "stale-mount-blocker", ready.MountPath, blockerReady)
	if err := blocker.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, blockerReady, 3*time.Second)

	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("synthetic vault owner did not terminate by SIGKILL: %v", err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("synthetic vault owner termination = %v, want SIGKILL", err)
		}
	}
	childWaited = true
	if _, err := os.ReadFile(filepath.Join(ready.MountPath, "history.db")); err == nil {
		t.Fatal("dead synthetic FUSE mount exposed history data or a plaintext fallback")
	}
	if ref, mounted, err := findMount(ready.MountPath); err != nil || !mounted || ref.id != ready.MountID {
		t.Fatalf("abrupt owner death did not leave the expected stale FUSE mount: mounted=%v ref=%+v err=%v", mounted, ref, err)
	}

	keyring := &persistentTestKeyring{path: keyringPath}
	opts := Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic.crash-reopen", ProfileID: "profile.synthetic.crash-reopen",
		Keyring: keyring, GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second,
	}
	missingKeyOptions := opts
	missingKeyOptions.Keyring = &persistentTestKeyring{path: filepath.Join(base, "missing-test-keyring")}
	if _, err := Open(context.Background(), missingKeyOptions); !errors.Is(err, ErrKeyringUnavailable) {
		t.Fatalf("missing synthetic key Open error = %v, want ErrKeyringUnavailable", err)
	}
	assertStaleMountAndLeaseUntouched(t, ready, leaseBytes, leasePath)
	preIdentityLease := lease
	preIdentityLease.State = leasePreparing
	preIdentityLease.MountID = 0
	preIdentityLease.EnginePID = 0
	preIdentityLease.EngineStartTicks = 0
	preIdentityBytes, err := json.Marshal(preIdentityLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, preIdentityBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("mount without durable mount-ID record Open error = %v, want ErrStaleMountUnverified", err)
	}
	assertStaleMountStillMounted(t, ready)
	if err := os.WriteFile(leasePath, leaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, manifestFileName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(`{"corrupt":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrUnrecognizedRoot) {
		t.Fatalf("corrupt manifest Open error = %v, want ErrUnrecognizedRoot", err)
	}
	assertStaleMountAndLeaseUntouched(t, ready, leaseBytes, leasePath)
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var mismatchedLease runtimeLease
	if err := json.Unmarshal(leaseBytes, &mismatchedLease); err != nil {
		t.Fatal(err)
	}
	mismatchedLease.MountID++
	tamperedLease, err := json.Marshal(mismatchedLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, tamperedLease, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("mismatched mount identity Open error = %v, want ErrStaleMountUnverified", err)
	}
	assertStaleMountStillMounted(t, ready)
	if err := os.WriteFile(leasePath, []byte(`{"corrupt":}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("corrupt lease Open error = %v, want ErrStaleMountUnverified", err)
	}
	assertStaleMountStillMounted(t, ready)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(readyPath, leasePath); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("replaced lease symlink Open error = %v, want ErrStaleMountUnverified", err)
	}
	assertStaleMountStillMounted(t, ready)
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, leaseBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), opts); !errors.Is(err, ErrUnmountFailed) {
		t.Fatalf("busy stale-mount recovery error = %v, want ErrUnmountFailed", err)
	}
	assertStaleMountStillMounted(t, ready)
	if err := blocker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = blocker.Wait()
	blocker = nil

	vault, err = Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("reopen after synthetic owner crash: %v", err)
	}
	if _, mounted, err := findMount(ready.MountPath); err != nil || mounted {
		t.Fatalf("verified stale FUSE mount survived successful reopen: mounted=%v err=%v", mounted, err)
	}
	if got, err := os.ReadFile(filepath.Join(vault.MountPath(), "media", "synthetic-crash-media.bin")); err != nil || !bytes.Equal(got, []byte("SYNTHETIC_CRASH_REOPEN_MEDIA_604a")) {
		t.Fatalf("synthetic media did not survive crash/reopen: got=%q err=%v", got, err)
	}
	assertSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", []byte("SYNTHETIC_CRASH_REOPEN_HISTORY_92e1"))

	beforeLookups := keyring.lookupCalls
	if second, err := Open(context.Background(), opts); !errors.Is(err, ErrVaultBusy) || second != nil {
		if second != nil {
			_ = second.Close(context.Background())
		}
		t.Fatalf("second Open while recovered vault is active = %v, want ErrVaultBusy", err)
	}
	if keyring.lookupCalls != beforeLookups {
		t.Fatal("busy second Open reached the keyring before owner-lock acquisition")
	}
	if err := vault.Close(context.Background()); err != nil {
		t.Fatalf("close recovered vault: %v", err)
	}
	vault = nil
	entries, err := os.ReadDir(runtimeDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runtime directory retained stale mount resources after close: entries=%v err=%v", entryNames(entries), err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, manifestFileName)); err != nil || strings.Contains(string(raw), string(keyring.key)) {
		t.Fatalf("master key appeared in the versioned vault manifest: err=%v", err)
	}
	t.Log("actual FUSE crash→reopen recovered the exact stale owned mount and retained synthetic history/media")
}

func assertStaleMountStillMounted(t *testing.T, ready crashReopenReady) {
	t.Helper()
	ref, mounted, err := findMount(ready.MountPath)
	if err != nil || !mounted || ref.id != ready.MountID || ref.fsType != "fuse.gocryptfs" || ref.owner != os.Geteuid() || ref.allowOther {
		t.Fatalf("fail-closed check changed the stale owned mount: mounted=%v ref=%+v err=%v", mounted, ref, err)
	}
}

func assertStaleMountAndLeaseUntouched(t *testing.T, ready crashReopenReady, leaseBytes []byte, leasePath string) {
	t.Helper()
	assertStaleMountStillMounted(t, ready)
	got, err := os.ReadFile(leasePath)
	if err != nil || !bytes.Equal(got, leaseBytes) {
		t.Fatalf("rejected Open changed the stale lease: err=%v", err)
	}
}

func TestGocryptfsOpenPreservesOtherLiveProfileMount(t *testing.T) {
	if os.Getenv("PROFILEVAULT_INTEGRATION") != "1" {
		t.Skip("opt in with PROFILEVAULT_INTEGRATION=1 and exact helper paths")
	}
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	unmount := os.Getenv("PROFILEVAULT_FUSERMOUNT")
	if engine == "" || unmount == "" {
		t.Fatal("opt-in integration requires PROFILEVAULT_GCRYPTFS and PROFILEVAULT_FUSERMOUNT")
	}
	base, err := os.MkdirTemp("", "pv-unrelated-live-")
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	firstOpts := Options{RootDir: filepath.Join(base, "first-profile"), RuntimeDir: runtimeDir,
		AppID: "app.synthetic.unrelated-first", ProfileID: "profile.synthetic.unrelated-first",
		Keyring: &persistentTestKeyring{path: filepath.Join(base, "first-keyring")}, GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second}
	secondOpts := Options{RootDir: filepath.Join(base, "second-profile"), RuntimeDir: runtimeDir,
		AppID: "app.synthetic.unrelated-second", ProfileID: "profile.synthetic.unrelated-second",
		Keyring: &persistentTestKeyring{path: filepath.Join(base, "second-keyring")}, GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second}
	var first, second *Vault
	t.Cleanup(func() {
		if second != nil {
			if err := second.Close(context.Background()); err != nil {
				t.Errorf("close second synthetic profile: %v", err)
			}
		}
		if first != nil {
			if err := first.Close(context.Background()); err != nil {
				t.Errorf("close first synthetic profile: %v", err)
			}
		}
		_ = os.RemoveAll(base)
	})
	first, err = Open(context.Background(), firstOpts)
	if err != nil {
		t.Fatalf("open first synthetic profile: %v", err)
	}
	marker := []byte("SYNTHETIC_UNRELATED_LIVE_PROFILE_247a")
	if err := os.WriteFile(filepath.Join(first.MountPath(), "profile-marker"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	firstPath, firstID := first.MountPath(), first.MountID()
	second, err = Open(context.Background(), secondOpts)
	if err != nil {
		t.Fatalf("open second profile in shared runtime: %v", err)
	}
	ref, mounted, err := findMount(firstPath)
	if err != nil || !mounted || ref.id != firstID || ref.fsType != "fuse.gocryptfs" || ref.owner != os.Geteuid() || ref.allowOther {
		t.Fatalf("opening another profile changed the unrelated live mount: mounted=%v ref=%+v err=%v", mounted, ref, err)
	}
	if got, err := os.ReadFile(filepath.Join(firstPath, "profile-marker")); err != nil || !bytes.Equal(got, marker) {
		t.Fatalf("unrelated live mount stopped serving its synthetic data: got=%q err=%v", got, err)
	}
	if second.MountPath() == firstPath || second.MountID() == firstID {
		t.Fatal("independent profiles unexpectedly shared one mount identity")
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("close second synthetic profile: %v", err)
	}
	second = nil
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("close first synthetic profile: %v", err)
	}
	first = nil
	entries, err := os.ReadDir(runtimeDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("shared runtime was not empty after both owned closes: entries=%v err=%v", entryNames(entries), err)
	}
}

func TestGocryptfsCrashReopenHelper(t *testing.T) {
	if os.Getenv("PROFILEVAULT_CRASH_REOPEN_HELPER") != "1" {
		return
	}
	root := os.Getenv("PROFILEVAULT_CRASH_REOPEN_ROOT")
	runtimeDir := os.Getenv("PROFILEVAULT_CRASH_REOPEN_RUNTIME")
	keyringPath := os.Getenv("PROFILEVAULT_CRASH_REOPEN_KEYRING")
	readyPath := os.Getenv("PROFILEVAULT_CRASH_REOPEN_READY")
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	unmount := os.Getenv("PROFILEVAULT_FUSERMOUNT")
	vault, err := Open(context.Background(), Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic.crash-reopen", ProfileID: "profile.synthetic.crash-reopen",
		Keyring:       &persistentTestKeyring{path: keyringPath},
		GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("child synthetic vault open failed: %v", err)
	}
	writeSyntheticSQLite(t, filepath.Join(vault.MountPath(), "history.db"), "history", []byte("SYNTHETIC_CRASH_REOPEN_HISTORY_92e1"))
	if err := os.Mkdir(filepath.Join(vault.MountPath(), "media"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault.MountPath(), "media", "synthetic-crash-media.bin"), []byte("SYNTHETIC_CRASH_REOPEN_MEDIA_604a"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err := json.Marshal(crashReopenReady{MountPath: vault.MountPath(), MountID: vault.MountID()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readyPath, ready, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

type crashReopenReady struct {
	MountPath string `json:"mount_path"`
	MountID   int    `json:"mount_id"`
}

type persistentTestKeyring struct {
	path        string
	lookupCalls int
	key         []byte
}

// persistentTestKeyring is a private, file-backed fixture for cross-process
// FUSE tests only; it is never used by production code or as a keyring example.
func (k *persistentTestKeyring) Lookup(_ context.Context, _, _ string) ([]byte, error) {
	k.lookupCalls++
	key, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	k.key = append(k.key[:0], key...)
	return append([]byte(nil), key...), nil
}

func (k *persistentTestKeyring) Store(_ context.Context, _, _ string, key []byte) error {
	if err := os.WriteFile(k.path, key, 0o600); err != nil {
		return err
	}
	k.key = append(k.key[:0], key...)
	return nil
}
