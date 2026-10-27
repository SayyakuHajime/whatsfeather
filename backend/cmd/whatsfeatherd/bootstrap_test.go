package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

func TestBootstrapRejectsLegacyPlaintextWithoutMutationOrSubordinateStartup(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "profile")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	plain := []byte("SQLite format 3\x00SYNTHETIC_LEGACY_PROFILE")
	if err := os.WriteFile(filepath.Join(root, "history.db"), plain, 0600); err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		calls = append(calls, "vault")
		return nil, errors.New("unexpected vault open")
	}
	deps.openApp = func(string, bool) (*app.App, error) {
		calls = append(calls, "app")
		return nil, errors.New("unexpected app open")
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		calls = append(calls, "adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		calls = append(calls, "listen")
		return nil, errors.New("unexpected RPC listener")
	}

	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if runtime != nil || !errors.Is(err, profilevault.ErrLegacyPlaintext) {
		t.Fatalf("bootstrap=(%v,%v), want legacy plaintext rejection", runtime, err)
	}
	if len(calls) != 0 {
		t.Fatalf("legacy root reached later startup stages: %v", calls)
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entryNamesForTest(beforeEntries), entryNamesForTest(afterEntries)) {
		t.Fatalf("legacy root entries changed: before=%v after=%v", entryNamesForTest(beforeEntries), entryNamesForTest(afterEntries))
	}
	got, err := os.ReadFile(filepath.Join(root, "history.db"))
	if err != nil || string(got) != string(plain) {
		t.Fatalf("legacy bytes changed: got=%q err=%v", got, err)
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy rejection created runtime directory: %v", err)
	}
}

func TestBootstrapClosesAppTransportBeforeVaultAndRetriesBusyVaultClose(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "encrypted-root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	vaultCloseCalls := 0
	transport := &recordingTransport{onDisconnect: func() { order = append(order, "transport-disconnect") }}
	vault := &fakeVault{mountPath: mount, mountID: 42, closeFunc: func(context.Context) error {
		vaultCloseCalls++
		order = append(order, "vault-close")
		if vaultCloseCalls == 1 {
			return profilevault.ErrUnmountFailed
		}
		if !reflect.DeepEqual(order[:2], []string{"transport-disconnect", "vault-close"}) {
			return fmt.Errorf("vault close preceded transport teardown: %v", order)
		}
		return nil
	}}
	deps := testBootstrapDependencies()
	deps.openVault = func(_ context.Context, opts profilevault.Options) (vaultLifecycle, error) {
		if opts.RootDir != root || opts.RuntimeDir != runtimeDir || opts.AppID == "" || opts.ProfileID == "" {
			t.Fatalf("vault identity/paths not explicit: %#v", opts)
		}
		return vault, nil
	}
	deps.openApp = app.Open
	deps.newAdapter = func(_ *app.App, dir string, _ *whatsapp.LiveTestScope) app.Transport {
		if dir != mount {
			t.Fatalf("adapter got plaintext or persistent-root path %q, want mounted path %q", dir, mount)
		}
		return transport
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) { return inertRPC{}, nil }

	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if transport.connectCalls != 0 {
		t.Fatalf("bootstrap connected transport %d times", transport.connectCalls)
	}
	if err := runtime.Close(context.Background()); !errors.Is(err, profilevault.ErrUnmountFailed) {
		t.Fatalf("first close error=%v, want retryable busy unmount", err)
	}
	if transport.disconnectCalls != 1 {
		t.Fatalf("transport disconnect count after first close=%d, want 1", transport.disconnectCalls)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"transport-disconnect", "vault-close", "vault-close"}) {
		t.Fatalf("teardown order=%v", order)
	}
}

func TestBootstrapMissingKeyringFailsBeforeAppAdapterOrSocket(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "encrypted-root")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	var calls []string
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		calls = append(calls, "vault")
		return nil, profilevault.ErrKeyringUnavailable
	}
	deps.openApp = func(string, bool) (*app.App, error) {
		calls = append(calls, "app")
		return nil, errors.New("unexpected app open")
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		calls = append(calls, "adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		calls = append(calls, "listen")
		return nil, errors.New("unexpected listener")
	}
	socket := filepath.Join(runtimeDir, "daemon.sock")
	if runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: socket, live: true,
	}, deps); runtime != nil || !errors.Is(err, profilevault.ErrKeyringUnavailable) {
		t.Fatalf("bootstrap=(%v,%v), want keyring failure", runtime, err)
	}
	if !reflect.DeepEqual(calls, []string{"vault"}) {
		t.Fatalf("missing-key path reached later stages: %v", calls)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-key path created a socket: %v", err)
	}
}

func TestRuntimePropagatesTransportCloseErrorWithoutUnmountingVault(t *testing.T) {
	closeErr := errors.New("synthetic transport store close failure")
	transport := &closeResultTransport{closeErr: closeErr}
	vaultCloseCalls := 0
	runtime, _ := bootstrapWithTestTransport(t, transport, func(context.Context) error {
		vaultCloseCalls++
		return nil
	})
	if err := runtime.Close(context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("runtime close error=%v, want transport store close error", err)
	}
	if vaultCloseCalls != 0 {
		t.Fatalf("vault closed %d times after transport close failed; mount ownership must be retained", vaultCloseCalls)
	}
	if transport.errorDisconnectCalls != 1 || transport.disconnectCalls != 0 {
		t.Fatalf("transport teardown used error-aware=%d legacy=%d", transport.errorDisconnectCalls, transport.disconnectCalls)
	}
}

func TestRuntimeBoundsAppTeardownAndKeepsVaultOwnedUntilRetry(t *testing.T) {
	release := make(chan struct{})
	transport := &closeResultTransport{started: make(chan struct{}), release: release}
	vaultCloseCalls := 0
	runtime, _ := bootstrapWithTestTransport(t, transport, func(context.Context) error {
		vaultCloseCalls++
		return nil
	})
	time.AfterFunc(250*time.Millisecond, func() { close(release) })
	closeCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runtime.Close(closeCtx) }()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("transport teardown did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded close error=%v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime close did not honor its cancellation deadline")
	}
	if vaultCloseCalls != 0 {
		t.Fatalf("vault closed %d times before app teardown completed", vaultCloseCalls)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("retry after app teardown completed: %v", err)
	}
	if vaultCloseCalls != 1 {
		t.Fatalf("vault close calls after retry=%d, want 1", vaultCloseCalls)
	}
}

func bootstrapWithTestTransport(t *testing.T, transport app.Transport, closeVault func(context.Context) error) (*daemonRuntime, *fakeVault) {
	t.Helper()
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	vault := &fakeVault{mountPath: mount, mountID: 42, closeFunc: closeVault}
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) { return vault, nil }
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport { return transport }
	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, vault
}

func TestBootstrapAppOpenFailureClosesVaultBeforeAdapterOrRPC(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	appErr := errors.New("synthetic app open failure")
	var stages []string
	vault := &fakeVault{mountPath: mount, mountID: 42, closeFunc: func(context.Context) error {
		stages = append(stages, "vault-close")
		return nil
	}}
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		return vault, nil
	}
	deps.openApp = func(string, bool) (*app.App, error) {
		stages = append(stages, "app-open")
		return nil, appErr
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		stages = append(stages, "adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		stages = append(stages, "listen")
		return nil, nil
	}
	socket := filepath.Join(runtimeDir, "daemon.sock")
	if runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: socket, live: true,
	}, deps); runtime != nil || !errors.Is(err, appErr) {
		t.Fatalf("bootstrap=(%v,%v), want app open error", runtime, err)
	}
	if !reflect.DeepEqual(stages, []string{"app-open", "vault-close"}) {
		t.Fatalf("failed app startup stages=%v", stages)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed app startup created a socket: %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("failed app startup wrote plaintext/profile data: entries=%v err=%v", entryNamesForTest(entries), err)
	}
}

func TestScopedLiveProfilePreparationStaysInsideMountedView(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	scopePath := filepath.Join(base, "scope.json")
	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	syntheticChat := types.NewJID("synthetic-test-group", types.GroupServer).String()
	scopeData, err := json.Marshal(map[string]any{
		// Symbolic parser fixture only; the injected recording transport never connects.
		"since": since, "chats": []string{syntheticChat}, "include_self": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopePath, scopeData, 0600); err != nil {
		t.Fatal(err)
	}
	scope, err := whatsapp.LoadLiveTestScope(scopePath)
	if err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{}
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		return &fakeVault{mountPath: mount, mountID: 42}, nil
	}
	deps.openApp = app.Open
	deps.newAdapter = func(_ *app.App, path string, received *whatsapp.LiveTestScope) app.Transport {
		if path != mount || received != scope {
			t.Fatalf("scoped adapter path/scope=(%q,%p), want (%q,%p)", path, received, mount, scope)
		}
		return transport
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) { return inertRPC{}, nil }
	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true, scope: scope,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mount, ".live-test-scope")); err != nil {
		t.Fatalf("scoped profile marker not written inside mounted profile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".live-test-scope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scope profile preparation touched encrypted backing root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mount, "history.db")); err != nil {
		t.Fatalf("live app was not opened inside mounted profile: %v", err)
	}
	if transport.connectCalls != 0 || transport.sendCalls != 0 {
		t.Fatalf("scoped live bootstrap dispatched transport connect=%d send=%d", transport.connectCalls, transport.sendCalls)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close scoped live runtime: %v", err)
	}
}

func TestBootstrapRejectsAmbiguousOrMisplacedLiveRuntime(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "root")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		t.Fatal("invalid mode/path reached vault open")
		return nil, nil
	}
	for _, options := range []bootstrapOptions{
		{dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock")},
		{dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(base, "outside.sock"), live: true},
		{dataDir: root, runtimeDir: root, socketPath: filepath.Join(root, "daemon.sock"), live: true},
	} {
		if runtime, err := bootstrap(context.Background(), options, deps); runtime != nil || err == nil {
			t.Fatalf("invalid startup options unexpectedly succeeded: options=%#v runtime=%v err=%v", options, runtime, err)
		}
	}
}

func TestCLIRequiresAnExplicitDemoOrLiveStorageMode(t *testing.T) {
	base := shortBootstrapTestDir(t)
	data := filepath.Join(base, "profile")
	socket := filepath.Join(base, "daemon.sock")
	err := runWithArgs([]string{"--data-dir", data, "--socket", socket}, testBootstrapDependencies())
	if err == nil || !strings.Contains(err.Error(), "exactly one explicit storage mode") {
		t.Fatalf("implicit startup mode error=%v, want explicit --demo or --live rejection", err)
	}
	if _, statErr := os.Lstat(data); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("implicit-mode rejection touched profile path: %v", statErr)
	}
	if _, statErr := os.Lstat(socket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("implicit-mode rejection touched socket path: %v", statErr)
	}
}

func TestExplicitDemoKeepsSyntheticPlaintextCompatibility(t *testing.T) {
	base := shortBootstrapTestDir(t)
	dataDir := filepath.Join(base, "demo-profile")
	runtimeDir := filepath.Join(base, "runtime")
	if err := os.Mkdir(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	deps := testBootstrapDependencies()
	deps.openApp = app.Open
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		t.Fatal("synthetic demo must not construct a live adapter")
		return nil
	}
	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: dataDir, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "demo.sock"), demo: true,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dataDir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("explicit synthetic profile permissions=%v err=%v", info, err)
	}
	status, err := runtime.app.Handle(context.Background(), "app.status", nil)
	if err != nil || status.(map[string]any)["mode"] != "demo" {
		t.Fatalf("explicit demo app status=%v err=%v", status, err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("close explicit demo runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "history.db")); err != nil {
		t.Fatalf("explicit demo did not persist synthetic plaintext history: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".profilevault.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit demo unexpectedly created a live vault: %v", err)
	}
}

func shortBootstrapTestDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "wf-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

type fakeKeyring struct{}

func (fakeKeyring) Lookup(context.Context, string, string) ([]byte, error) {
	return nil, profilevault.ErrKeyNotFound
}
func (fakeKeyring) Store(context.Context, string, string, []byte) error { return nil }

type fakeVault struct {
	mountPath        string
	mountID          int
	migrationStarted bool
	closeFunc        func(context.Context) error
}

func (v *fakeVault) MountPath() string           { return v.mountPath }
func (v *fakeVault) MountID() int                { return v.mountID }
func (v *fakeVault) MigrationStarted() bool      { return v.migrationStarted }
func (v *fakeVault) MarkMigrationStarted() error { v.migrationStarted = true; return nil }
func (v *fakeVault) Close(ctx context.Context) error {
	if v.closeFunc != nil {
		return v.closeFunc(ctx)
	}
	return nil
}

type inertRPC struct{}

func (inertRPC) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

type recordingTransport struct {
	connectCalls    int
	disconnectCalls int
	sendCalls       int
	onDisconnect    func()
}

func (t *recordingTransport) Connect(context.Context) error { t.connectCalls++; return nil }
func (t *recordingTransport) Disconnect() {
	t.disconnectCalls++
	if t.onDisconnect != nil {
		t.onDisconnect()
	}
}
func (t *recordingTransport) Send(context.Context, string, string, string) (int64, error) {
	t.sendCalls++
	return 0, nil
}

type closeResultTransport struct {
	disconnectCalls      int
	errorDisconnectCalls int
	closeErr             error
	started              chan struct{}
	release              <-chan struct{}
	startOnce            sync.Once
}

func (*closeResultTransport) Connect(context.Context) error { return nil }
func (t *closeResultTransport) Disconnect()                 { t.disconnectCalls++ }
func (t *closeResultTransport) DisconnectWithError() error {
	t.errorDisconnectCalls++
	if t.started != nil {
		t.startOnce.Do(func() { close(t.started) })
	}
	if t.release != nil {
		<-t.release
	}
	return t.closeErr
}
func (*closeResultTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}

func entryNamesForTest(entries []os.DirEntry) []string {
	result := make([]string, len(entries))
	for i, entry := range entries {
		result[i] = entry.Name()
	}
	return result
}

func testBootstrapDependencies() bootstrapDependencies {
	return bootstrapDependencies{
		keyring:         fakeKeyring{},
		gocryptfsPath:   "/synthetic/gocryptfs",
		fuseUnmountPath: "/synthetic/fusermount3",
		openVault: func(context.Context, profilevault.Options) (vaultLifecycle, error) {
			return nil, errors.New("unexpected vault open")
		},
		openApp: app.Open,
		newAdapter: func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
			return &recordingTransport{}
		},
		listen: func(string, *app.App) (rpcRunner, error) { return inertRPC{}, nil },
	}
}
