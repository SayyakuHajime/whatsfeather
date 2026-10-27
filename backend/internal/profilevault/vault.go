package profilevault

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrKeyNotFound          = errors.New("profilevault: key not found")
	ErrLegacyPlaintext      = errors.New("profilevault: legacy plaintext profile roots are not opened")
	ErrUnrecognizedRoot     = errors.New("profilevault: root is not a recognized versioned vault")
	ErrUnsupportedVersion   = errors.New("profilevault: unsupported vault format version")
	ErrVaultBusy            = errors.New("profilevault: profile vault is already owned by another process")
	ErrUnsafePath           = errors.New("profilevault: unsafe root path or permissions")
	ErrKeyringUnavailable   = errors.New("profilevault: keyring operation failed")
	ErrKeyringConflict      = errors.New("profilevault: a different keyring item already owns this profile identity")
	ErrKeyringStateUnknown  = errors.New("profilevault: keyring store result could not be reconciled")
	ErrInvalidMasterKey     = errors.New("profilevault: stored master key is malformed")
	ErrInitializationFailed = errors.New("profilevault: encrypted-root initialization failed")
	ErrIncompleteVault      = errors.New("profilevault: encrypted root is incomplete or inconsistent")
	ErrMountFailed          = errors.New("profilevault: encrypted mount failed")
	ErrMountChanged         = errors.New("profilevault: mount ownership changed; refusing to unmount it")
	ErrUnmountFailed        = errors.New("profilevault: normal unmount failed; vault remains owned for retry")
	ErrVaultClosed          = errors.New("profilevault: vault is closed")
	ErrStaleMountUnverified = errors.New("profilevault: stale runtime mount ownership is ambiguous; refusing recovery")
)

const (
	lockFileName       = ".profilevault.lock"
	manifestFileName   = ".profilevault.json"
	cipherDirName      = "cipher"
	manifestFormat     = "whatsfeather-profile-vault"
	manifestVersion    = 1
	manifestInitState  = "initializing"
	manifestReadyState = "ready"
	maxManifestBytes   = 4096
	maxDirectoryItems  = 256
	maxCommandOutput   = 64 * 1024
	fuseSuperMagic     = 0x65735546
)

// Keyring stores opaque profile master-key bytes outside the profile directory.
// Implementations must not persist the key outside the OS secret service.
type Keyring interface {
	Lookup(ctx context.Context, appID, profileID string) ([]byte, error)
	Store(ctx context.Context, appID, profileID string, key []byte) error
}

// Options identifies one opaque app/profile pair and the encrypted-root runtime.
// RootDir and RuntimeDir must be owned, private directories without symlink
// components. GocryptfsPath must name a vetted executable selected by the caller.
type Options struct {
	RootDir         string
	RuntimeDir      string
	AppID           string
	ProfileID       string
	Keyring         Keyring
	GocryptfsPath   string
	FuseUnmountPath string
	CommandTimeout  time.Duration
	MountTimeout    time.Duration
}

type vaultManifest struct {
	Format           string `json:"format"`
	Version          int    `json:"version"`
	VaultID          string `json:"vault_id"`
	State            string `json:"state"`
	MigrationStarted bool   `json:"migration_started,omitempty"`
}

// Open initializes a new empty root or opens a recognized versioned encrypted
// root. Unrecognized non-empty roots are rejected before any keyring or engine
// operation; migration is intentionally not performed here.
func Open(ctx context.Context, opts Options) (*Vault, error) {
	if ctx == nil || opts.RootDir == "" || opts.RuntimeDir == "" || opts.AppID == "" || opts.ProfileID == "" || opts.Keyring == nil || opts.GocryptfsPath == "" {
		return nil, ErrUnsafePath
	}
	if validateIdentity(opts.AppID, opts.ProfileID) != nil {
		return nil, ErrUnsafePath
	}
	rootPath, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, ErrUnsafePath
	}
	opts.RootDir = rootPath
	rootFD, err := openDirectory(rootPath, true)
	if err != nil {
		return nil, err
	}
	rootFile := os.NewFile(uintptr(rootFD), rootPath)
	keepRoot := false
	defer func() {
		if !keepRoot {
			_ = rootFile.Close()
		}
	}()

	lockFD, err := syscall.Openat(rootFD, lockFileName, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, ErrUnsafePath
	}
	lockFile := os.NewFile(uintptr(lockFD), lockFileName)
	lockStat := &syscall.Stat_t{}
	if err := syscall.Fstat(lockFD, lockStat); err != nil || lockStat.Mode&syscall.S_IFMT != syscall.S_IFREG || int(lockStat.Uid) != os.Geteuid() || lockStat.Nlink != 1 || lockStat.Mode&0o077 != 0 {
		_ = lockFile.Close()
		return nil, ErrUnsafePath
	}
	if err := syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrVaultBusy
		}
		return nil, ErrUnsafePath
	}
	keepLock := false
	defer func() {
		if !keepLock {
			_ = syscall.Flock(lockFD, syscall.LOCK_UN)
			_ = lockFile.Close()
		}
	}()

	entries, err := directoryEntries(rootFD)
	if err != nil {
		return nil, ErrUnsafePath
	}
	manifestFound := contains(entries, manifestFileName)
	for _, name := range entries {
		if name == lockFileName || name == manifestFileName || name == cipherDirName {
			continue
		}
		if !manifestFound {
			return nil, ErrLegacyPlaintext
		}
		return nil, ErrUnrecognizedRoot
	}
	if !manifestFound && contains(entries, cipherDirName) {
		return nil, ErrLegacyPlaintext
	}

	// Check executable provenance only after rejecting legacy/plaintext roots.
	if err := validateExecutable(opts.GocryptfsPath); err != nil {
		return nil, ErrMountFailed
	}
	unmountPath, err := resolveUnmountPath(opts.FuseUnmountPath)
	if err != nil {
		return nil, ErrMountFailed
	}
	if err := validateExecutable(unmountPath); err != nil {
		return nil, ErrMountFailed
	}

	var manifest vaultManifest
	var key []byte
	if !manifestFound {
		key, manifest, err = initializeNewRoot(ctx, rootFD, opts)
	} else {
		manifest, err = readManifestAt(rootFD)
		if err == nil {
			cipherFD, cipherExists, cipherErr := openCipherDir(rootFD)
			if cipherErr != nil || (manifest.State == manifestReadyState && !cipherExists) {
				err = ErrIncompleteVault
			} else {
				if cipherExists {
					_ = syscall.Close(cipherFD)
				}
			}
		}
		if err == nil {
			key, err = prepareRecognizedRoot(ctx, rootFD, manifest, opts)
		}
	}
	if err != nil {
		wipe(key)
		return nil, err
	}
	defer wipe(key)

	if err := verifyRootPath(rootPath, rootFD); err != nil {
		return nil, err
	}
	if err := recoverRuntimeLeases(opts, rootPath, rootFD, manifest, unmountPath); err != nil {
		return nil, err
	}
	view, err := startPrivateMount(ctx, opts, unmountPath, rootPath, rootFD, manifest, key)
	if err != nil {
		return nil, err
	}
	v := &Vault{
		rootPath: rootPath, cipherPath: filepath.Join(rootPath, cipherDirName),
		mountPath: view.mountPath, mountID: view.mountID, mountDirInfo: view.mountDirInfo,
		underlayFile: view.underlayFile, rootFile: rootFile, lockFile: lockFile, process: view.process,
		unmountPath: unmountPath, runtimeFile: view.runtimeFile,
		lease: view.lease, leaseInfo: view.leaseInfo, leaseName: view.lease.MountName + leaseSuffix,
		migrationStarted: manifest.MigrationStarted,
	}
	keepRoot = true
	keepLock = true
	return v, nil
}

// Vault owns one foreground gocryptfs process, one private mount and the root
// lock. Call Close only after all database and media handles have been closed.
type Vault struct {
	mu               sync.Mutex
	rootPath         string
	cipherPath       string
	mountPath        string
	mountID          int
	mountDirInfo     os.FileInfo
	underlayFile     *os.File
	rootFile         *os.File
	lockFile         *os.File
	process          *ownedProcess
	unmountPath      string
	runtimeFile      *os.File
	lease            runtimeLease
	leaseInfo        os.FileInfo
	leaseName        string
	migrationStarted bool
	closed           bool
}

// MountPath returns the private decrypted view. It does not transfer lifecycle
// ownership; the returned path is valid only until Close succeeds.
func (v *Vault) MountPath() string {
	if v == nil {
		return ""
	}
	return v.mountPath
}

// MountID returns the kernel mount identifier validated and owned by this
// vault instance. It is metadata only and does not transfer lifecycle ownership.
func (v *Vault) MountID() int {
	if v == nil {
		return 0
	}
	return v.mountID
}

// MigrationStarted reports the durable destination-bound offline-migration
// intent stored in the existing outer vault manifest.
func (v *Vault) MigrationStarted() bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.migrationStarted
}

// MarkMigrationStarted durably marks this destination before profile bytes are
// copied. It writes only the existing outer vault manifest, never the mounted
// profile tree or a new vault-root entry.
func (v *Vault) MarkMigrationStarted() error {
	if v == nil {
		return ErrVaultClosed
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.rootFile == nil {
		return ErrVaultClosed
	}
	if v.migrationStarted {
		return nil
	}
	rootFD := int(v.rootFile.Fd())
	if err := verifyRootPath(v.rootPath, rootFD); err != nil {
		return ErrUnsafePath
	}
	manifest, err := readManifestAt(rootFD)
	if err != nil || manifest.State != manifestReadyState {
		return ErrIncompleteVault
	}
	manifest.MigrationStarted = true
	if err := writeManifestAt(rootFD, manifest, false); err != nil {
		return ErrIncompleteVault
	}
	v.migrationStarted = true
	return nil
}

// Close performs a normal non-lazy unmount. A busy/error result leaves the
// process and lock owned so the caller can release handles and retry safely.
func (v *Vault) Close(ctx context.Context) error {
	if v == nil || ctx == nil {
		return ErrVaultClosed
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil
	}
	if !v.ownsDurableLease() {
		return ErrMountChanged
	}

	ref, mounted, err := findMount(v.mountPath)
	if err != nil {
		return ErrUnmountFailed
	}
	if mounted {
		if ref.id != v.mountID || ref.fsType != "fuse.gocryptfs" || ref.owner != os.Geteuid() || ref.allowOther || ref.source != v.cipherPath {
			return ErrMountChanged
		}
		if err := normalUnmount(ctx, v.unmountPath, v.mountPath); err != nil {
			return ErrUnmountFailed
		}
	}
	if err := stopOwnedProcess(ctx, v.process); err != nil {
		return ErrUnmountFailed
	}
	_, stillMounted, err := findMount(v.mountPath)
	if err != nil || stillMounted {
		return ErrUnmountFailed
	}
	if err := removeOwnedMountpoint(v.mountPath, v.mountDirInfo); err != nil {
		return ErrUnsafePath
	}
	if v.underlayFile != nil {
		if err := v.underlayFile.Close(); err != nil {
			return ErrUnsafePath
		}
		v.underlayFile = nil
	}
	if err := removeLeaseAt(int(v.runtimeFile.Fd()), v.leaseName, v.lease.RuntimePath, v.lease, v.leaseInfo); err != nil {
		return ErrUnsafePath
	}
	v.leaseInfo = nil
	if v.runtimeFile != nil {
		if err := v.runtimeFile.Close(); err != nil {
			return ErrUnsafePath
		}
		v.runtimeFile = nil
	}
	if v.lockFile != nil {
		fd := int(v.lockFile.Fd())
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		if err := v.lockFile.Close(); err != nil {
			return ErrUnsafePath
		}
	}
	if v.rootFile != nil {
		if err := v.rootFile.Close(); err != nil {
			return ErrUnsafePath
		}
	}
	v.closed = true
	return nil
}

func initializeNewRoot(ctx context.Context, rootFD int, opts Options) ([]byte, vaultManifest, error) {
	existing, err := opts.Keyring.Lookup(ctx, opts.AppID, opts.ProfileID)
	if err == nil {
		wipe(existing)
		return nil, vaultManifest{}, ErrKeyringConflict
	}
	wipe(existing)
	if !errors.Is(err, ErrKeyNotFound) {
		return nil, vaultManifest{}, ErrKeyringUnavailable
	}
	key, err := newMasterKey()
	if err != nil {
		return nil, vaultManifest{}, ErrInitializationFailed
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		wipe(key)
		return nil, vaultManifest{}, ErrInitializationFailed
	}
	manifest := vaultManifest{Format: manifestFormat, Version: manifestVersion, VaultID: hex.EncodeToString(id), State: manifestInitState}
	if err := writeManifestAt(rootFD, manifest, true); err != nil {
		wipe(key)
		return nil, vaultManifest{}, ErrIncompleteVault
	}
	storeKey := append([]byte(nil), key...)
	_ = opts.Keyring.Store(ctx, opts.AppID, opts.ProfileID, storeKey)
	wipe(storeKey)
	reconciled, lookupErr := opts.Keyring.Lookup(ctx, opts.AppID, opts.ProfileID)
	if lookupErr != nil || subtle.ConstantTimeCompare(reconciled, key) != 1 {
		wipe(reconciled)
		wipe(key)
		return nil, vaultManifest{}, ErrKeyringStateUnknown
	}
	wipe(reconciled)
	if err := createCipherDir(rootFD); err != nil {
		wipe(key)
		return nil, vaultManifest{}, ErrIncompleteVault
	}
	if err := initializeCipher(ctx, opts, filepath.Join(opts.RootDir, cipherDirName), key); err != nil {
		wipe(key)
		return nil, vaultManifest{}, err
	}
	manifest.State = manifestReadyState
	if err := writeManifestAt(rootFD, manifest, false); err != nil {
		wipe(key)
		return nil, vaultManifest{}, ErrIncompleteVault
	}
	return key, manifest, nil
}

func prepareRecognizedRoot(ctx context.Context, rootFD int, manifest vaultManifest, opts Options) ([]byte, error) {
	key, err := opts.Keyring.Lookup(ctx, opts.AppID, opts.ProfileID)
	if err != nil {
		wipe(key)
		return nil, ErrKeyringUnavailable
	}
	if !validMasterKey(key) {
		wipe(key)
		return nil, ErrInvalidMasterKey
	}
	cipherFD, cipherExists, err := openCipherDir(rootFD)
	if err != nil {
		wipe(key)
		return nil, ErrIncompleteVault
	}
	if !cipherExists {
		if manifest.State != manifestInitState {
			wipe(key)
			return nil, ErrIncompleteVault
		}
		if err := createCipherDir(rootFD); err != nil {
			wipe(key)
			return nil, ErrIncompleteVault
		}
		cipherFD, cipherExists, err = openCipherDir(rootFD)
		if err != nil || !cipherExists {
			wipe(key)
			return nil, ErrIncompleteVault
		}
	}
	defer syscall.Close(cipherFD)

	initialized, err := hasConfigMarker(cipherFD)
	if err != nil {
		wipe(key)
		return nil, ErrIncompleteVault
	}
	cipherEntries, err := directoryEntries(cipherFD)
	if err != nil {
		wipe(key)
		return nil, ErrIncompleteVault
	}
	if manifest.State == manifestReadyState && !initialized {
		wipe(key)
		return nil, ErrIncompleteVault
	}
	if !initialized {
		if len(cipherEntries) != 0 {
			wipe(key)
			return nil, ErrIncompleteVault
		}
		if err := initializeCipher(ctx, opts, filepath.Join(opts.RootDir, cipherDirName), key); err != nil {
			wipe(key)
			return nil, err
		}
	}
	if manifest.State == manifestInitState {
		manifest.State = manifestReadyState
		if err := writeManifestAt(rootFD, manifest, false); err != nil {
			wipe(key)
			return nil, ErrIncompleteVault
		}
	}
	return key, nil
}

func initializeCipher(ctx context.Context, opts Options, cipherPath string, key []byte) error {
	if err := validateExecutable(opts.GocryptfsPath); err != nil {
		return ErrInitializationFailed
	}
	if err := runEngine(ctx, opts, opts.GocryptfsPath, []string{"-init", "-q", "--", cipherPath}, key); err != nil {
		return ErrInitializationFailed
	}
	fd, exists, err := openCipherDirPath(cipherPath)
	if err != nil || !exists {
		return ErrIncompleteVault
	}
	defer syscall.Close(fd)
	initialized, err := hasConfigMarker(fd)
	if err != nil || !initialized {
		return ErrIncompleteVault
	}
	return nil
}

func runEngine(parent context.Context, opts Options, executable string, args []string, key []byte) error {
	timeout := boundedTimeout(opts.CommandTimeout, 10*time.Second, 60*time.Second)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	payload := append(append([]byte(nil), key...), '\n')
	defer wipe(payload)
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.WaitDelay = time.Second
	stdout := &limitedOutput{limit: maxCommandOutput}
	stderr := &limitedOutput{limit: maxCommandOutput}
	defer func() {
		wipe(stdout.Bytes())
		wipe(stderr.Bytes())
	}()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if err != nil || ctx.Err() != nil || stdout.overflow || stderr.overflow {
		return ErrInitializationFailed
	}
	return nil
}

type ownedProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	stdout  *limitedOutput
	stderr  *limitedOutput
}

func terminateOwnedProcess(process *ownedProcess) error {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return nil
	}
	select {
	case <-process.done:
		return nil
	default:
	}
	_ = process.cmd.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(750 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-process.done:
		return nil
	case <-timer.C:
	}
	_ = process.cmd.Process.Kill()
	select {
	case <-process.done:
		return nil
	case <-time.After(2 * time.Second):
		return ErrMountFailed
	}
}

func stopOwnedProcess(ctx context.Context, process *ownedProcess) error {
	if process == nil {
		return nil
	}
	select {
	case <-process.done:
		return nil
	default:
	}
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	_ = process.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
	}
	_ = process.cmd.Process.Kill()
	select {
	case <-process.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return ErrMountFailed
	}
}

func normalUnmount(parent context.Context, executable, mountPath string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-u", mountPath)
	cmd.WaitDelay = time.Second
	stdout := &limitedOutput{limit: 4096}
	stderr := &limitedOutput{limit: 4096}
	defer func() {
		wipe(stdout.Bytes())
		wipe(stderr.Bytes())
	}()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil || ctx.Err() != nil || stdout.overflow || stderr.overflow {
		return ErrUnmountFailed
	}
	return nil
}

func resolveUnmountPath(path string) (string, error) {
	if path == "" {
		found, err := exec.LookPath("fusermount3")
		if err != nil {
			return "", ErrMountFailed
		}
		return found, nil
	}
	return path, nil
}

func validateExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrMountFailed
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Mode()&0o022 != 0 {
		return ErrMountFailed
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != 0 && int(stat.Uid) != os.Geteuid()) {
		return ErrMountFailed
	}
	return nil
}

func openDirectory(path string, createFinal bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return -1, ErrUnsafePath
	}
	fd, err := syscall.Open(string(filepath.Separator), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrUnsafePath
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = syscall.Close(fd)
			return -1, ErrUnsafePath
		}
		next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(openErr, syscall.ENOENT) && createFinal && i == len(parts)-1 {
			if mkErr := syscall.Mkdirat(fd, part, 0o700); mkErr != nil && !errors.Is(mkErr, syscall.EEXIST) {
				_ = syscall.Close(fd)
				return -1, ErrUnsafePath
			}
			next, openErr = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		_ = syscall.Close(fd)
		if openErr != nil {
			return -1, ErrUnsafePath
		}
		fd = next
	}
	st := &syscall.Stat_t{}
	if err := syscall.Fstat(fd, st); err != nil || int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 {
		_ = syscall.Close(fd)
		return -1, ErrUnsafePath
	}
	return fd, nil
}

func verifyRootPath(path string, rootFD int) error {
	fd, err := openDirectory(path, false)
	if err != nil {
		return ErrUnsafePath
	}
	defer syscall.Close(fd)
	var current syscall.Stat_t
	var original syscall.Stat_t
	if syscall.Fstat(fd, &current) != nil {
		return ErrUnsafePath
	}
	if rootFD >= 0 {
		if syscall.Fstat(rootFD, &original) != nil || current.Dev != original.Dev || current.Ino != original.Ino {
			return ErrUnsafePath
		}
	}
	return nil
}

func directoryEntries(dirFD int) ([]string, error) {
	fd, err := syscall.Openat(dirFD, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "profilevault-directory")
	defer f.Close()
	names, err := f.Readdirnames(maxDirectoryItems + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > maxDirectoryItems {
		return nil, ErrUnsafePath
	}
	return names, nil
}

func createCipherDir(rootFD int) error {
	if err := syscall.Mkdirat(rootFD, cipherDirName, 0o700); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	fd, exists, err := openCipherDir(rootFD)
	if err != nil || !exists {
		return ErrUnsafePath
	}
	return syscall.Close(fd)
}

func openCipherDir(rootFD int) (int, bool, error) {
	fd, err := syscall.Openat(rootFD, cipherDirName, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return -1, false, nil
	}
	if err != nil {
		return -1, false, err
	}
	st := &syscall.Stat_t{}
	if err := syscall.Fstat(fd, st); err != nil || int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 {
		_ = syscall.Close(fd)
		return -1, false, ErrUnsafePath
	}
	return fd, true, nil
}

func openCipherDirPath(path string) (int, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, false, err
	}
	fd, err := openDirectory(abs, false)
	if err != nil {
		return -1, false, err
	}
	return fd, true, nil
}

func hasConfigMarker(cipherFD int) (bool, error) {
	fd, err := syscall.Openat(cipherFD, "gocryptfs.conf", syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.Close(fd)
	st := &syscall.Stat_t{}
	if err := syscall.Fstat(fd, st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return false, ErrUnsafePath
	}
	return true, nil
}

func readManifestAt(rootFD int) (vaultManifest, error) {
	fd, err := syscall.Openat(rootFD, manifestFileName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	f := os.NewFile(uintptr(fd), manifestFileName)
	defer f.Close()
	st := &syscall.Stat_t{}
	if syscall.Fstat(fd, st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || st.Mode&0o077 != 0 || st.Size < 2 || st.Size > maxManifestBytes {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil || len(data) > maxManifestBytes {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest vaultManifest
	if decoder.Decode(&manifest) != nil {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	if manifest.Format != manifestFormat {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	if manifest.Version != manifestVersion {
		return vaultManifest{}, ErrUnsupportedVersion
	}
	id, err := hex.DecodeString(manifest.VaultID)
	if err != nil || len(id) != 16 || (manifest.State != manifestInitState && manifest.State != manifestReadyState) {
		return vaultManifest{}, ErrUnrecognizedRoot
	}
	return manifest, nil
}

func writeManifestAt(rootFD int, manifest vaultManifest, create bool) error {
	data, err := json.Marshal(manifest)
	if err != nil || len(data) > maxManifestBytes {
		return ErrIncompleteVault
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return ErrIncompleteVault
	}
	temp := ".profilevault.tmp." + hex.EncodeToString(nonce)
	fd, err := syscall.Openat(rootFD, temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return ErrIncompleteVault
	}
	f := os.NewFile(uintptr(fd), temp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = syscall.Unlinkat(rootFD, temp)
		return ErrIncompleteVault
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = syscall.Unlinkat(rootFD, temp)
		return ErrIncompleteVault
	}
	if err := f.Close(); err != nil {
		_ = syscall.Unlinkat(rootFD, temp)
		return ErrIncompleteVault
	}
	if create {
		err = unix.Renameat2(rootFD, temp, rootFD, manifestFileName, unix.RENAME_NOREPLACE)
	} else {
		err = syscall.Renameat(rootFD, temp, rootFD, manifestFileName)
	}
	if err != nil {
		_ = syscall.Unlinkat(rootFD, temp)
		return ErrIncompleteVault
	}
	if err := syscall.Fsync(rootFD); err != nil {
		return ErrIncompleteVault
	}
	return nil
}

func newMasterKey() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	key := []byte(base64.RawStdEncoding.EncodeToString(raw))
	wipe(raw)
	return key, nil
}

func validMasterKey(key []byte) bool {
	if len(key) != 43 {
		return false
	}
	raw, err := base64.RawStdEncoding.DecodeString(string(key))
	if err != nil || len(raw) != 32 || base64.RawStdEncoding.EncodeToString(raw) != string(key) {
		wipe(raw)
		return false
	}
	wipe(raw)
	return true
}

func findMount(path string) (mountReference, bool, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return mountReference{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return mountReference{}, false, ErrMountFailed
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 || unescapeMountField(left[4]) != path {
			continue
		}
		id, err := strconv.Atoi(left[0])
		if err != nil {
			return mountReference{}, false, ErrMountFailed
		}
		superOptions := strings.Split(right[2], ",")
		allOptions := append(strings.Split(left[5], ","), superOptions...)
		owner := -1
		allowOther := false
		for _, option := range allOptions {
			if option == "allow_other" {
				allowOther = true
			}
			if strings.HasPrefix(option, "user_id=") {
				parsed, parseErr := strconv.Atoi(strings.TrimPrefix(option, "user_id="))
				if parseErr != nil {
					return mountReference{}, false, ErrMountFailed
				}
				owner = parsed
			}
		}
		statfsValid := false
		if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && owner < 0 {
				owner = int(st.Uid)
			}
			var fs syscall.Statfs_t
			if syscall.Statfs(path, &fs) == nil && uint64(fs.Type) == fuseSuperMagic {
				statfsValid = true
			}
		}
		return mountReference{
			id: id, fsType: right[0], source: unescapeMountField(right[1]),
			owner: owner, allowOther: allowOther, statfsValid: statfsValid,
		}, true, nil
	}
	if err := scanner.Err(); err != nil {
		return mountReference{}, false, err
	}
	return mountReference{}, false, nil
}

type mountReference struct {
	id          int
	fsType      string
	source      string
	owner       int
	allowOther  bool
	statfsValid bool
}

func unescapeMountField(value string) string {
	return strings.NewReplacer("\\134", "\\", "\\040", " ", "\\011", "\t", "\\012", "\n").Replace(value)
}

func removeOwnedMountpoint(path string, original os.FileInfo) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !sameFileIdentity(info, original) {
		return ErrUnsafePath
	}
	if err := os.Remove(path); err != nil {
		return ErrUnsafePath
	}
	return nil
}

func sameFileIdentity(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Dev == sb.Dev && sa.Ino == sb.Ino && int(sa.Uid) == os.Geteuid() && int(sb.Uid) == os.Geteuid()
}

func boundedTimeout(value, fallback, ceiling time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	if value > ceiling {
		return ceiling
	}
	return value
}

func contains(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}
