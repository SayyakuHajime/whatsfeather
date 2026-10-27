package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilemigration"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
)

func TestMigrationPreflightRejectsARealDaemonOwnerRelaunch(t *testing.T) {
	if os.Getenv("WF_MIGRATION_OWNER_CHILD") == "1" {
		lock, err := lockDemoProfile(os.Getenv("WF_MIGRATION_OWNER_SOURCE"))
		if err != nil {
			t.Fatal(err)
		}
		defer releaseDemoLock(lock, nil)
		if err := os.WriteFile(os.Getenv("WF_MIGRATION_OWNER_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	base := shortBootstrapTestDir(t)
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	ready := filepath.Join(base, "daemon-owner.ready")
	command := exec.Command(os.Args[0], "-test.run=^TestMigrationPreflightRejectsARealDaemonOwnerRelaunch$")
	command.Env = append(os.Environ(), "WF_MIGRATION_OWNER_CHILD=1", "WF_MIGRATION_OWNER_SOURCE="+source, "WF_MIGRATION_OWNER_READY="+ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("compliant daemon relaunch did not acquire profile.lock: %v", err)
	}
	if plan, err := profilemigration.Prepare(source, destination); plan != nil || !errors.Is(err, profilemigration.ErrSourceBusy) {
		if plan != nil {
			_ = plan.Close()
		}
		t.Fatalf("migration preflight during a real daemon owner relaunch = (%v,%v), want cooperative-lock busy rejection", plan, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("daemon-owner probe exit = %v, want stopped synthetic child", err)
	}
	plan, err := profilemigration.Prepare(source, destination)
	if err != nil {
		t.Fatalf("migration preflight after compliant owner exits: %v", err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineMigrationCLICompletesBoundedCopyWithoutDaemonOrConnect(t *testing.T) {
	base := shortBootstrapTestDir(t)
	source := filepath.Join(base, "synthetic-legacy-live")
	destination := filepath.Join(base, "synthetic-encrypted-profile")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), app.Message{
		ID: "offline-migration-message", ChatID: "synthetic:contact", Body: "migration-fixture-only",
		Timestamp: 1770000000000, Status: "received",
	}, "Synthetic contact"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	sourceDBBefore, err := os.ReadFile(filepath.Join(source, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}

	vaultCloseCalls := 0
	var migrationVault *fakeVault
	deps := testBootstrapDependencies()
	deps.openVault = func(_ context.Context, options profilevault.Options) (vaultLifecycle, error) {
		if options.RootDir != destination || options.ProfileID != profileKeyringIdentity(destination) {
			t.Fatalf("offline migration destination identity mismatch: %+v", options)
		}
		if err := os.Mkdir(destination, 0700); err != nil {
			return nil, err
		}
		migrationVault = &fakeVault{mountPath: mount, mountID: 73, closeFunc: func(context.Context) error {
			vaultCloseCalls++
			return nil
		}}
		return migrationVault, nil
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		t.Fatal("offline migration must not construct a live adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		t.Fatal("offline migration must not open an RPC listener")
		return nil, nil
	}
	if err := runWithArgs([]string{
		"--migrate-live-source", source,
		"--data-dir", destination,
		"--runtime-dir", runtimeDir,
	}, deps); err != nil {
		t.Fatalf("explicit offline migration command: %v", err)
	}
	if vaultCloseCalls != 1 {
		t.Fatalf("migration closed encrypted destination %d times, want normal close once", vaultCloseCalls)
	}
	if migrationVault == nil || !migrationVault.MigrationStarted() {
		t.Fatal("offline migration copied data without first publishing destination-bound outer intent")
	}
	if err := profilemigration.CheckStartupState(mount, destination); err != nil {
		t.Fatalf("CLI did not publish a destination-bound completed state: %v", err)
	}
	sourceDBAfter, err := os.ReadFile(filepath.Join(source, "history.db"))
	if err != nil || !reflect.DeepEqual(sourceDBBefore, sourceDBAfter) {
		t.Fatalf("offline CLI migration changed source database bytes: before=%d after=%d err=%v", len(sourceDBBefore), len(sourceDBAfter), err)
	}
	migrated, err := app.Open(mount, false)
	if err != nil {
		t.Fatalf("open copied marked-live profile: %v", err)
	}
	defer migrated.Close()
	result, err := migrated.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic:contact","limit":10}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !bytes.Contains(encoded, []byte("migration-fixture-only")) {
		t.Fatalf("copied history was not readable through app projections: %s err=%v", encoded, err)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline CLI migration unexpectedly created a daemon socket: %v", err)
	}
}

func TestOfflineMigrationCLIUsesDistinctEncryptedDestinationWithoutStartingDaemon(t *testing.T) {
	base := shortBootstrapTestDir(t)
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "encrypted-destination")
	runtimeDir := filepath.Join(base, "runtime")
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	deviceDB, err := sql.Open("sqlite", filepath.Join(source, "device.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = deviceDB.Exec("CREATE TABLE synthetic_device(marker TEXT); INSERT INTO synthetic_device VALUES('fake-device-only')"); err != nil {
		_ = deviceDB.Close()
		t.Fatal(err)
	}
	if err = deviceDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(source, "device.db"), 0600); err != nil {
		t.Fatal(err)
	}

	vaultErr := errors.New("synthetic stop after explicit migration preflight")
	vaultCalls := 0
	var openedOptions profilevault.Options
	deps := testBootstrapDependencies()
	deps.openVault = func(_ context.Context, options profilevault.Options) (vaultLifecycle, error) {
		vaultCalls++
		openedOptions = options
		return nil, vaultErr
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		t.Fatal("offline migration must not start an RPC listener")
		return nil, nil
	}
	err = runWithArgs([]string{
		"--migrate-live-source", source,
		"--data-dir", destination,
		"--runtime-dir", runtimeDir,
	}, deps)
	if !errors.Is(err, vaultErr) {
		t.Fatalf("offline migration error = %v, want encrypted destination open after accepted live-source preflight", err)
	}
	if vaultCalls != 1 || openedOptions.RootDir != destination || openedOptions.ProfileID != profileKeyringIdentity(destination) {
		t.Fatalf("migration opened vault %d times with options %+v; destination identity was not kept path-bound", vaultCalls, openedOptions)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline migration created a daemon socket: %v", err)
	}
}

func TestMigrationHoldsSourceLockThroughFinalPublicationAgainstDaemonRelaunch(t *testing.T) {
	if mode := os.Getenv("WF_MIGRATION_LOCK_PROBE_MODE"); mode != "" {
		source := os.Getenv("WF_MIGRATION_LOCK_PROBE_SOURCE")
		lock, err := lockDemoProfile(source)
		if mode == "busy" {
			if err == nil {
				_ = releaseDemoLock(lock, nil)
				t.Fatal("compliant daemon reacquired profile.lock while migration still owned it")
			}
			if !strings.Contains(err.Error(), "already in use") {
				t.Fatalf("daemon relaunch lock error = %v, want active-owner rejection", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("compliant daemon could not reacquire profile.lock after migration: %v", err)
		}
		defer releaseDemoLock(lock, nil)
		if err := os.WriteFile(os.Getenv("WF_MIGRATION_LOCK_PROBE_READY"), []byte("acquired"), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	base := shortBootstrapTestDir(t)
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	plan, err := profilemigration.Prepare(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	probe := exec.Command(os.Args[0], "-test.run=^TestMigrationHoldsSourceLockThroughFinalPublicationAgainstDaemonRelaunch$")
	probe.Env = append(os.Environ(), "WF_MIGRATION_LOCK_PROBE_MODE=busy", "WF_MIGRATION_LOCK_PROBE_SOURCE="+source)
	probed := false
	err = plan.Migrate(context.Background(), mount, profilemigration.Options{Failpoint: func(point profilemigration.MigrationPoint) error {
		if point != profilemigration.PointBeforeComplete {
			return nil
		}
		output, probeErr := probe.CombinedOutput()
		if probeErr != nil {
			return fmt.Errorf("synthetic daemon relaunch probe failed: %w (%s)", probeErr, output)
		}
		if !bytes.Contains(output, []byte("PASS")) {
			return fmt.Errorf("synthetic daemon relaunch probe did not pass: %s", output)
		}
		probed = true
		return nil
	}})
	if err != nil || !probed {
		t.Fatalf("migration while compliant daemon relaunch was refused: err=%v probed=%t", err, probed)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(base, "daemon-relaunch.ready")
	command := exec.Command(os.Args[0], "-test.run=^TestMigrationHoldsSourceLockThroughFinalPublicationAgainstDaemonRelaunch$")
	command.Env = append(os.Environ(), "WF_MIGRATION_LOCK_PROBE_MODE=acquire", "WF_MIGRATION_LOCK_PROBE_SOURCE="+source, "WF_MIGRATION_LOCK_PROBE_READY="+ready)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("compliant daemon relaunch did not acquire profile.lock after Plan.Close")
}

func TestMigrationRetryErrorExplainsNewDestinationAndPreservesExistingData(t *testing.T) {
	base := shortBootstrapTestDir(t)
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "existing-encrypted-vault")
	runtimeDir := filepath.Join(base, "runtime")
	for _, dir := range []string{destination, runtimeDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	sourceBefore, err := os.ReadFile(filepath.Join(source, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(destination, "cipher")
	if err := os.WriteFile(sentinelPath, []byte("synthetic-existing-vault"), 0600); err != nil {
		t.Fatal(err)
	}
	destinationBefore, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	deps := testBootstrapDependencies()
	vaultOpens := 0
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		vaultOpens++
		return nil, errors.New("existing vault must not be opened or reset")
	}

	err = runOfflineMigration(context.Background(), source, destination, runtimeDir, deps)
	if !errors.Is(err, profilemigration.ErrDestinationNotEmpty) {
		t.Fatalf("retry against existing destination error = %v, want non-empty-destination rejection", err)
	}
	for _, hint := range []string{"not resumable", "keep the plaintext source", "different new --data-dir", "Do not delete the source", "reset an existing vault"} {
		if !strings.Contains(err.Error(), hint) {
			t.Fatalf("operator error %q omitted recovery instruction %q", err, hint)
		}
	}
	if vaultOpens != 0 {
		t.Fatalf("existing encrypted vault was opened %d times before rejection", vaultOpens)
	}
	if sourceAfter, err := os.ReadFile(filepath.Join(source, "history.db")); err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatalf("rejected retry changed plaintext source: %v", err)
	}
	if destinationAfter, err := os.ReadFile(sentinelPath); err != nil || !bytes.Equal(destinationBefore, destinationAfter) {
		t.Fatalf("rejected retry changed existing encrypted vault: %v", err)
	}
}

func TestBootstrapRejectsMissingInnerMarkerWhenOuterMigrationIntentExists(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "encrypted-root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	partial, err := app.Open(mount, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(mount, ".whatsfeather-migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial fixture unexpectedly has an inner migration marker: %v", err)
	}

	var stages []string
	vaultCloseCalls := 0
	deps := testBootstrapDependencies()
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		stages = append(stages, "vault")
		return &fakeVault{mountPath: mount, mountID: 43, migrationStarted: true, closeFunc: func(context.Context) error {
			vaultCloseCalls++
			return nil
		}}, nil
	}
	deps.openApp = func(string, bool) (*app.App, error) {
		stages = append(stages, "app")
		return nil, errors.New("partial profile must not open")
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		stages = append(stages, "adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		stages = append(stages, "listen")
		return inertRPC{}, nil
	}

	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if runtime != nil {
		_ = runtime.Close(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("bootstrap error = %v, want missing-inner-marker migration rejection", err)
	}
	if len(stages) != 1 || stages[0] != "vault" || vaultCloseCalls != 1 {
		t.Fatalf("markerless partial profile reached stages %v and closed vault %d times; want vault only and one close", stages, vaultCloseCalls)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("markerless partial profile started an RPC socket: %v", err)
	}
}

func TestBootstrapRejectsIncompleteMigrationBeforeOpeningApplication(t *testing.T) {
	base := shortBootstrapTestDir(t)
	root := filepath.Join(base, "encrypted-root")
	runtimeDir := filepath.Join(base, "runtime")
	mount := filepath.Join(runtimeDir, "mount")
	for _, dir := range []string{root, runtimeDir, mount} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	state := map[string]any{
		"format":               "whatsfeather-profile-migration",
		"version":              1,
		"state":                "copying",
		"source_identity":      strings.Repeat("0", 64),
		"destination_identity": profileKeyringIdentity(root),
		"source_device":        uint64(1),
		"source_inode":         uint64(2),
	}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, ".whatsfeather-migration.json"), stateBytes, 0600); err != nil {
		t.Fatal(err)
	}

	var stages []string
	deps := testBootstrapDependencies()
	vaultCloseCalls := 0
	deps.openVault = func(context.Context, profilevault.Options) (vaultLifecycle, error) {
		stages = append(stages, "vault")
		return &fakeVault{mountPath: mount, mountID: 42, closeFunc: func(context.Context) error {
			vaultCloseCalls++
			return nil
		}}, nil
	}
	deps.openApp = func(string, bool) (*app.App, error) {
		stages = append(stages, "app")
		return nil, errors.New("app must not open for an incomplete migration")
	}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport {
		stages = append(stages, "adapter")
		return nil
	}
	deps.listen = func(string, *app.App) (rpcRunner, error) {
		stages = append(stages, "listen")
		return inertRPC{}, nil
	}

	runtime, err := bootstrap(context.Background(), bootstrapOptions{
		dataDir: root, runtimeDir: runtimeDir, socketPath: filepath.Join(runtimeDir, "daemon.sock"), live: true,
	}, deps)
	if runtime != nil {
		_ = runtime.Close(context.Background())
	}
	if err == nil || !strings.Contains(err.Error(), "migration") {
		t.Fatalf("bootstrap error = %v, want fail-closed migration-state rejection", err)
	}
	if len(stages) != 1 || stages[0] != "vault" || vaultCloseCalls != 1 {
		t.Fatalf("incomplete migration reached stages %v and closed vault %d times; want vault only and one close", stages, vaultCloseCalls)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, "daemon.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial migration started an RPC socket: %v", err)
	}
}

func writeMigrationSourceLock(t *testing.T, source string) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(source, "profile.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("create synthetic legacy daemon owner lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close synthetic legacy daemon owner lock: %v", err)
	}
}
