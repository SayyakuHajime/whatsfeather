package profilevault

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	leaseFormat        = "whatsfeather-profile-vault-runtime-lease"
	leaseVersion       = 1
	leasePreparing     = "preparing"
	leaseAttached      = "attached"
	leaseMounted       = "mounted"
	leaseSuffix        = ".lease"
	maxLeaseBytes      = 4096
	maxRuntimeLeases   = maxDirectoryItems / 2
	profileHashHexSize = sha256.Size * 2
)

type runtimeLease struct {
	Format           string `json:"format"`
	Version          int    `json:"version"`
	State            string `json:"state"`
	RuntimePath      string `json:"runtime_path"`
	MountName        string `json:"mount_name"`
	RootPath         string `json:"root_path"`
	CipherPath       string `json:"cipher_path"`
	ProfileHash      string `json:"profile_hash"`
	VaultID          string `json:"vault_id"`
	BootID           string `json:"boot_id"`
	UID              int    `json:"uid"`
	OwnerPID         int    `json:"owner_pid"`
	OwnerStartTicks  uint64 `json:"owner_start_ticks"`
	EnginePID        int    `json:"engine_pid"`
	EngineStartTicks uint64 `json:"engine_start_ticks"`
	MountID          int    `json:"mount_id"`
	RuntimeDev       uint64 `json:"runtime_dev"`
	RuntimeIno       uint64 `json:"runtime_ino"`
	RootDev          uint64 `json:"root_dev"`
	RootIno          uint64 `json:"root_ino"`
	CipherDev        uint64 `json:"cipher_dev"`
	CipherIno        uint64 `json:"cipher_ino"`
	MountDev         uint64 `json:"mount_dev"`
	MountIno         uint64 `json:"mount_ino"`
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

type mountedView struct {
	mountPath    string
	mountID      int
	mountDirInfo os.FileInfo
	underlayFile *os.File
	process      *ownedProcess
	runtimeFile  *os.File
	lease        runtimeLease
	leaseInfo    os.FileInfo
}

func profileIdentityHash(appID, profileID string) string {
	hash := sha256.Sum256([]byte(appID + "\x00" + profileID))
	return hex.EncodeToString(hash[:])
}

func startPrivateMount(ctx context.Context, opts Options, unmountPath, rootPath string, rootFD int, manifest vaultManifest, key []byte) (*mountedView, error) {
	runtimePath, err := filepath.Abs(opts.RuntimeDir)
	if err != nil || filepath.Clean(runtimePath) != runtimePath {
		return nil, ErrUnsafePath
	}
	runtimeFD, err := openDirectory(runtimePath, false)
	if err != nil {
		return nil, err
	}
	runtimeFile := os.NewFile(uintptr(runtimeFD), runtimePath)
	keepRuntime := false
	defer func() {
		if !keepRuntime {
			_ = runtimeFile.Close()
		}
	}()
	runtimeID, err := identityFromFD(runtimeFD)
	if err != nil {
		return nil, ErrUnsafePath
	}
	if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
		return nil, ErrUnsafePath
	}
	rootID, err := identityFromFD(rootFD)
	if err != nil {
		return nil, ErrUnsafePath
	}
	cipherFD, cipherExists, err := openCipherDir(rootFD)
	if err != nil || !cipherExists {
		return nil, ErrIncompleteVault
	}
	cipherID, err := identityFromFD(cipherFD)
	_ = syscall.Close(cipherFD)
	if err != nil {
		return nil, ErrUnsafePath
	}
	bootID, err := currentBootID()
	if err != nil {
		return nil, ErrUnsafePath
	}
	ownerStart, err := currentProcessStartTicks()
	if err != nil {
		return nil, ErrUnsafePath
	}

	mountPath, err := os.MkdirTemp(runtimePath, "wf-pv-")
	if err != nil {
		return nil, ErrUnsafePath
	}
	mountInfo, err := os.Lstat(mountPath)
	if err != nil || !mountInfo.IsDir() || mountInfo.Mode()&os.ModeSymlink != 0 || mountInfo.Mode().Perm() != 0o700 {
		return nil, ErrUnsafePath
	}
	mountIdentity, err := identityFromInfo(mountInfo)
	if err != nil {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, ErrUnsafePath
	}
	underlayFile, err := os.Open(mountPath)
	if err != nil {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, ErrUnsafePath
	}
	keepUnderlay := false
	defer func() {
		if !keepUnderlay {
			_ = underlayFile.Close()
		}
	}()
	if _, mounted, err := findMount(mountPath); err != nil || mounted {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, ErrMountChanged
	}
	if err := verifyRootPath(rootPath, rootFD); err != nil {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, err
	}
	if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, ErrUnsafePath
	}

	lease := makeRuntimeLease(runtimePath, mountPath, rootPath, runtimeID, rootID, cipherID, mountIdentity, opts, manifest, bootID, os.Getpid(), ownerStart)
	leaseName := lease.MountName + leaseSuffix
	leaseInfo, err := persistLeaseAt(runtimeFD, leaseName, lease, runtimeLease{}, nil)
	if err != nil {
		_ = removeCreatedMountpoint(mountPath, mountInfo)
		return nil, err
	}
	leaseCandidates := []runtimeLease{lease}
	var process *ownedProcess
	expectedMountID := 0
	cleanup := func() {
		if verifyDirectoryIdentity(runtimePath, runtimeID) != nil {
			_ = terminateOwnedProcess(process)
			return
		}
		if ref, mounted, findErr := findMount(mountPath); findErr == nil && mounted && expectedMountID > 0 &&
			ref.id == expectedMountID && ref.fsType == "fuse.gocryptfs" && ref.owner == os.Geteuid() &&
			!ref.allowOther && ref.statfsValid && ref.source == filepath.Join(rootPath, cipherDirName) {
			_ = normalUnmount(context.Background(), unmountPath, mountPath)
		}
		_ = terminateOwnedProcess(process)
		if _, mounted, findErr := findMount(mountPath); findErr == nil && !mounted {
			if removeLeaseTarget(runtimePath, lease, false) == nil {
				if current, currentInfo, readErr := readLeaseAt(runtimeFD, leaseName, runtimePath); readErr == nil {
					for _, candidate := range leaseCandidates {
						if current == candidate {
							_ = removeLeaseAt(runtimeFD, leaseName, runtimePath, candidate, currentInfo)
							break
						}
					}
				}
			}
		}
	}

	if _, mounted, err := findMount(mountPath); err != nil || mounted {
		cleanup()
		return nil, ErrMountChanged
	}
	if err := verifyRootPath(rootPath, rootFD); err != nil {
		cleanup()
		return nil, err
	}
	if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
		cleanup()
		return nil, ErrUnsafePath
	}

	cipherPath := filepath.Join(rootPath, cipherDirName)
	cmd := exec.Command(opts.GocryptfsPath, "-fg", "-q", "-noexec", "--", cipherPath, mountPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = time.Second
	stdout := &limitedOutput{limit: maxCommandOutput}
	stderr := &limitedOutput{limit: maxCommandOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, ErrMountFailed
	}
	process = &ownedProcess{cmd: cmd, done: make(chan struct{}), stdout: stdout, stderr: stderr}
	startResult := make(chan error, 1)
	go func() {
		// Linux Pdeathsig belongs to the thread that starts the child; pin
		// this owner through Wait so that thread outlives the child.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			startResult <- err
			return
		}
		startResult <- nil
		process.waitErr = cmd.Wait()
		wipe(stdout.Bytes())
		wipe(stderr.Bytes())
		close(process.done)
	}()
	if err := <-startResult; err != nil {
		_ = stdin.Close()
		wipe(stdout.Bytes())
		wipe(stderr.Bytes())
		cleanup()
		return nil, ErrMountFailed
	}
	payload := append(append([]byte(nil), key...), '\n')
	_, writeErr := stdin.Write(payload)
	wipe(payload)
	closeErr := stdin.Close()
	if writeErr != nil || closeErr != nil {
		cleanup()
		return nil, ErrMountFailed
	}

	timeout := boundedTimeout(opts.MountTimeout, 10*time.Second, 60*time.Second)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		ref, mounted, inspectErr := findMount(mountPath)
		if inspectErr != nil {
			cleanup()
			return nil, ErrMountFailed
		}
		if mounted {
			if ref.fsType != "fuse.gocryptfs" || ref.owner != os.Geteuid() || ref.allowOther || !ref.statfsValid || ref.source != cipherPath {
				cleanup()
				return nil, ErrMountChanged
			}
			expectedMountID = ref.id
			select {
			case <-process.done:
				cleanup()
				return nil, ErrMountFailed
			default:
			}
			engineStart, _, statErr := readProcessStat(cmd.Process.Pid)
			if statErr != nil {
				cleanup()
				return nil, ErrMountFailed
			}
			attachedLease := lease
			attachedLease.State = leaseAttached
			attachedLease.MountID = ref.id
			attachedLease.EnginePID = cmd.Process.Pid
			attachedLease.EngineStartTicks = engineStart
			leaseCandidates = append(leaseCandidates, attachedLease)
			newLeaseInfo, persistErr := persistLeaseAt(runtimeFD, leaseName, attachedLease, lease, leaseInfo)
			lease = attachedLease
			if persistErr != nil {
				if current, currentInfo, readErr := readLeaseAt(runtimeFD, leaseName, runtimePath); readErr == nil {
					lease, leaseInfo = current, currentInfo
				}
				cleanup()
				return nil, ErrMountFailed
			}
			leaseInfo = newLeaseInfo

			if err := syscall.Fchmod(int(underlayFile.Fd()), 0); err != nil {
				cleanup()
				return nil, ErrUnsafePath
			}
			mountInfo, err = underlayFile.Stat()
			if err != nil || mountInfo.Mode().Perm() != 0 {
				cleanup()
				return nil, ErrUnsafePath
			}
			finalRef, stillMounted, finalErr := findMount(mountPath)
			if finalErr != nil || !stillMounted || finalRef.id != ref.id || finalRef.fsType != "fuse.gocryptfs" ||
				finalRef.owner != os.Geteuid() || finalRef.allowOther || !finalRef.statfsValid || finalRef.source != cipherPath {
				cleanup()
				return nil, ErrMountFailed
			}
			select {
			case <-process.done:
				cleanup()
				return nil, ErrMountFailed
			default:
			}
			mountedLease := lease
			mountedLease.State = leaseMounted
			mountedInfo, persistErr := persistLeaseAt(runtimeFD, leaseName, mountedLease, lease, leaseInfo)
			lease = mountedLease
			if persistErr != nil {
				if current, currentInfo, readErr := readLeaseAt(runtimeFD, leaseName, runtimePath); readErr == nil {
					lease, leaseInfo = current, currentInfo
				}
				cleanup()
				return nil, ErrMountFailed
			}
			leaseInfo = mountedInfo
			if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
				cleanup()
				return nil, ErrUnsafePath
			}
			keepUnderlay = true
			keepRuntime = true
			return &mountedView{
				mountPath: mountPath, mountID: ref.id, mountDirInfo: mountInfo,
				underlayFile: underlayFile, process: process, runtimeFile: runtimeFile,
				lease: lease, leaseInfo: leaseInfo,
			}, nil
		}
		select {
		case <-process.done:
			cleanup()
			return nil, ErrMountFailed
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-timer.C:
			cleanup()
			return nil, ErrMountFailed
		case <-tick.C:
		}
	}
}

func (v *Vault) ownsDurableLease() bool {
	if v == nil || v.runtimeFile == nil || v.leaseName == "" || v.leaseInfo == nil || v.process == nil || v.process.cmd == nil || v.process.cmd.Process == nil {
		return false
	}
	current, info, err := readLeaseAt(int(v.runtimeFile.Fd()), v.leaseName, v.lease.RuntimePath)
	if err != nil || current != v.lease || !sameFileIdentity(info, v.leaseInfo) {
		return false
	}
	ownerStart, err := currentProcessStartTicks()
	return err == nil && v.lease.OwnerPID == os.Getpid() && v.lease.OwnerStartTicks == ownerStart &&
		v.lease.State == leaseMounted && v.lease.MountID == v.mountID &&
		v.lease.EnginePID == v.process.cmd.Process.Pid && v.lease.CipherPath == v.cipherPath
}

func identityFromFD(fd int) (fileIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func identityFromInfo(info os.FileInfo) (fileIdentity, error) {
	if info == nil {
		return fileIdentity{}, ErrUnsafePath
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, ErrUnsafePath
	}
	return fileIdentity{dev: uint64(st.Dev), ino: st.Ino}, nil
}

func verifyDirectoryIdentity(path string, expected fileIdentity) error {
	fd, err := openDirectory(path, false)
	if err != nil {
		return ErrUnsafePath
	}
	defer syscall.Close(fd)
	current, err := identityFromFD(fd)
	if err != nil || current != expected {
		return ErrUnsafePath
	}
	return nil
}

func removeCreatedMountpoint(path string, original os.FileInfo) error {
	if _, mounted, err := findMount(path); err != nil || mounted {
		return ErrStaleMountUnverified
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !sameFileIdentity(current, original) {
		return ErrStaleMountUnverified
	}
	if err := os.Remove(path); err != nil {
		return ErrUnsafePath
	}
	return nil
}

func currentProcessStartTicks() (uint64, error) {
	start, _, err := readProcessStat(os.Getpid())
	return start, err
}

func currentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(data))
	if !validBootID(bootID) {
		return "", ErrStaleMountUnverified
	}
	return bootID, nil
}

func validBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func readProcessStat(pid int) (uint64, byte, error) {
	if pid <= 0 {
		return 0, 0, ErrStaleMountUnverified
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, err
	}
	if len(data) > 8192 {
		return 0, 0, ErrStaleMountUnverified
	}
	// The comm field can contain spaces and parentheses; the final ')' ends it.
	closeParen := bytes.LastIndexByte(data, ')')
	if closeParen < 0 || closeParen+2 >= len(data) {
		return 0, 0, ErrStaleMountUnverified
	}
	fields := strings.Fields(string(data[closeParen+1:]))
	// fields[0] is proc stat field 3 (state); fields[19] is field 22 (starttime).
	if len(fields) <= 19 || len(fields[0]) != 1 {
		return 0, 0, ErrStaleMountUnverified
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return 0, 0, ErrStaleMountUnverified
	}
	return start, fields[0][0], nil
}

func processIdentityAlive(pid int, startTicks uint64) (bool, error) {
	start, state, err := readProcessStat(pid)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if start != startTicks {
		// The PID was reused; it is not the process named by this lease.
		return false, nil
	}
	return state != 'Z' && state != 'X', nil
}

func makeRuntimeLease(runtimePath, mountPath, rootPath string, runtimeID, rootID, cipherID, mountID fileIdentity, opts Options, manifest vaultManifest, bootID string, ownerPID int, ownerStartTicks uint64) runtimeLease {
	return runtimeLease{
		Format: leaseFormat, Version: leaseVersion, State: leasePreparing,
		RuntimePath: runtimePath, MountName: filepath.Base(mountPath),
		RootPath: rootPath, CipherPath: filepath.Join(rootPath, cipherDirName),
		ProfileHash: profileIdentityHash(opts.AppID, opts.ProfileID), VaultID: manifest.VaultID,
		BootID: bootID, UID: os.Geteuid(), OwnerPID: ownerPID, OwnerStartTicks: ownerStartTicks,
		RuntimeDev: runtimeID.dev, RuntimeIno: runtimeID.ino,
		RootDev: rootID.dev, RootIno: rootID.ino,
		CipherDev: cipherID.dev, CipherIno: cipherID.ino,
		MountDev: mountID.dev, MountIno: mountID.ino,
	}
}

func validLease(lease runtimeLease, filename, runtimePath string) bool {
	if lease.Format != leaseFormat || lease.Version != leaseVersion ||
		(lease.State != leasePreparing && lease.State != leaseAttached && lease.State != leaseMounted) ||
		!filepath.IsAbs(lease.RuntimePath) || filepath.Clean(lease.RuntimePath) != lease.RuntimePath ||
		lease.RuntimePath != runtimePath || !filepath.IsAbs(lease.RootPath) || filepath.Clean(lease.RootPath) != lease.RootPath ||
		lease.CipherPath != filepath.Join(lease.RootPath, cipherDirName) || !filepath.IsAbs(lease.CipherPath) ||
		filename != lease.MountName+leaseSuffix || filepath.Base(lease.MountName) != lease.MountName ||
		!strings.HasPrefix(lease.MountName, "wf-pv-") || len(lease.MountName) < len("wf-pv-")+6 ||
		len(lease.MountName) > len("wf-pv-")+32 || !validHex(lease.ProfileHash, profileHashHexSize) ||
		!validHex(lease.VaultID, 32) || !validBootID(lease.BootID) || lease.UID != os.Geteuid() ||
		lease.OwnerPID <= 0 || lease.OwnerStartTicks == 0 || lease.RuntimeIno == 0 || lease.RootIno == 0 ||
		lease.CipherIno == 0 || lease.MountIno == 0 || lease.RuntimeDev == 0 || lease.RootDev == 0 ||
		lease.CipherDev == 0 || lease.MountDev == 0 {
		return false
	}
	for _, char := range lease.MountName[len("wf-pv-"):] {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	if lease.State == leasePreparing {
		return lease.MountID == 0 && lease.EnginePID == 0 && lease.EngineStartTicks == 0
	}
	return lease.MountID > 0 && lease.EnginePID > 0 && lease.EngineStartTicks > 0
}

func validHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func readLeaseAt(dirFD int, filename, runtimePath string) (runtimeLease, os.FileInfo, error) {
	if filename == "" || filepath.Base(filename) != filename || !strings.HasPrefix(filename, "wf-pv-") || !strings.HasSuffix(filename, leaseSuffix) {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	fd, err := syscall.Openat(dirFD, filename, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	file := os.NewFile(uintptr(fd), filename)
	defer file.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || st.Mode&0o777 != 0o600 || st.Size < 2 || st.Size > maxLeaseBytes {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLeaseBytes+1))
	if err != nil || len(data) > maxLeaseBytes {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lease runtimeLease
	if decoder.Decode(&lease) != nil {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	canonical, err := json.Marshal(lease)
	if err != nil || !bytes.Equal(data, canonical) || !validLease(lease, filename, runtimePath) {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	info, err := file.Stat()
	if err != nil {
		return runtimeLease{}, nil, ErrStaleMountUnverified
	}
	return lease, info, nil
}

func persistLeaseAt(dirFD int, filename string, lease runtimeLease, prior runtimeLease, priorInfo os.FileInfo) (os.FileInfo, error) {
	if !validLease(lease, filename, lease.RuntimePath) {
		return nil, ErrStaleMountUnverified
	}
	data, err := json.Marshal(lease)
	if err != nil || len(data) > maxLeaseBytes {
		return nil, ErrStaleMountUnverified
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, ErrStaleMountUnverified
	}
	temporary := ".profilevault-lease.tmp." + hex.EncodeToString(nonce[:])
	fd, err := syscall.Openat(dirFD, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, ErrUnsafePath
	}
	file := os.NewFile(uintptr(fd), temporary)
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = syscall.Unlinkat(dirFD, temporary)
		}
	}()
	written, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || written != len(data) || syncErr != nil || closeErr != nil {
		return nil, ErrUnsafePath
	}
	if priorInfo == nil {
		err = unix.Renameat2(dirFD, temporary, dirFD, filename, unix.RENAME_NOREPLACE)
	} else {
		current, currentInfo, readErr := readLeaseAt(dirFD, filename, lease.RuntimePath)
		if readErr != nil || current != prior || !sameFileIdentity(currentInfo, priorInfo) {
			return nil, ErrStaleMountUnverified
		}
		err = syscall.Renameat(dirFD, temporary, dirFD, filename)
	}
	if err != nil {
		return nil, ErrUnsafePath
	}
	removeTemporary = false
	if err := syscall.Fsync(dirFD); err != nil {
		return nil, ErrUnsafePath
	}
	_, info, err := readLeaseAt(dirFD, filename, lease.RuntimePath)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func removeLeaseAt(dirFD int, filename, runtimePath string, expected runtimeLease, expectedInfo os.FileInfo) error {
	current, info, err := readLeaseAt(dirFD, filename, runtimePath)
	if err != nil || current != expected || !sameFileIdentity(info, expectedInfo) {
		return ErrStaleMountUnverified
	}
	if err := syscall.Unlinkat(dirFD, filename); err != nil {
		return ErrUnsafePath
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return ErrUnsafePath
	}
	return nil
}

// recoverRuntimeLeases reconciles only version-1 leases written by this package.
// Call it only while Open holds the encrypted root's exclusive flock and has
// passed manifest/ciphertext and keyring preflight. Old `wf-pv-*` targets
// without a valid lease, plus preparing leases with a visible FUSE mount, are
// ambiguous and are rejected without cleanup. A complete stale lease is
// normal-unmounted only after its boot-bound owner and engine PID/start-time
// pairs are proved dead and its exact mount ID, source, UID, root, and private
// runtime binding still match. Prior-boot leases are only cleaned if no mount
// exists at their recorded target.
func recoverRuntimeLeases(opts Options, rootPath string, rootFD int, manifest vaultManifest, unmountPath string) error {
	runtimePath, err := filepath.Abs(opts.RuntimeDir)
	if err != nil || filepath.Clean(runtimePath) != runtimePath {
		return ErrUnsafePath
	}
	runtimeFD, err := openDirectory(runtimePath, false)
	if err != nil {
		return err
	}
	defer syscall.Close(runtimeFD)
	runtimeID, err := identityFromFD(runtimeFD)
	if err != nil {
		return ErrUnsafePath
	}
	if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
		return ErrUnsafePath
	}
	rootID, err := identityFromFD(rootFD)
	if err != nil {
		return ErrUnsafePath
	}
	cipherPath := filepath.Join(rootPath, cipherDirName)
	cipherFD, cipherExists, err := openCipherDir(rootFD)
	if err != nil || !cipherExists {
		return ErrIncompleteVault
	}
	cipherID, err := identityFromFD(cipherFD)
	_ = syscall.Close(cipherFD)
	if err != nil {
		return ErrUnsafePath
	}
	bootID, err := currentBootID()
	if err != nil {
		return ErrStaleMountUnverified
	}
	entries, err := directoryEntries(runtimeFD)
	if err != nil {
		return ErrUnsafePath
	}
	sort.Strings(entries)
	leaseNames := make([]string, 0, len(entries)/2)
	entrySet := make(map[string]struct{}, len(entries))
	for _, name := range entries {
		entrySet[name] = struct{}{}
		if strings.HasPrefix(name, ".profilevault-lease.tmp.") {
			// A crashed atomic publication is ambiguous: its temporary payload
			// might describe a mount whose final ownership record never landed.
			return ErrStaleMountUnverified
		}
		if strings.HasPrefix(name, "wf-pv-") && strings.HasSuffix(name, leaseSuffix) {
			leaseNames = append(leaseNames, name)
		}
	}
	if len(leaseNames) > maxRuntimeLeases {
		return ErrUnsafePath
	}
	for _, name := range entries {
		if strings.HasPrefix(name, "wf-pv-") && !strings.HasSuffix(name, leaseSuffix) {
			if _, hasLease := entrySet[name+leaseSuffix]; !hasLease {
				// Unknown-prefix entries have no durable ownership binding. Never
				// infer ownership or clean them from their name alone.
				return ErrStaleMountUnverified
			}
		}
	}
	for _, filename := range leaseNames {
		lease, info, err := readLeaseAt(runtimeFD, filename, runtimePath)
		if err != nil {
			return ErrStaleMountUnverified
		}
		if lease.RuntimeDev != runtimeID.dev || lease.RuntimeIno != runtimeID.ino {
			return ErrStaleMountUnverified
		}
		sameRoot := lease.RootPath == rootPath || (lease.RootDev == rootID.dev && lease.RootIno == rootID.ino)
		sameProfile := lease.ProfileHash == profileIdentityHash(opts.AppID, opts.ProfileID)
		sameCipher := lease.CipherPath == cipherPath || (lease.CipherDev == cipherID.dev && lease.CipherIno == cipherID.ino)
		if !sameRoot && !sameProfile && !sameCipher {
			continue
		}
		if !sameRoot || !sameProfile || !sameCipher || lease.RootPath != rootPath ||
			lease.CipherPath != cipherPath || lease.VaultID != manifest.VaultID ||
			lease.RootDev != rootID.dev || lease.RootIno != rootID.ino ||
			lease.CipherDev != cipherID.dev || lease.CipherIno != cipherID.ino {
			return ErrStaleMountUnverified
		}
		if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
			return ErrStaleMountUnverified
		}
		if err := recoverOneLease(runtimeFD, runtimePath, lease, info, bootID, unmountPath); err != nil {
			return err
		}
	}
	return nil
}

func recoverOneLease(runtimeFD int, runtimePath string, lease runtimeLease, info os.FileInfo, bootID, unmountPath string) error {
	mountPath := filepath.Join(runtimePath, lease.MountName)
	runtimeID := fileIdentity{dev: lease.RuntimeDev, ino: lease.RuntimeIno}
	if lease.BootID != bootID {
		if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
			return ErrStaleMountUnverified
		}
		if _, mounted, err := findMount(mountPath); err != nil || mounted {
			return ErrStaleMountUnverified
		}
		if err := removeLeaseTarget(runtimePath, lease, false); err != nil {
			return err
		}
		return removeLeaseAt(runtimeFD, lease.MountName+leaseSuffix, runtimePath, lease, info)
	}
	ownerAlive, err := processIdentityAlive(lease.OwnerPID, lease.OwnerStartTicks)
	if err != nil {
		return ErrStaleMountUnverified
	}
	engineAlive := false
	if lease.EnginePID != 0 {
		engineAlive, err = processIdentityAlive(lease.EnginePID, lease.EngineStartTicks)
		if err != nil {
			return ErrStaleMountUnverified
		}
	}
	if ownerAlive || engineAlive {
		return ErrVaultBusy
	}
	ref, mounted, err := findMount(mountPath)
	if err != nil {
		return ErrStaleMountUnverified
	}
	if mounted {
		if lease.State == leasePreparing || ref.id != lease.MountID || ref.fsType != "fuse.gocryptfs" ||
			ref.owner != lease.UID || ref.owner != os.Geteuid() || ref.allowOther || ref.source != lease.CipherPath {
			return ErrStaleMountUnverified
		}
		if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
			return ErrStaleMountUnverified
		}
		if err := normalUnmount(context.Background(), unmountPath, mountPath); err != nil {
			return ErrUnmountFailed
		}
		if _, stillMounted, err := findMount(mountPath); err != nil || stillMounted {
			return ErrUnmountFailed
		}
	} else if lease.State == leasePreparing {
		// A preparation lease proves ownership only of an unmounted private
		// directory. A mount created before mount-ID publication is ambiguous.
	}
	if err := verifyDirectoryIdentity(runtimePath, runtimeID); err != nil {
		return ErrStaleMountUnverified
	}
	if err := removeLeaseTarget(runtimePath, lease, lease.State == leaseMounted); err != nil {
		return err
	}
	return removeLeaseAt(runtimeFD, lease.MountName+leaseSuffix, runtimePath, lease, info)
}

func removeLeaseTarget(runtimePath string, lease runtimeLease, requirePrivate bool) error {
	mountPath := filepath.Join(runtimePath, lease.MountName)
	_, mounted, err := findMount(mountPath)
	if err != nil || mounted {
		return ErrStaleMountUnverified
	}
	info, err := os.Lstat(mountPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrStaleMountUnverified
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != lease.UID || uint64(st.Dev) != lease.MountDev || st.Ino != lease.MountIno {
		return ErrStaleMountUnverified
	}
	if requirePrivate && info.Mode().Perm() != 0 {
		return ErrStaleMountUnverified
	}
	if err := os.Remove(mountPath); err != nil {
		return ErrUnsafePath
	}
	return nil
}
