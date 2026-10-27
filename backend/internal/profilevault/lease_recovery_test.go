package profilevault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRecoverRuntimeLeaseLeavesUnrelatedProfileAlone(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	foreignRoot := filepath.Join(fixture.base, "foreign-profile")
	if err := os.Mkdir(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(foreignRoot, cipherDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	foreignRootFD, err := openDirectory(foreignRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(foreignRootFD)
	foreignCipherFD, exists, err := openCipherDir(foreignRootFD)
	if err != nil || !exists {
		t.Fatalf("open synthetic foreign cipher: exists=%v err=%v", exists, err)
	}
	defer syscall.Close(foreignCipherFD)

	mountPath, err := os.MkdirTemp(fixture.runtimePath, "wf-pv-")
	if err != nil {
		t.Fatal(err)
	}
	mountInfo, err := os.Lstat(mountPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID, _ := identityFromFD(fixture.runtimeFD)
	foreignRootID, _ := identityFromFD(foreignRootFD)
	foreignCipherID, _ := identityFromFD(foreignCipherFD)
	mountID, _ := identityFromInfo(mountInfo)
	bootID, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	foreignOpts := fixture.opts
	foreignOpts.RootDir = foreignRoot
	foreignOpts.AppID = "app.synthetic.decoy"
	foreignOpts.ProfileID = "profile.synthetic.decoy"
	foreignManifest := vaultManifest{Format: manifestFormat, Version: manifestVersion, VaultID: "ffeeddccbbaa99887766554433221100", State: manifestReadyState}
	ownerStart, err := currentProcessStartTicks()
	if err != nil {
		t.Fatal(err)
	}
	decoy := makeRuntimeLease(fixture.runtimePath, mountPath, foreignRoot, runtimeID, foreignRootID, foreignCipherID, mountID, foreignOpts, foreignManifest, bootID, os.Getpid(), ownerStart)
	decoyInfo, err := persistLeaseAt(fixture.runtimeFD, decoy.MountName+leaseSuffix, decoy, runtimeLease{}, nil)
	if err != nil {
		t.Fatalf("persist synthetic unrelated lease: %v", err)
	}

	if err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3"); err != nil {
		t.Fatalf("recovery with an unrelated live profile lease: %v", err)
	}
	if _, err := os.Lstat(mountPath); err != nil {
		t.Fatalf("unrelated mountpoint was removed: %v", err)
	}
	if got, info, err := readLeaseAt(fixture.runtimeFD, decoy.MountName+leaseSuffix, fixture.runtimePath); err != nil || got != decoy || !sameFileIdentity(info, decoyInfo) {
		t.Fatalf("unrelated lease changed: got=%+v err=%v", got, err)
	}
}

func TestRecoverRuntimeLeaseRefusesLiveOwner(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	_, _ = fixture.makeLease(t, leasePreparing, os.Getpid(), 0, 0)

	err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3")
	if !errors.Is(err, ErrVaultBusy) {
		t.Fatalf("recovery error = %v, want ErrVaultBusy", err)
	}
	if _, err := os.Lstat(fixture.mountPath); err != nil {
		t.Fatalf("live owner's mountpoint was modified: %v", err)
	}
	if got, _, err := readLeaseAt(fixture.runtimeFD, fixture.lease.MountName+leaseSuffix, fixture.runtimePath); err != nil || got != fixture.lease {
		t.Fatalf("live owner's lease changed: got=%+v err=%v", got, err)
	}
}

func TestRecoverRuntimeLeaseRefusesReplacedMountpoint(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	_, _ = fixture.makeLease(t, leaseMounted, 2147483646, 2147483645, 678)
	replacementPath := fixture.mountPath + ".replacement"
	if err := os.Mkdir(replacementPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.mountPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, fixture.mountPath); err != nil {
		t.Fatal(err)
	}
	err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3")
	if !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("recovery error = %v, want ErrStaleMountUnverified", err)
	}
	if info, err := os.Lstat(fixture.mountPath); err != nil || !info.IsDir() {
		t.Fatalf("replacement directory was removed or changed: info=%v err=%v", info, err)
	}
	if got, _, err := readLeaseAt(fixture.runtimeFD, fixture.lease.MountName+leaseSuffix, fixture.runtimePath); err != nil || got != fixture.lease {
		t.Fatalf("lease for replaced target changed: got=%+v err=%v", got, err)
	}
}

func TestRecoverRuntimeLeaseRejectsTamperedRecordWithoutCleanup(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	lease, _ := fixture.makeLease(t, leasePreparing, 2147483646, 0, 0)
	leasePath := filepath.Join(fixture.runtimePath, lease.MountName+leaseSuffix)
	if err := os.WriteFile(leasePath, []byte(`{"format":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3")
	if !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("recovery error = %v, want ErrStaleMountUnverified", err)
	}
	if _, err := os.Lstat(fixture.mountPath); err != nil {
		t.Fatalf("tampered lease caused mountpoint cleanup: %v", err)
	}
	if got, err := os.ReadFile(leasePath); err != nil || string(got) != `{"format":"wrong"}` {
		t.Fatalf("tampered lease changed: got=%q err=%v", got, err)
	}
}

func TestRecoverRuntimeLeaseTreatsCurrentCipherBindingAsRelevant(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	lease, _ := fixture.makeLease(t, leasePreparing, 2147483646, 0, 0)
	lease.RootPath = filepath.Join(fixture.base, "forged-root")
	lease.CipherPath = filepath.Join(lease.RootPath, cipherDirName)
	lease.RootDev++
	lease.RootIno++
	lease.ProfileHash = profileIdentityHash("app.synthetic.forged", "profile.synthetic.forged")
	leasePath := filepath.Join(fixture.runtimePath, lease.MountName+leaseSuffix)
	payload, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leasePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	err = recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3")
	if !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("recovery error = %v, want ErrStaleMountUnverified", err)
	}
	if _, err := os.Lstat(fixture.mountPath); err != nil {
		t.Fatalf("forged root/profile binding caused target cleanup: %v", err)
	}
	if got, err := os.ReadFile(leasePath); err != nil || string(got) != string(payload) {
		t.Fatalf("forged lease changed: got=%q err=%v", got, err)
	}
}

func TestRecoverPreparingLeaseCleansOnlyItsUnmountedDirectory(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	_, _ = fixture.makeLease(t, leasePreparing, 2147483646, 0, 0)

	if err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3"); err != nil {
		t.Fatalf("recovery of an unmounted preparation lease: %v", err)
	}
	if _, err := os.Lstat(fixture.mountPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified unmounted preparation directory remains: %v", err)
	}
	if entries, err := os.ReadDir(fixture.runtimePath); err != nil || len(entries) != 0 {
		t.Fatalf("preparation lease cleanup left runtime entries: entries=%v err=%v", entryNames(entries), err)
	}
}

func TestRecoverRuntimeLeaseBlocksUnknownOrphanWithoutOwnershipRecord(t *testing.T) {
	fixture := newLeaseRecoveryFixture(t)
	orphan := filepath.Join(fixture.runtimePath, "wf-pv-orphan0001")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	err := recoverRuntimeLeases(fixture.opts, fixture.rootPath, fixture.rootFD, fixture.manifest, "/unused/fusermount3")
	if !errors.Is(err, ErrStaleMountUnverified) {
		t.Fatalf("recovery error = %v, want ErrStaleMountUnverified", err)
	}
	if info, err := os.Lstat(orphan); err != nil || !info.IsDir() {
		t.Fatalf("unowned orphan was removed or changed: info=%v err=%v", info, err)
	}
}

func TestProcessIdentityRejectsReusedPIDByStartTime(t *testing.T) {
	start, err := currentProcessStartTicks()
	if err != nil {
		t.Fatal(err)
	}
	if alive, err := processIdentityAlive(os.Getpid(), start); err != nil || !alive {
		t.Fatalf("current process identity = alive %v, err %v; want alive", alive, err)
	}
	if alive, err := processIdentityAlive(os.Getpid(), start+1); err != nil || alive {
		t.Fatalf("mismatched PID start time = alive %v, err %v; want not alive", alive, err)
	}
}

type leaseRecoveryFixture struct {
	base        string
	runtimePath string
	rootPath    string
	runtimeFD   int
	rootFD      int
	opts        Options
	manifest    vaultManifest
	lease       runtimeLease
	leaseInfo   os.FileInfo
	mountPath   string
}

func newLeaseRecoveryFixture(t *testing.T) *leaseRecoveryFixture {
	t.Helper()
	base := t.TempDir()
	runtimePath := filepath.Join(base, "runtime")
	rootPath := filepath.Join(base, "profile")
	if err := os.Mkdir(runtimePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, cipherDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeFD, err := openDirectory(runtimePath, false)
	if err != nil {
		t.Fatal(err)
	}
	rootFD, err := openDirectory(rootPath, false)
	if err != nil {
		_ = syscall.Close(runtimeFD)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Close(rootFD)
		_ = syscall.Close(runtimeFD)
	})
	fixture := &leaseRecoveryFixture{
		base: base, runtimePath: runtimePath, rootPath: rootPath, runtimeFD: runtimeFD, rootFD: rootFD,
		opts:     Options{RootDir: rootPath, RuntimeDir: runtimePath, AppID: "app.synthetic.lease", ProfileID: "profile.synthetic.lease"},
		manifest: vaultManifest{Format: manifestFormat, Version: manifestVersion, VaultID: "00112233445566778899aabbccddeeff", State: manifestReadyState},
	}
	return fixture
}

func (fixture *leaseRecoveryFixture) makeLease(t *testing.T, state string, ownerPID, enginePID, mountID int) (runtimeLease, os.FileInfo) {
	t.Helper()
	if fixture.mountPath != "" {
		if err := os.Remove(fixture.mountPath); err != nil {
			t.Fatal(err)
		}
	}
	mountPath, err := os.MkdirTemp(fixture.runtimePath, "wf-pv-")
	if err != nil {
		t.Fatal(err)
	}
	fixture.mountPath = mountPath
	mountInfo, err := os.Lstat(mountPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID, err := identityFromFD(fixture.runtimeFD)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := identityFromFD(fixture.rootFD)
	if err != nil {
		t.Fatal(err)
	}
	cipherFD, exists, err := openCipherDir(fixture.rootFD)
	if err != nil || !exists {
		t.Fatalf("open cipher directory: exists=%v err=%v", exists, err)
	}
	cipherID, err := identityFromFD(cipherFD)
	_ = syscall.Close(cipherFD)
	if err != nil {
		t.Fatal(err)
	}
	mountIDInfo, err := identityFromInfo(mountInfo)
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	ownerStart := uint64(1)
	if ownerPID == os.Getpid() {
		ownerStart, err = currentProcessStartTicks()
		if err != nil {
			t.Fatal(err)
		}
	}
	engineStart := uint64(0)
	if enginePID != 0 {
		engineStart = 1
	}
	lease := makeRuntimeLease(fixture.runtimePath, mountPath, fixture.rootPath, runtimeID, rootID, cipherID, mountIDInfo, fixture.opts, fixture.manifest, bootID, ownerPID, ownerStart)
	lease.State = state
	lease.EnginePID = enginePID
	lease.EngineStartTicks = engineStart
	lease.MountID = mountID
	if state == leaseMounted {
		if err := os.Chmod(mountPath, 0); err != nil {
			t.Fatal(err)
		}
	}
	leaseInfo, err := persistLeaseAt(fixture.runtimeFD, lease.MountName+leaseSuffix, lease, runtimeLease{}, nil)
	if err != nil {
		t.Fatalf("persist test lease: %v", err)
	}
	fixture.lease, fixture.leaseInfo = lease, leaseInfo
	return lease, leaseInfo
}
