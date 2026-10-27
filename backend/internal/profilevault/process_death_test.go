package profilevault

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGocryptfsParentDeathLeavesNoPlaintextFallback(t *testing.T) {
	if os.Getenv("PROFILEVAULT_INTEGRATION") != "1" {
		t.Skip("opt in with PROFILEVAULT_INTEGRATION=1 and exact helper paths")
	}
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	unmount := os.Getenv("PROFILEVAULT_FUSERMOUNT")
	if engine == "" || unmount == "" {
		t.Fatal("opt-in integration requires PROFILEVAULT_GCRYPTFS and PROFILEVAULT_FUSERMOUNT")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("opt-in integration requires /dev/fuse: %v", err)
	}
	base, err := os.MkdirTemp("", "pv-parent-death-")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "profile")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeFD, runtimeErr := openDirectory(runtimeDir, false)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	ready := filepath.Join(base, "ready")
	mountPathFile := filepath.Join(base, "mount-path")
	child := exec.Command(os.Args[0], "-test.run=^TestGocryptfsParentDeathHelper$")
	child.Env = append(os.Environ(),
		"PROFILEVAULT_PARENT_DEATH_HELPER=1",
		"PROFILEVAULT_PARENT_DEATH_ROOT="+root,
		"PROFILEVAULT_PARENT_DEATH_RUNTIME="+runtimeDir,
		"PROFILEVAULT_PARENT_DEATH_READY="+ready,
		"PROFILEVAULT_PARENT_DEATH_MOUNTPATH="+mountPathFile,
		"PROFILEVAULT_GCRYPTFS="+engine,
		"PROFILEVAULT_FUSERMOUNT="+unmount,
	)
	if err := child.Start(); err != nil {
		_ = syscall.Close(runtimeFD)
		_ = os.Remove(runtimeDir)
		_ = os.Remove(base)
		t.Fatal(err)
	}
	childWaited := false
	mountPath := ""
	var lease runtimeLease
	var leaseInfo os.FileInfo
	t.Cleanup(func() {
		if !childWaited && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		if pathBytes, err := os.ReadFile(mountPathFile); err == nil {
			mountPath = strings.TrimSpace(string(pathBytes))
		}
		if mountPath != "" {
			ref, mounted, findErr := findMount(mountPath)
			if findErr == nil && mounted && lease.State == leaseMounted && ref.id == lease.MountID &&
				ref.fsType == "fuse.gocryptfs" && ref.owner == os.Geteuid() && !ref.allowOther &&
				ref.source == filepath.Join(root, cipherDirName) && lease.OwnerPID == child.Process.Pid {
				_ = normalUnmount(context.Background(), unmount, mountPath)
			}
			if _, mounted, _ := findMount(mountPath); !mounted && lease.State == leaseMounted && lease.MountID > 0 {
				if removeLeaseTarget(runtimeDir, lease, true) == nil {
					_ = removeLeaseAt(runtimeFD, lease.MountName+leaseSuffix, runtimeDir, lease, leaseInfo)
				}
			}
		}
		_ = syscall.Close(runtimeFD)
		_ = os.Remove(runtimeDir)
		_ = os.Remove(ready)
		_ = os.Remove(mountPathFile)
		_ = os.Remove(base)
	})
	waitForFile(t, ready, 10*time.Second)
	pathBytes, err := os.ReadFile(mountPathFile)
	if err != nil {
		t.Fatal(err)
	}
	mountPath = strings.TrimSpace(string(pathBytes))
	if mountPath == "" || filepath.Dir(mountPath) != runtimeDir {
		t.Fatal("child returned a mount path outside its private runtime directory")
	}
	lease, leaseInfo, err = readLeaseAt(runtimeFD, filepath.Base(mountPath)+leaseSuffix, runtimeDir)
	if err != nil || lease.MountID <= 0 || lease.State != leaseMounted || lease.OwnerPID != child.Process.Pid {
		t.Fatalf("child did not leave a durable exact mount lease: lease=%+v err=%v", lease, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("parent-death helper did not terminate as expected: %v", err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() {
			t.Fatalf("parent-death helper did not die from the requested signal: %v", err)
		}
	}
	childWaited = true
	fallback := filepath.Join(mountPath, "plaintext-fallback-must-not-exist")
	if err := os.WriteFile(fallback, []byte("SYNTHETIC_ONLY"), 0o600); err == nil {
		_ = os.Remove(fallback)
		t.Fatal("dead gocryptfs mount accepted a plaintext write")
	}
	if err := normalUnmount(context.Background(), unmount, mountPath); err != nil {
		t.Fatalf("normal cleanup of the dead synthetic FUSE mount failed: %v", err)
	}
	info, err := os.Lstat(mountPath)
	mode := os.FileMode(0)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err != nil || mode != 0 {
		t.Fatalf("underlying mountpoint after parent death has mode %#o, err=%v; want 000", mode, err)
	}
	if err := os.WriteFile(fallback, []byte("SYNTHETIC_ONLY"), 0o600); err == nil {
		_ = os.Remove(fallback)
		t.Fatal("unmount exposed a writable plaintext fallback directory")
	}
}

func TestGocryptfsParentDeathHelper(t *testing.T) {
	if os.Getenv("PROFILEVAULT_PARENT_DEATH_HELPER") != "1" {
		return
	}
	root := os.Getenv("PROFILEVAULT_PARENT_DEATH_ROOT")
	runtimeDir := os.Getenv("PROFILEVAULT_PARENT_DEATH_RUNTIME")
	ready := os.Getenv("PROFILEVAULT_PARENT_DEATH_READY")
	mountPathFile := os.Getenv("PROFILEVAULT_PARENT_DEATH_MOUNTPATH")
	engine := os.Getenv("PROFILEVAULT_GCRYPTFS")
	unmount := os.Getenv("PROFILEVAULT_FUSERMOUNT")
	vault, err := Open(context.Background(), Options{
		RootDir: root, RuntimeDir: runtimeDir,
		AppID: "app.synthetic.parent-death", ProfileID: "profile.synthetic.parent-death",
		Keyring: &memoryKeyring{}, GocryptfsPath: engine, FuseUnmountPath: unmount,
		CommandTimeout: 15 * time.Second, MountTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("child synthetic vault open failed: %v", err)
	}
	if err := os.WriteFile(mountPathFile, []byte(vault.MountPath()), 0o600); err != nil {
		t.Fatalf("child mount path handoff failed: %v", err)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatalf("child readiness handoff failed: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}
