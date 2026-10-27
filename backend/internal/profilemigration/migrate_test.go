package profilemigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
)

func TestBoundedSourceTotalHasSpecificLimitErrors(t *testing.T) {
	if total, err := boundedSourceTotal(0, 8, 8, 10); err != nil || total != 8 {
		t.Fatalf("per-file and aggregate boundary should be inclusive: total=%d err=%v", total, err)
	}
	if _, err := boundedSourceTotal(0, 9, 8, 10); !errors.Is(err, ErrSourceFileTooLarge) {
		t.Fatalf("per-file cap error = %v, want descriptive file-size error", err)
	}
	if _, err := boundedSourceTotal(8, 3, 8, 10); !errors.Is(err, ErrSourceAggregateLimit) {
		t.Fatalf("aggregate cap error = %v, want descriptive profile-size error", err)
	}
}

func TestPrepareRequiresExplicitMarkedLiveModeWithoutSourceMutation(t *testing.T) {
	for _, fixtureMode := range []string{"unmarked", "demo"} {
		t.Run(fixtureMode, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "legacy-source")
			destination := filepath.Join(base, "new-encrypted-destination")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := app.Open(source, fixtureMode == "demo")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			createLegacyOwnerLock(t, source)
			if fixtureMode == "unmarked" {
				db, err := sql.Open("sqlite", filepath.Join(source, "history.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("DELETE FROM meta WHERE key='mode'"); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotMigrationTree(t, source)
			if _, err := Prepare(source, destination); !errors.Is(err, ErrWrongSourceMode) {
				t.Fatalf("Prepare for %s source error = %v, want explicit live-mode rejection", fixtureMode, err)
			}
			if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(before, after) {
				t.Fatal("rejecting non-live source mutated source files")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejecting non-live source created destination: %v", err)
			}
		})
	}
}

func TestPrepareRequiresExistingCooperativeSourceOwnerLock(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-destination")
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
	if _, err := os.Lstat(filepath.Join(source, "profile.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has a cooperative owner lock: %v", err)
	}

	plan, err := Prepare(source, destination)
	if plan != nil {
		_ = plan.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "cooperative profile owner lock") {
		t.Fatalf("Prepare without the daemon's existing cooperative owner lock = %v, want fail-closed lock-required error", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock-required rejection created destination root: %v", err)
	}
}

func TestPrepareRejectsOverlappingPathsAndUnsafeSourceEntries(t *testing.T) {
	for _, attack := range []string{"overlap", "source-symlink", "media-symlink", "hardlink", "special", "oversize"} {
		t.Run(attack, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "legacy-live")
			destination := filepath.Join(base, "encrypted-destination")
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
			createLegacyOwnerLock(t, source)
			prepareSource := source
			prepareDestination := destination
			if attack == "overlap" {
				prepareDestination = filepath.Join(source, "nested-destination")
			}
			if attack == "source-symlink" {
				prepareSource = filepath.Join(base, "legacy-source-alias")
				if err := os.Symlink(source, prepareSource); err != nil {
					t.Fatal(err)
				}
			}
			if attack != "source-symlink" && attack != "overlap" {
				mediaDir := filepath.Join(source, "media")
				if err := os.Mkdir(mediaDir, 0700); err != nil {
					t.Fatal(err)
				}
				switch attack {
				case "media-symlink":
					outside := filepath.Join(base, "outside.bin")
					if err := os.WriteFile(outside, []byte("synthetic outside"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, filepath.Join(mediaDir, "escape")); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(filepath.Join(source, "history.db"), filepath.Join(mediaDir, "aliased.db")); err != nil {
						t.Fatal(err)
					}
				case "special":
					if err := syscall.Mkfifo(filepath.Join(mediaDir, "fifo"), 0600); err != nil {
						t.Fatal(err)
					}
				case "oversize":
					file, err := os.OpenFile(filepath.Join(mediaDir, "too-large"), os.O_CREATE|os.O_RDWR, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Truncate(MaxFileBytes + 1); err != nil {
						_ = file.Close()
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := snapshotMigrationTree(t, source)
			_, err = Prepare(prepareSource, prepareDestination)
			if attack == "oversize" && !errors.Is(err, ErrSourceFileTooLarge) {
				t.Fatalf("oversized file rejection = %v, want clear per-file limit error", err)
			}
			if err == nil {
				t.Fatalf("Prepare accepted %s attack", attack)
			}
			if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejecting %s attack mutated source files", attack)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejecting %s attack created destination root: %v", attack, statErr)
			}
		})
	}
}

func TestPrepareRejectsProcessWithOpenSourceFile(t *testing.T) {
	if os.Getenv("WF_PROFILE_MIGRATION_OPEN_CHILD") == "1" {
		file, err := os.Open(filepath.Join(os.Getenv("WF_PROFILE_MIGRATION_OPEN_SOURCE"), "history.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := os.WriteFile(os.Getenv("WF_PROFILE_MIGRATION_OPEN_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "encrypted-destination")
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
	createLegacyOwnerLock(t, source)
	ready := filepath.Join(base, "open-source.ready")
	command := exec.Command(os.Args[0], "-test.run=^TestPrepareRejectsProcessWithOpenSourceFile$")
	command.Env = append(os.Environ(),
		"WF_PROFILE_MIGRATION_OPEN_CHILD=1",
		"WF_PROFILE_MIGRATION_OPEN_SOURCE="+source,
		"WF_PROFILE_MIGRATION_OPEN_READY="+ready,
	)
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
		t.Fatalf("open-source process did not publish readiness: %v", err)
	}
	plan, err := Prepare(source, destination)
	if plan != nil {
		defer plan.Close()
	}
	if !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Prepare with another process holding a source DB handle = %v, want busy rejection", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active-process rejection created the destination root: %v", err)
	}
}

func TestPrepareRejectsActiveOwnerAndSQLiteSidecarsBeforeDestinationCreation(t *testing.T) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "legacy-live")
			destination := filepath.Join(base, "encrypted-destination")
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
			createLegacyOwnerLock(t, source)
			if suffix != "" {
				if err := os.WriteFile(filepath.Join(source, "history.db"+suffix), []byte("synthetic sqlite sidecar"), 0600); err != nil {
					t.Fatal(err)
				}
				before := snapshotMigrationTree(t, source)
				if _, err := Prepare(source, destination); !errors.Is(err, ErrSQLiteSidecars) {
					t.Fatalf("Prepare with %q sidecar error = %v, want explicit unsupported-sidecar rejection", suffix, err)
				}
				if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(before, after) {
					t.Fatal("rejecting an unsupported SQLite sidecar mutated the source")
				}
			} else {
				lockPath := filepath.Join(source, "profile.lock")
				fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					_ = syscall.Close(fd)
					t.Fatal(err)
				}
				if _, err := Prepare(source, destination); !errors.Is(err, ErrSourceBusy) {
					t.Fatalf("Prepare with active profile owner error = %v, want busy rejection", err)
				}
				if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("active-owner rejection created destination root: %v", err)
				}
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = syscall.Close(fd)
			}
		})
	}
}

func TestMigrationRechecksWholeSourceBeforePublishingCopyState(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	for _, dir := range []string{source, mount} {
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
	createLegacyOwnerLock(t, source)
	localDir := filepath.Join(source, "local-expressions")
	if err := os.Mkdir(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	changedPath := filepath.Join(localDir, "preferences.json")
	if err := os.WriteFile(changedPath, []byte("source-before"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if err := os.WriteFile(changedPath, []byte("source-after!"), 0600); err != nil {
		t.Fatal(err)
	}
	changedBytes, err := os.ReadFile(changedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(changedPath, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := plan.Migrate(context.Background(), mount, Options{}); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("Migrate after source mutation error = %v, want stable-source rejection", err)
	}
	if entries, err := os.ReadDir(mount); err != nil || len(entries) != 0 {
		t.Fatalf("source instability should be detected before state or copied files: count=%d err=%v", len(entries), err)
	}
	if after, err := os.ReadFile(changedPath); err != nil || !bytes.Equal(changedBytes, after) {
		t.Fatalf("migration changed source after rejecting source instability: err=%v", err)
	}
}

func TestMigrationRechecksSourceAfterFinalPreCompleteCallback(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	for _, dir := range []string{source, mount} {
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
	createLegacyOwnerLock(t, source)

	plan, err := Prepare(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	lateWrite := false
	err = plan.Migrate(context.Background(), mount, Options{Failpoint: func(point MigrationPoint) error {
		if point != PointBeforeComplete {
			return nil
		}
		db, err := sql.Open("sqlite", filepath.Join(source, "history.db"))
		if err != nil {
			return err
		}
		_, err = db.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('synthetic-late-writer','preserve-me')")
		closeErr := db.Close()
		lateWrite = err == nil && closeErr == nil
		if err != nil {
			return err
		}
		return closeErr
	}})
	if !lateWrite {
		t.Fatal("uncooperative same-UID writer did not mutate the source at the final publication seam")
	}
	if !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("migration after a post-rescan source write = %v, want stale-source rejection", err)
	}
	if stateErr := CheckStartupState(mount, destination); !errors.Is(stateErr, ErrIncompleteMigration) {
		t.Fatalf("stale source migration startup state = %v, want incomplete and inactive", stateErr)
	}
}

func TestMigrationCancellationDuringLargeFileLeavesIncompleteState(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	for _, dir := range []string{source, mount, filepath.Join(source, "media")} {
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
	createLegacyOwnerLock(t, source)
	large, err := os.OpenFile(filepath.Join(source, "media", "large-synthetic.bin"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(64 << 20); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	historyCopied := false
	largeFileChunks := 0
	err = plan.Migrate(ctx, mount, Options{Failpoint: func(point MigrationPoint) error {
		if point == PointFileCopied && !historyCopied {
			historyCopied = true
		}
		if point == MigrationPoint("file-copy-chunk") && historyCopied {
			largeFileChunks++
			if largeFileChunks == 2 {
				cancel()
			}
		}
		return nil
	}})
	if largeFileChunks < 2 {
		t.Fatalf("migration did not observe cancellation within the large file; chunk callbacks=%d err=%v", largeFileChunks, err)
	}
	if !errors.Is(err, ErrMigrationInterrupted) {
		t.Fatalf("migration canceled during large-file copy = %v, want interrupted result", err)
	}
	if stateErr := CheckStartupState(mount, destination); !errors.Is(stateErr, ErrIncompleteMigration) {
		t.Fatalf("canceled large-file migration state = %v, want inactive incomplete state", stateErr)
	}
}

func TestMigrationCancellationAfterLastFileDoesNotPublishComplete(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "new-encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	for _, dir := range []string{source, mount} {
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
	createLegacyOwnerLock(t, source)
	plan, err := Prepare(source, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = plan.Migrate(ctx, mount, Options{Failpoint: func(point MigrationPoint) error {
		if point == PointFileCopied {
			cancel()
		}
		return nil
	}})
	if !errors.Is(err, ErrMigrationInterrupted) {
		t.Fatalf("cancellation at the last copied file = %v, want interrupted result", err)
	}
	if stateErr := CheckStartupState(mount, destination); !errors.Is(stateErr, ErrIncompleteMigration) {
		t.Fatalf("canceled last-file migration state = %v, want inactive incomplete state", stateErr)
	}
}

func TestInterruptedMigrationRetriesToFreshDestinationWithoutDeletingSourceOrOverwritingVault(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination1 := filepath.Join(base, "encrypted-destination-1")
	destination2 := filepath.Join(base, "encrypted-destination-2")
	mount1 := filepath.Join(base, "private-mount-1")
	mount2 := filepath.Join(base, "private-mount-2")
	for _, dir := range []string{source, mount1, mount2} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), app.Message{
		ID: "synthetic-retry", ChatID: "synthetic:retry", Body: "keep plaintext source", Timestamp: 1770000000000, Status: "received",
	}, "Synthetic contact"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	createLegacyOwnerLock(t, source)
	sourceBefore := snapshotMigrationTree(t, source)

	plan1, err := Prepare(source, destination1)
	if err != nil {
		t.Fatal(err)
	}
	interruptErr := errors.New("synthetic interruption after first copied file")
	migrationErr := plan1.Migrate(context.Background(), mount1, Options{Failpoint: func(point MigrationPoint) error {
		if point == PointFileCopied {
			return interruptErr
		}
		return nil
	}})
	closeErr := plan1.Close()
	if !errors.Is(migrationErr, ErrMigrationInterrupted) || closeErr != nil {
		t.Fatalf("first-destination interrupted migration = %v, plan close = %v", migrationErr, closeErr)
	}
	if stateErr := CheckStartupState(mount1, destination1); !errors.Is(stateErr, ErrIncompleteMigration) {
		t.Fatalf("interrupted first destination startup state = %v, want inactive/incomplete", stateErr)
	}
	incomplete1 := snapshotMigrationTree(t, mount1)

	plan2, err := Prepare(source, destination2)
	if err != nil {
		t.Fatalf("fresh-destination retry preflight: %v", err)
	}
	if err := plan2.Migrate(context.Background(), mount2, Options{}); err != nil {
		_ = plan2.Close()
		t.Fatalf("fresh-destination retry: %v", err)
	}
	if err := plan2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckStartupState(mount2, destination2); err != nil {
		t.Fatalf("fresh retry did not complete destination 2: %v", err)
	}
	if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(sourceBefore, after) {
		t.Fatal("fresh-destination retry changed or deleted the plaintext source")
	}
	if after := snapshotMigrationTree(t, mount1); !reflect.DeepEqual(incomplete1, after) {
		t.Fatal("retry modified the interrupted first encrypted destination")
	}

	// Model the existing non-empty encrypted vault root. The retry path must
	// reject it before opening or changing that vault.
	if err := os.Mkdir(destination2, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination2, "cipher"), []byte("synthetic-existing-ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	fullVaultBefore := snapshotMigrationTree(t, destination2)
	completeMountBefore := snapshotMigrationTree(t, mount2)
	if plan, err := Prepare(source, destination2); plan != nil || !errors.Is(err, ErrDestinationNotEmpty) {
		if plan != nil {
			_ = plan.Close()
		}
		t.Fatalf("retry targeting the completed destination = (%v,%v), want non-empty-destination refusal", plan, err)
	}
	if after := snapshotMigrationTree(t, destination2); !reflect.DeepEqual(fullVaultBefore, after) {
		t.Fatal("refused retry changed the existing encrypted vault root")
	}
	if after := snapshotMigrationTree(t, mount2); !reflect.DeepEqual(completeMountBefore, after) {
		t.Fatal("refused retry changed the completed encrypted profile")
	}
	if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(sourceBefore, after) {
		t.Fatal("refused retry changed the plaintext source")
	}
}

func TestMigrationKillLeavesDurableIncompleteState(t *testing.T) {
	if os.Getenv("WF_PROFILE_MIGRATION_KILL_CHILD") == "1" {
		source := os.Getenv("WF_PROFILE_MIGRATION_SOURCE")
		destination := os.Getenv("WF_PROFILE_MIGRATION_DESTINATION")
		mount := os.Getenv("WF_PROFILE_MIGRATION_MOUNT")
		ready := os.Getenv("WF_PROFILE_MIGRATION_READY")
		wanted := MigrationPoint(os.Getenv("WF_PROFILE_MIGRATION_POINT"))
		plan, err := Prepare(source, destination)
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Close()
		_ = plan.Migrate(context.Background(), mount, Options{Failpoint: func(point MigrationPoint) error {
			if point == wanted {
				if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
					return err
				}
				for {
					time.Sleep(time.Hour)
				}
			}
			return nil
		}})
		t.Fatal("child migration returned without reaching its kill boundary")
	}

	for _, point := range []MigrationPoint{PointIncompletePublished, PointFileCopied, PointBeforeComplete, PointCompletePublished} {
		t.Run(string(point), func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "legacy-live")
			destination := filepath.Join(base, "ciphertext")
			mount := filepath.Join(base, "view")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
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
			createLegacyOwnerLock(t, source)
			ready := filepath.Join(base, "child.ready")
			command := exec.Command(os.Args[0], "-test.run=^TestMigrationKillLeavesDurableIncompleteState$")
			command.Env = append(os.Environ(),
				"WF_PROFILE_MIGRATION_KILL_CHILD=1",
				"WF_PROFILE_MIGRATION_SOURCE="+source,
				"WF_PROFILE_MIGRATION_DESTINATION="+destination,
				"WF_PROFILE_MIGRATION_MOUNT="+mount,
				"WF_PROFILE_MIGRATION_READY="+ready,
				"WF_PROFILE_MIGRATION_POINT="+string(point),
			)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(ready); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("migration child did not reach the requested kill boundary: %v", err)
			}
			if err := command.Process.Kill(); err != nil {
				_ = command.Wait()
				t.Fatalf("send SIGKILL at migration boundary: %v", err)
			}
			if err := command.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
				t.Fatalf("migration child exit = %v, want SIGKILL", err)
			}
			stateErr := CheckStartupState(mount, destination)
			if point == PointCompletePublished {
				if stateErr != nil {
					t.Fatalf("ordinary startup state check after complete-state SIGKILL = %v, want completed state acceptance", stateErr)
				}
				store, err := app.Open(mount, false)
				if err != nil {
					t.Fatalf("ordinary app open after durable complete-state SIGKILL: %v", err)
				}
				if err := store.Close(); err != nil {
					t.Fatalf("close app after complete-state SIGKILL: %v", err)
				}
			} else if !errors.Is(stateErr, ErrIncompleteMigration) {
				t.Fatalf("ordinary startup state check after partial SIGKILL = %v, want incomplete migration rejection", stateErr)
			} else if point == PointFileCopied {
				if _, err := os.Stat(filepath.Join(mount, "history.db")); err != nil {
					t.Fatalf("mid-copy kill boundary did not leave a copied partial database: %v", err)
				}
			}
		})
	}
}

func TestPersistedMediaMetadataValidationEnforcesAppProjectionBounds(t *testing.T) {
	if err := validateAttachmentMetadata([]byte(`{}`)); err != nil {
		t.Fatalf("valid legacy-compatible attachment object rejected: %v", err)
	}
	for name, raw := range map[string][]byte{
		"bad-json":         []byte(`not-json`),
		"wrong-field-type": []byte(`{"size":"1"}`),
		"negative-size":    []byte(`{"size":-1}`),
	} {
		t.Run("attachment/"+name, func(t *testing.T) {
			if err := validateAttachmentMetadata(raw); !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("attachment metadata validation = %v, want unsupported source", err)
			}
		})
	}
	oversizedAttachment, err := json.Marshal(app.Attachment{Name: strings.Repeat("x", maxMediaMetadataBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAttachmentMetadata(oversizedAttachment); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("oversized attachment metadata validation = %v, want bounded rejection", err)
	}

	validItem := app.Attachment{Kind: "image", Mode: "photo", MIME: "image/png", State: "ready", Size: 1}
	validAlbum, err := json.Marshal([]app.Attachment{validItem, validItem})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAlbumMetadata(validAlbum); err != nil {
		t.Fatalf("valid app album projection rejected: %v", err)
	}
	invalidAlbums := map[string][]app.Attachment{
		"too-few-items": {validItem},
		"invalid-state": {validItem, {Kind: "image", Mode: "photo", MIME: "image/png", State: "remote", Size: 1}},
		"wrong-kind":    {validItem, {Kind: "document", Mode: "photo", MIME: "image/png", State: "ready", Size: 1}},
		"negative-size": {validItem, {Kind: "image", Mode: "photo", MIME: "image/png", State: "ready", Size: -1}},
	}
	for name, items := range invalidAlbums {
		t.Run("album/"+name, func(t *testing.T) {
			raw, err := json.Marshal(items)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateAlbumMetadata(raw); !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("album metadata validation = %v, want unsupported source", err)
			}
		})
	}
	oversizedAlbum, err := json.Marshal([]app.Attachment{
		{Kind: "image", Mode: "photo", MIME: "image/png", State: "ready", Size: 1, Attribution: strings.Repeat("x", maxMediaMetadataBytes)},
		validItem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAlbumMetadata(oversizedAlbum); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("oversized album metadata validation = %v, want bounded rejection", err)
	}
}

func TestMigrationRejectsMalformedPersistedMediaMetadataBeforeComplete(t *testing.T) {
	for _, table := range []string{"attachments", "message_albums"} {
		t.Run(table, func(t *testing.T) {
			base := t.TempDir()
			source := filepath.Join(base, "legacy-live")
			destination := filepath.Join(base, "new-encrypted-profile")
			mount := filepath.Join(base, "private-mount")
			for _, dir := range []string{source, mount} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			store, err := app.Open(source, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Record(context.Background(), app.Message{
				ID: "synthetic-damaged-media", ChatID: "synthetic:media", Body: "fixture", Timestamp: 1770000000000, Status: "received",
			}, "Synthetic contact"); err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			createLegacyOwnerLock(t, source)
			db, err := sql.Open("sqlite", filepath.Join(source, "history.db"))
			if err != nil {
				t.Fatal(err)
			}
			if table == "attachments" {
				_, err = db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", "synthetic:media", "synthetic-damaged-media", "not-json")
			} else {
				_, err = db.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,?)", "synthetic:media", "synthetic-damaged-media", "not-json", "[]")
			}
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("seed malformed synthetic %s row: exec=%v close=%v", table, err, closeErr)
			}
			plan, err := Prepare(source, destination)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			err = plan.Migrate(context.Background(), mount, Options{})
			if !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("migration with malformed %s JSON = %v, want damaged-row rejection", table, err)
			}
			if stateErr := CheckStartupState(mount, destination); !errors.Is(stateErr, ErrIncompleteMigration) {
				t.Fatalf("malformed %s destination startup state = %v, want inactive incomplete state", table, stateErr)
			}
		})
	}
}

func TestMigrationCopiesMarkedLiveProfileAndRebasesOnlyValidatedOwnedMedia(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "legacy-live")
	destination := filepath.Join(base, "encrypted-profile")
	mount := filepath.Join(base, "private-mount")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}

	store, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []app.Message{
		{ID: "legacy-owned", ChatID: "synthetic:chat", Body: "synthetic legacy history", Timestamp: 1770000000000, Status: "received", Kind: "document"},
		{ID: "legacy-foreign", ChatID: "synthetic:chat", Body: "foreign media must remain rejected", Timestamp: 1770000001000, Status: "received", Kind: "document"},
	} {
		if err := store.Record(context.Background(), message, "Synthetic contact"); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	mediaDir := filepath.Join(source, "media")
	if err := os.Mkdir(mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	var mediaBuffer bytes.Buffer
	mediaImage := image.NewRGBA(image.Rect(0, 0, 2, 2))
	mediaImage.SetRGBA(0, 0, color.RGBA{R: 220, G: 10, B: 30, A: 255})
	if err := png.Encode(&mediaBuffer, mediaImage); err != nil {
		t.Fatal(err)
	}
	mediaBytes := mediaBuffer.Bytes()
	digestBytes := sha256.Sum256(mediaBytes)
	digest := hex.EncodeToString(digestBytes[:])
	mediaName := digest + ".png"
	mediaPath := filepath.Join(mediaDir, mediaName)
	if err := os.WriteFile(mediaPath, mediaBytes, 0600); err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(source, "local-expressions", "sticker-packs", "synthetic-pack")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	preferences := []byte(`{"version":1,"recentEmojis":["🧪"],"favoriteEmojis":["🛡️"],"stickerPacks":[]}`)
	if err := os.WriteFile(filepath.Join(source, "local-expressions", "preferences.json"), preferences, 0600); err != nil {
		t.Fatal(err)
	}
	stickerBytes := []byte("synthetic local sticker pack bytes")
	if err := os.WriteFile(filepath.Join(localDir, "fixture.webp"), stickerBytes, 0600); err != nil {
		t.Fatal(err)
	}
	devicePath := filepath.Join(source, "device.db")
	deviceDB, err := sql.Open("sqlite", devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deviceDB.Exec("CREATE TABLE fake_device(marker TEXT); INSERT INTO fake_device VALUES('synthetic-only-device-row')"); err != nil {
		_ = deviceDB.Close()
		t.Fatal(err)
	}
	if err := deviceDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(devicePath, 0600); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(source, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	owned := app.Attachment{
		Name: "legacy.png", Size: int64(len(mediaBytes)), MIME: "image/png", Kind: "image", Mode: "photo",
		SHA256: digest, State: "ready", URL: (&url.URL{Scheme: "file", Path: mediaPath}).String(),
	}
	foreign := owned
	foreign.URL = (&url.URL{Scheme: "file", Path: filepath.Join(base, "foreign", mediaName)}).String()
	ownedJSON, err := json.Marshal(owned)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	foreignJSON, err := json.Marshal(foreign)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", "synthetic:chat", "legacy-owned", string(ownedJSON)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	album, err := json.Marshal([]app.Attachment{owned, foreign})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,?)", "synthetic:chat", "legacy-owned", string(album), `[{"id":"original-client-token"}]`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", "synthetic:chat", "legacy-foreign", string(foreignJSON)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	createLegacyOwnerLock(t, source)
	before := snapshotMigrationTree(t, source)

	plan, err := Prepare(source, destination)
	if err != nil {
		t.Fatalf("prepare marked-live synthetic profile: %v", err)
	}
	if err := plan.Migrate(context.Background(), mount, Options{}); err != nil {
		_ = plan.Close()
		t.Fatalf("offline copy into distinct encrypted view: %v", err)
	}
	if err := plan.Close(); err != nil {
		t.Fatalf("release offline source ownership: %v", err)
	}
	if after := snapshotMigrationTree(t, source); !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed legacy source bytes or permissions")
	}
	if err := CheckStartupState(mount, destination); err != nil {
		t.Fatalf("completed migration is not accepted for ordinary startup: %v", err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(mount, stateFilename))
	if err != nil || strings.Contains(string(stateBytes), source) || strings.Contains(string(stateBytes), "SYNTHETIC-LEGACY-MEDIA") {
		t.Fatalf("migration state exposed source path/content or could not be read: err=%v", err)
	}
	if copied, err := os.ReadFile(filepath.Join(mount, "local-expressions", "preferences.json")); err != nil || !reflect.DeepEqual(copied, preferences) {
		t.Fatalf("profile-local expression preferences were not copied exactly: bytes=%d err=%v", len(copied), err)
	}
	if copied, err := os.ReadFile(filepath.Join(mount, "local-expressions", "sticker-packs", "synthetic-pack", "fixture.webp")); err != nil || !reflect.DeepEqual(copied, stickerBytes) {
		t.Fatalf("local sticker-pack asset was not copied exactly: bytes=%d err=%v", len(copied), err)
	}
	copiedDevice, err := sql.Open("sqlite", filepath.Join(mount, "device.db"))
	if err != nil {
		t.Fatal(err)
	}
	var deviceMarker string
	if err := copiedDevice.QueryRow("SELECT marker FROM fake_device").Scan(&deviceMarker); err != nil || deviceMarker != "synthetic-only-device-row" {
		_ = copiedDevice.Close()
		t.Fatalf("synthetic device store did not survive copy: marker=%q err=%v", deviceMarker, err)
	}
	if err := copiedDevice.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := app.Open(mount, false)
	if err != nil {
		t.Fatalf("open copied live profile: %v", err)
	}
	defer migrated.Close()
	result, err := migrated.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic:chat","limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	resultBytes, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err := json.Unmarshal(resultBytes, &page); err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		switch message.ID {
		case "legacy-owned":
			if message.Attachment == nil || message.Attachment.URL == "" || !strings.HasPrefix(message.Attachment.URL, (&url.URL{Scheme: "file", Path: filepath.Join(mount, "media")}).String()) {
				t.Fatalf("owned attachment did not rebase into new profile: %#v", message.Attachment)
			}
			parsed, err := url.Parse(message.Attachment.URL)
			if err != nil || parsed.Path != filepath.Join(mount, "media", mediaName) {
				t.Fatalf("rebased attachment URL does not resolve to verified cache file: %q (%v)", message.Attachment.URL, err)
			}
			if data, err := os.ReadFile(parsed.Path); err != nil || !reflect.DeepEqual(data, mediaBytes) {
				t.Fatalf("rebased attachment bytes mismatch: bytes=%d err=%v", len(data), err)
			}
			if message.Album == nil || len(*message.Album) != 2 || (*message.Album)[0].URL == "" || (*message.Album)[1].URL != "" || (*message.Album)[1].State != "remote" {
				t.Fatalf("album rebase did not preserve owned-vs-foreign distinctions: %#v", message.Album)
			}
		case "legacy-foreign":
			if message.Attachment == nil || message.Attachment.URL != "" || message.Attachment.State != "remote" {
				t.Fatalf("foreign legacy media reference was rebased or exposed: %#v", message.Attachment)
			}
		}
	}
	var sourceTokens string
	if err := migratedDBQuery(mount, "SELECT source_tokens FROM message_albums WHERE chat_id=? AND message_id=?", &sourceTokens, "synthetic:chat", "legacy-owned"); err != nil || sourceTokens != `[{"id":"original-client-token"}]` {
		t.Fatalf("migration invalidated album idempotency/source tokens: %q err=%v", sourceTokens, err)
	}
}

func createLegacyOwnerLock(t *testing.T, root string) {
	t.Helper()
	lockPath := filepath.Join(root, "profile.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("create private legacy owner lock: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close private legacy owner lock: %v", err)
	}
}

func snapshotMigrationTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			result[rel] = info.Mode().String()
			return nil
		}
		if !info.Mode().IsRegular() {
			result[rel] = info.Mode().String()
			return nil
		}
		if info.Size() > MaxFileBytes {
			result[rel] = info.Mode().String() + ":oversize:" + info.ModTime().String()
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		result[rel] = info.Mode().String() + ":" + hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func migratedDBQuery(root, query string, destination any, args ...any) error {
	db, err := sql.Open("sqlite", filepath.Join(root, "history.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRow(query, args...).Scan(destination)
}
