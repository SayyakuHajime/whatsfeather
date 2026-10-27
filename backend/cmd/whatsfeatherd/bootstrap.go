package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilemigration"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/rpc"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
)

const (
	vaultAppID           = "org.whatsfeather.WhatsFeather"
	vettedGocryptfsPath  = "/usr/bin/gocryptfs"
	vettedUnmountPath    = "/usr/bin/fusermount3"
	vettedSecretToolPath = "/usr/bin/secret-tool"
	shutdownCloseBudget  = 20 * time.Second
	shutdownRetryDelay   = 200 * time.Millisecond
)

type bootstrapOptions struct {
	dataDir    string
	runtimeDir string
	socketPath string
	demo       bool
	live       bool
	scope      *whatsapp.LiveTestScope
}

type vaultLifecycle interface {
	MountPath() string
	MountID() int
	MigrationStarted() bool
	MarkMigrationStarted() error
	Close(context.Context) error
}

type rpcRunner interface {
	Run(context.Context) error
}

type bootstrapDependencies struct {
	keyring         profilevault.Keyring
	gocryptfsPath   string
	fuseUnmountPath string
	openVault       func(context.Context, profilevault.Options) (vaultLifecycle, error)
	openApp         func(string, bool) (*app.App, error)
	newAdapter      func(*app.App, string, *whatsapp.LiveTestScope) app.Transport
	listen          func(string, *app.App) (rpcRunner, error)
}

func productionBootstrapDependencies() bootstrapDependencies {
	return bootstrapDependencies{
		keyring:         &profilevault.SecretToolKeyring{Executable: vettedSecretToolPath},
		gocryptfsPath:   vettedGocryptfsPath,
		fuseUnmountPath: vettedUnmountPath,
		openVault: func(ctx context.Context, options profilevault.Options) (vaultLifecycle, error) {
			return profilevault.Open(ctx, options)
		},
		openApp: app.Open,
		newAdapter: func(store *app.App, profilePath string, scope *whatsapp.LiveTestScope) app.Transport {
			if scope != nil {
				return whatsapp.NewWithLiveTestScope(store, profilePath, scope)
			}
			return whatsapp.New(store, profilePath)
		},
		listen: func(socketPath string, store *app.App) (rpcRunner, error) {
			return rpc.Listen(socketPath, store)
		},
	}
}

func bootstrap(ctx context.Context, options bootstrapOptions, deps bootstrapDependencies) (*daemonRuntime, error) {
	if ctx == nil {
		return nil, fmt.Errorf("startup context is required")
	}
	if options.demo == options.live {
		return nil, fmt.Errorf("select exactly one explicit storage mode: --demo or --live")
	}
	if options.scope != nil && (!options.live || options.demo) {
		return nil, fmt.Errorf("live-test scope requires explicit encrypted --live mode")
	}
	if err := validateBootstrapDependencies(deps, options.live); err != nil {
		return nil, err
	}
	dataDir, socketPath, runtimeDir, err := normalizeBootstrapPaths(options)
	if err != nil {
		return nil, err
	}

	if options.demo {
		return bootstrapDemo(ctx, options, dataDir, socketPath, deps)
	}
	return bootstrapLive(ctx, options, dataDir, runtimeDir, socketPath, deps)
}

func validateBootstrapDependencies(deps bootstrapDependencies, live bool) error {
	if deps.openApp == nil || deps.listen == nil {
		return fmt.Errorf("daemon app and RPC factories are required")
	}
	if !live {
		return nil
	}
	if deps.keyring == nil || deps.openVault == nil || deps.newAdapter == nil ||
		deps.gocryptfsPath == "" || deps.fuseUnmountPath == "" {
		return fmt.Errorf("encrypted live storage dependencies are incomplete")
	}
	return nil
}

func normalizeBootstrapPaths(options bootstrapOptions) (string, string, string, error) {
	if options.dataDir == "" || options.socketPath == "" || !filepath.IsAbs(options.dataDir) || !filepath.IsAbs(options.socketPath) {
		return "", "", "", fmt.Errorf("absolute --data-dir and --socket paths are required")
	}
	dataDir, err := filepath.Abs(options.dataDir)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve profile root: %w", err)
	}
	socketPath, err := filepath.Abs(options.socketPath)
	if err != nil || filepath.Clean(socketPath) != socketPath || len(socketPath) > 103 {
		return "", "", "", fmt.Errorf("socket path must be clean, absolute, and at most 103 bytes")
	}
	runtimeDir := options.runtimeDir
	if runtimeDir == "" {
		runtimeDir = filepath.Dir(socketPath)
	}
	if !filepath.IsAbs(runtimeDir) {
		return "", "", "", fmt.Errorf("runtime directory must be absolute")
	}
	runtimeDir, err = filepath.Abs(runtimeDir)
	if err != nil || filepath.Clean(runtimeDir) != runtimeDir {
		return "", "", "", fmt.Errorf("runtime directory must be clean and absolute")
	}
	return filepath.Clean(dataDir), socketPath, runtimeDir, nil
}

func bootstrapDemo(ctx context.Context, options bootstrapOptions, dataDir, socketPath string, deps bootstrapDependencies) (*daemonRuntime, error) {
	if err := whatsapp.RejectOrdinaryProfileMode(dataDir); err != nil {
		return nil, err
	}
	if err := ensureDemoProfileDirectory(dataDir); err != nil {
		return nil, err
	}
	lock, err := lockDemoProfile(dataDir)
	if err != nil {
		return nil, err
	}
	store, err := deps.openApp(dataDir, true)
	if err != nil {
		return nil, releaseDemoLock(lock, err)
	}
	server, err := deps.listen(socketPath, store)
	if err != nil {
		return nil, cleanupStartupFailure(store, nil, lock, err)
	}
	return &daemonRuntime{app: store, server: server, demoLock: lock}, nil
}

func bootstrapLive(ctx context.Context, options bootstrapOptions, dataDir, runtimeDir, socketPath string, deps bootstrapDependencies) (*daemonRuntime, error) {
	if filepath.Dir(socketPath) != runtimeDir {
		return nil, fmt.Errorf("live RPC socket must be a direct child of its private runtime directory")
	}
	if pathContains(dataDir, runtimeDir) || pathContains(runtimeDir, dataDir) {
		return nil, fmt.Errorf("encrypted profile root and runtime directory must be separate")
	}
	if err := validateNoSymlinkComponents(dataDir); err != nil {
		return nil, err
	}
	if err := validateNoSymlinkComponents(runtimeDir); err != nil {
		return nil, err
	}
	if err := preflightVaultRoot(dataDir); err != nil {
		return nil, err
	}
	if err := ensureDirectory(dataDir, true); err != nil {
		return nil, err
	}
	if err := ensureDirectory(runtimeDir, true); err != nil {
		return nil, fmt.Errorf("secure runtime directory: %w", err)
	}

	profileID := profileKeyringIdentity(dataDir)
	vault, err := deps.openVault(ctx, profilevault.Options{
		RootDir: dataDir, RuntimeDir: runtimeDir,
		AppID: vaultAppID, ProfileID: profileID,
		Keyring: deps.keyring, GocryptfsPath: deps.gocryptfsPath,
		FuseUnmountPath: deps.fuseUnmountPath,
	})
	if err != nil {
		return nil, fmt.Errorf("open mandatory encrypted profile vault: %w", err)
	}
	if vault == nil {
		return nil, fmt.Errorf("open mandatory encrypted profile vault: empty vault owner")
	}
	mountPath := vault.MountPath()
	if !filepath.IsAbs(mountPath) || filepath.Clean(mountPath) != mountPath || filepath.Dir(mountPath) != runtimeDir {
		return nil, cleanupStartupFailure(nil, vault, nil, fmt.Errorf("vault mount is outside the private runtime directory"))
	}
	if err := profilemigration.CheckStartupStateWithIntent(mountPath, dataDir, vault.MigrationStarted()); err != nil {
		return nil, cleanupStartupFailure(nil, vault, nil, fmt.Errorf("encrypted profile migration state: %w", err))
	}
	if options.scope != nil {
		if _, err := whatsapp.PrepareScopedLiveTestProfile(mountPath, options.scope); err != nil {
			return nil, cleanupStartupFailure(nil, vault, nil, fmt.Errorf("prepare encrypted scoped profile: %w", err))
		}
	} else if err := whatsapp.RejectOrdinaryProfileMode(mountPath); err != nil {
		return nil, cleanupStartupFailure(nil, vault, nil, err)
	}

	store, err := deps.openApp(mountPath, false)
	if err != nil {
		return nil, cleanupStartupFailure(nil, vault, nil, fmt.Errorf("open app inside encrypted profile: %w", err))
	}
	if err := store.SetEncryptedStorageView(dataDir, runtimeDir, mountPath, vault.MountID()); err != nil {
		return nil, cleanupStartupFailure(store, vault, nil, fmt.Errorf("configure encrypted profile view: %w", err))
	}
	transport := deps.newAdapter(store, mountPath, options.scope)
	if transport == nil {
		return nil, cleanupStartupFailure(store, vault, nil, fmt.Errorf("construct inert live transport adapter"))
	}
	store.SetTransport(transport)
	server, err := deps.listen(socketPath, store)
	if err != nil {
		return nil, cleanupStartupFailure(store, vault, nil, fmt.Errorf("listen on private RPC socket: %w", err))
	}
	return &daemonRuntime{app: store, vault: vault, server: server}, nil
}

func cleanupStartupFailure(store *app.App, vault vaultLifecycle, demoLock *os.File, cause error) error {
	var cleanupErr error
	if store != nil {
		if err := store.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close app before vault cleanup: %w", err))
		}
	}
	// Keep the mount and vault lock owned if SQLite or transport teardown failed.
	if cleanupErr == nil && vault != nil {
		if err := vault.Close(context.Background()); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close encrypted profile vault: %w", err))
		}
	}
	if cleanupErr == nil && demoLock != nil {
		if err := releaseDemoLock(demoLock, nil); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return errors.Join(cause, cleanupErr)
}

func profileKeyringIdentity(root string) string {
	digest := sha256.Sum256([]byte(root))
	return "profile-path-sha256-" + hex.EncodeToString(digest[:])
}

func preflightVaultRoot(root string) error {
	if err := validateNoSymlinkComponents(root); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return profilevault.ErrUnsafePath
	}
	if !ownedPrivateDirectory(info) {
		return profilevault.ErrUnsafePath
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return profilevault.ErrUnsafePath
	}
	manifestFound := false
	unknownFound := false
	cipherFound := false
	for _, entry := range entries {
		switch entry.Name() {
		case ".profilevault.lock":
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return profilevault.ErrUnsafePath
			}
		case ".profilevault.json":
			manifestFound = true
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return profilevault.ErrUnrecognizedRoot
			}
		case "cipher":
			cipherFound = true
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return profilevault.ErrUnrecognizedRoot
			}
		default:
			unknownFound = true
		}
	}
	if manifestFound && unknownFound {
		return profilevault.ErrUnrecognizedRoot
	}
	if !manifestFound && (unknownFound || cipherFound) {
		return profilevault.ErrLegacyPlaintext
	}
	return nil
}

func ensureDemoProfileDirectory(path string) error {
	if err := validateNoSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return profilevault.ErrUnsafePath
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return profilevault.ErrUnsafePath
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return ensureDirectory(path, false)
}

func ensureDirectory(path string, create bool) error {
	if err := validateNoSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedPrivateDirectory(info) {
		return profilevault.ErrUnsafePath
	}
	return nil
}

func ownedPrivateDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func validateNoSymlinkComponents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return profilevault.ErrUnsafePath
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return profilevault.ErrUnsafePath
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func lockDemoProfile(dir string) (*os.File, error) {
	path := filepath.Join(dir, "profile.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), path)
	stat := &syscall.Stat_t{}
	if syscall.Fstat(fd, stat) != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 || stat.Mode&0077 != 0 {
		_ = lock.Close()
		return nil, profilevault.ErrUnsafePath
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("demo profile is already in use")
	}
	return lock, nil
}

func releaseDemoLock(lock *os.File, cause error) error {
	if lock == nil {
		return cause
	}
	fd := int(lock.Fd())
	unlockErr := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := lock.Close()
	return errors.Join(cause, unlockErr, closeErr)
}

func closeAppWithin(ctx context.Context, store *app.App) error {
	result := make(chan error, 1)
	go func() { result <- store.Close() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type daemonRuntime struct {
	app      *app.App
	vault    vaultLifecycle
	server   rpcRunner
	demoLock *os.File

	mu             sync.Mutex
	closing        bool
	closeDone      chan struct{}
	appClosed      bool
	closed         bool
	lastCloseError error
}

func (d *daemonRuntime) Run(ctx context.Context) error {
	if d == nil || d.server == nil || ctx == nil {
		return fmt.Errorf("daemon runtime is incomplete")
	}
	runErr := d.server.Run(ctx)
	closeCtx, cancel := context.WithTimeout(context.Background(), shutdownCloseBudget)
	defer cancel()
	closeErr := d.closeWithBusyRetry(closeCtx)
	return errors.Join(runErr, closeErr)
}

func (d *daemonRuntime) Close(ctx context.Context) error {
	if d == nil || ctx == nil {
		return fmt.Errorf("daemon close requires a runtime and context")
	}
	for {
		d.mu.Lock()
		if d.closed {
			err := d.lastCloseError
			d.mu.Unlock()
			return err
		}
		if d.closing {
			done := d.closeDone
			d.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		d.closing = true
		d.closeDone = make(chan struct{})
		closeDone := d.closeDone
		appClosed := d.appClosed
		d.mu.Unlock()

		var closeErr error
		if !appClosed && d.app != nil {
			closeErr = closeAppWithin(ctx, d.app)
			if closeErr == nil {
				d.mu.Lock()
				d.appClosed = true
				d.mu.Unlock()
			}
		}
		if closeErr == nil && d.vault != nil {
			closeErr = d.vault.Close(ctx)
		}
		if closeErr == nil && d.demoLock != nil {
			closeErr = releaseDemoLock(d.demoLock, nil)
			if closeErr == nil {
				d.demoLock = nil
			}
		}

		d.mu.Lock()
		d.lastCloseError = closeErr
		if closeErr == nil {
			d.closed = true
		}
		d.closing = false
		close(closeDone)
		d.mu.Unlock()
		return closeErr
	}
}

func (d *daemonRuntime) closeWithBusyRetry(ctx context.Context) error {
	var lastErr error
	for {
		lastErr = d.Close(ctx)
		if lastErr == nil || !errors.Is(lastErr, profilevault.ErrUnmountFailed) || ctx.Err() != nil {
			return lastErr
		}
		timer := time.NewTimer(shutdownRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return lastErr
		}
	}
}

var _ rpcRunner = (*rpc.Server)(nil)
var _ vaultLifecycle = (*profilevault.Vault)(nil)
