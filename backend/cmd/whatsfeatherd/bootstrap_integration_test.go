package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/profilevault"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
)

func TestEncryptedDaemonRPCMigratedLegacyProfileSurvivesFUSECloseAndReopen(t *testing.T) {
	acceptanceRequest := os.Getenv("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE")
	if acceptanceRequest != "" && acceptanceRequest != "1" {
		t.Fatalf("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE must be 1 when set")
	}
	if acceptanceRequest != "1" {
		t.Skip("real native/FUSE migration acceptance is opt-in; run scripts/verify-encrypted-profile.sh")
	}
	engine := integrationGocryptfs(t)
	nativeTestBin := os.Getenv("WHATSFEATHER_NATIVE_PROFILE_TEST_BIN")
	if nativeTestBin == "" {
		t.Fatal("real native/FUSE migration acceptance requires WHATSFEATHER_NATIVE_PROFILE_TEST_BIN")
	}
	if err := validateIntegrationExecutable(nativeTestBin); err != nil {
		t.Fatalf("explicit WHATSFEATHER_NATIVE_PROFILE_TEST_BIN is invalid: %v", err)
	}
	unmount := os.Getenv("WHATSFEATHER_TEST_FUSERMOUNT3")
	if unmount == "" {
		unmount = "/usr/sbin/fusermount3"
	}
	if err := validateIntegrationExecutable(unmount); err != nil {
		t.Fatalf("normal FUSE unmount helper is unavailable or invalid: %v", err)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("real native/FUSE migration acceptance requested but /dev/fuse is unavailable: %v", err)
	}

	base := integrationTempDir(t)
	root := filepath.Join(base, "encrypted-destination")
	runtimeDir := filepath.Join(base, "runtime")
	source := filepath.Join(base, "synthetic-legacy-live")
	decoyRoot := filepath.Join(base, "decoy")
	for _, dir := range []string{root, runtimeDir, source, decoyRoot, filepath.Join(decoyRoot, "cipher")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(runtimeDir, "daemon.sock")
	keyring := &integrationMemoryKeyring{}
	t.Cleanup(keyring.wipe)

	historyMarker := "wf-synthetic-legacy-migration-history-4e91b"
	deviceMarker := "wf-synthetic-legacy-migration-device-7c20f"
	mediaBytes, err := os.ReadFile(integrationStickerFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	mediaDigest := sha256.Sum256(mediaBytes)
	mediaHash := hex.EncodeToString(mediaDigest[:])
	mediaName := mediaHash + ".webp"
	mediaDir := filepath.Join(source, "media")
	if err := os.Mkdir(mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyMediaPath := filepath.Join(mediaDir, mediaName)
	if err := os.WriteFile(legacyMediaPath, mediaBytes, 0600); err != nil {
		t.Fatal(err)
	}
	legacyAttachment := app.Attachment{
		Name: "legacy.webp", Size: int64(len(mediaBytes)), MIME: "image/webp", Kind: "image", Mode: "photo",
		SHA256: mediaHash, State: "ready", URL: (&url.URL{Scheme: "file", Path: legacyMediaPath}).String(),
	}
	legacyApp, err := app.Open(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyApp.Record(context.Background(), app.Message{
		ID: "legacy-migrated-1", ChatID: "synthetic-chat", Body: historyMarker,
		Timestamp: 1770000000000, Status: "received", Kind: "image", Attachment: &legacyAttachment,
	}, "Synthetic legacy contact"); err != nil {
		_ = legacyApp.Close()
		t.Fatal(err)
	}
	if err := legacyApp.Close(); err != nil {
		t.Fatal(err)
	}
	legacyDB, err := sql.Open("sqlite", filepath.Join(source, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	album, err := json.Marshal([]app.Attachment{legacyAttachment, legacyAttachment})
	if err == nil {
		_, err = legacyDB.Exec("INSERT INTO message_albums(chat_id,message_id,items,source_tokens) VALUES(?,?,?,?)", "synthetic-chat", "legacy-migrated-1", string(album), `[{"id":"legacy-client-token"}]`)
	}
	if closeErr := legacyDB.Close(); err != nil || closeErr != nil {
		t.Fatalf("seed synthetic legacy album: exec=%v close=%v", err, closeErr)
	}
	devicePath := filepath.Join(source, "device.db")
	deviceDB, err := sql.Open("sqlite", devicePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deviceDB.Exec("CREATE TABLE synthetic_device(marker TEXT); INSERT INTO synthetic_device VALUES(?)", deviceMarker); err != nil {
		_ = deviceDB.Close()
		t.Fatal(err)
	}
	if err := deviceDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(devicePath, 0600); err != nil {
		t.Fatal(err)
	}

	packID := "0123456789abcdef0123456789abcdef"
	stickerID := mediaHash
	stickerDir := filepath.Join(source, "local-expressions", "sticker-packs", packID)
	if err := os.MkdirAll(stickerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stickerDir, stickerID+".webp"), mediaBytes, 0600); err != nil {
		t.Fatal(err)
	}
	preferences, err := json.Marshal(map[string]any{
		"version": 1, "recent_emojis": []string{"🧪"}, "favorite_emojis": []string{"🛡️"},
		"sticker_packs": []any{map[string]any{
			"id": packID, "name": "Synthetic migrated pack",
			"stickers": []any{map[string]any{"id": stickerID, "name": "Migrated test sticker"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "local-expressions", "preferences.json"), preferences, 0600); err != nil {
		t.Fatal(err)
	}
	writeMigrationSourceLock(t, source)
	legacySourceBefore := snapshotSyntheticLegacyProfile(t, source)

	deps := productionBootstrapDependencies()
	deps.keyring = keyring
	deps.gocryptfsPath = engine
	deps.fuseUnmountPath = unmount
	if err := runWithArgs([]string{
		"--migrate-live-source", source,
		"--data-dir", root,
		"--runtime-dir", runtimeDir,
	}, deps); err != nil {
		t.Fatalf("explicit offline CLI migration into a real FUSE encrypted destination: %v", err)
	}
	if current := snapshotSyntheticLegacyProfile(t, source); !equalSyntheticSnapshots(legacySourceBefore, current) {
		t.Fatal("migration changed original source bytes or permissions")
	}
	assertCiphertextHasNoSyntheticPlaintext(t, root, historyMarker, deviceMarker, source,
		"Synthetic migrated pack", "Migrated test sticker", "SQLite format 3\x00")
	assertNoPlaintextProfileFiles(t, root)

	first, firstTransport, firstStop := startRealEncryptedTestDaemon(t, root, runtimeDir, socket, keyring, engine, unmount)
	firstMount := first.vault.MountPath()
	if _, err := os.Lstat(filepath.Join(firstMount, "device.db")); err != nil {
		t.Fatalf("migrated fake device database did not reopen under FUSE: %v", err)
	}
	if _, messageErr := first.app.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-chat","limit":10}`)); messageErr != nil {
		t.Fatalf("direct migrated app history projection failed before RPC: %v", messageErr)
	}
	firstMessages := callMessageList(t, socket)
	if len(firstMessages) != 1 || firstMessages[0].ID != "legacy-migrated-1" || firstMessages[0].Body != historyMarker {
		t.Fatalf("migrated RPC history projection = %#v", firstMessages)
	}
	firstMediaPath := pathFromFileURLForTest(t, attachmentURLForTest(t, firstMessages, "legacy-migrated-1"))
	if !strings.HasPrefix(firstMediaPath, filepath.Join(firstMount, "media")+string(filepath.Separator)) || !bytes.Equal(readRequiredTestFile(t, firstMediaPath), mediaBytes) {
		t.Fatalf("migrated legacy media did not resolve under first encrypted mount: %q", firstMediaPath)
	}
	runNativeEncryptedProfileController(t, nativeTestBin, "verify", socket, root,
		runtimeDir, firstMount, fmt.Sprint(first.vault.MountID()), decoyRoot, integrationStickerFixture(t))
	if firstTransport.connectCount() != 0 {
		t.Fatalf("migration acceptance connected the synthetic transport %d times", firstTransport.connectCount())
	}
	stopRealEncryptedTestDaemon(t, firstStop)
	if _, err := os.Lstat(firstMount); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal daemon close left the first migration mount: %v", err)
	}

	second, secondTransport, secondStop := startRealEncryptedTestDaemon(t, root, runtimeDir, socket, keyring, engine, unmount)
	secondMount := second.vault.MountPath()
	if firstMount == secondMount {
		t.Fatalf("reopened migrated profile reused stale mount path %q", secondMount)
	}
	secondMessages := callMessageList(t, socket)
	if len(secondMessages) != 1 || secondMessages[0].ID != "legacy-migrated-1" || secondMessages[0].Body != historyMarker {
		t.Fatalf("reopened migrated RPC history projection = %#v", secondMessages)
	}
	secondMediaPath := pathFromFileURLForTest(t, attachmentURLForTest(t, secondMessages, "legacy-migrated-1"))
	if !strings.HasPrefix(secondMediaPath, filepath.Join(secondMount, "media")+string(filepath.Separator)) || secondMediaPath == firstMediaPath || !bytes.Equal(readRequiredTestFile(t, secondMediaPath), mediaBytes) {
		t.Fatalf("reopened legacy media did not resolve under the new encrypted mount: %q", secondMediaPath)
	}
	runNativeEncryptedProfileController(t, nativeTestBin, "verify", socket, root,
		runtimeDir, secondMount, fmt.Sprint(second.vault.MountID()), decoyRoot, integrationStickerFixture(t))
	if secondTransport.connectCount() != 0 {
		t.Fatalf("reopened migration acceptance connected the synthetic transport %d times", secondTransport.connectCount())
	}
	stopRealEncryptedTestDaemon(t, secondStop)
	if current := snapshotSyntheticLegacyProfile(t, source); !equalSyntheticSnapshots(legacySourceBefore, current) {
		t.Fatal("encrypted close/reopen changed original legacy source bytes or permissions")
	}
	assertCiphertextHasNoSyntheticPlaintext(t, root, historyMarker, deviceMarker, source,
		"Synthetic migrated pack", "Migrated test sticker")
	assertNoPlaintextProfileFiles(t, root)
}

func TestEncryptedDaemonRPCMediaSurvivesFUSECloseAndReopen(t *testing.T) {
	if os.Getenv("WHATSFEATHER_HOT_JOURNAL_FUSE_CHILD") == "1" {
		runEncryptedFUSEHotJournalWriter(t)
		return
	}
	engine := os.Getenv("WHATSFEATHER_TEST_GCRYPTFS")
	if engine != "" {
		if err := validateIntegrationExecutable(engine); err != nil {
			t.Fatalf("explicit WHATSFEATHER_TEST_GCRYPTFS is invalid: %v", err)
		}
	}
	nativeTestBin := os.Getenv("WHATSFEATHER_NATIVE_PROFILE_TEST_BIN")
	if nativeTestBin != "" {
		if err := validateIntegrationExecutable(nativeTestBin); err != nil {
			t.Fatalf("explicit WHATSFEATHER_NATIVE_PROFILE_TEST_BIN is invalid: %v", err)
		}
	}
	unmount := os.Getenv("WHATSFEATHER_TEST_FUSERMOUNT3")
	if unmount != "" {
		if err := validateIntegrationExecutable(unmount); err != nil {
			t.Fatalf("explicit WHATSFEATHER_TEST_FUSERMOUNT3 is invalid: %v", err)
		}
	}
	acceptanceRequest := os.Getenv("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE")
	if acceptanceRequest != "" && acceptanceRequest != "1" {
		t.Fatalf("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE must be 1 when set")
	}
	if acceptanceRequest != "1" {
		t.Skip("real native/FUSE acceptance is opt-in; run scripts/verify-encrypted-profile.sh")
	}
	if engine == "" {
		t.Fatal("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE=1 requires explicit WHATSFEATHER_TEST_GCRYPTFS")
	}
	if nativeTestBin == "" {
		t.Fatal("WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE=1 requires WHATSFEATHER_NATIVE_PROFILE_TEST_BIN")
	}
	engine = integrationGocryptfs(t)
	if unmount == "" {
		unmount = "/usr/sbin/fusermount3"
	}
	if err := validateIntegrationExecutable(unmount); err != nil {
		t.Fatalf("normal FUSE unmount helper is unavailable or invalid: %v", err)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("real native/FUSE acceptance requested but /dev/fuse is unavailable: %v", err)
	}

	base := integrationTempDir(t)
	root := filepath.Join(base, "profile")
	runtimeDir := filepath.Join(base, "runtime")
	decoyRoot := filepath.Join(base, "decoy")
	if err := os.Mkdir(decoyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(decoyRoot, "cipher"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(runtimeDir, "daemon.sock")
	keyring := &integrationMemoryKeyring{}
	t.Cleanup(keyring.wipe)

	first, firstTransport, firstStop := startRealEncryptedTestDaemon(t, root, runtimeDir, socket, keyring, engine, unmount)
	firstMount := first.vault.MountPath()
	if firstMount == root || filepath.Dir(firstMount) != runtimeDir {
		t.Fatalf("live app mount path %q is not a runtime-owned encrypted view", firstMount)
	}
	mountInfo, mountInfoErr := os.ReadFile("/proc/self/mountinfo")
	if mountInfoErr != nil {
		t.Fatalf("read test mount table: %v", mountInfoErr)
	}
	if namespace, namespaceErr := os.Readlink("/proc/self/ns/mnt"); namespaceErr == nil {
		t.Logf("FUSE integration mount namespace: %s", namespace)
	}
	for _, line := range strings.Split(string(mountInfo), "\n") {
		if strings.Contains(line, " "+firstMount+" ") {
			t.Logf("real gocryptfs mountinfo: %s", line)
		}
	}
	if firstTransport.connectCount() != 0 {
		t.Fatalf("bootstrap connected transport %d times; expected zero", firstTransport.connectCount())
	}
	if _, err := os.Lstat(filepath.Join(firstMount, "device.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon created device database before explicit Connect: %v", err)
	}
	status, err := callTestRPC(socket, "app.status", map[string]any{})
	if err != nil {
		t.Fatalf("read live app status through RPC: %v", err)
	}
	var liveStatus struct {
		Mode        string `json:"mode"`
		StorageView struct {
			Version        int    `json:"version"`
			Mode           string `json:"mode"`
			CiphertextRoot string `json:"ciphertext_root"`
			RuntimeDir     string `json:"runtime_dir"`
			ProfileRoot    string `json:"profile_root"`
			ViewID         string `json:"view_id"`
			MountID        string `json:"mount_id"`
		} `json:"storage_view"`
	}
	if err := json.Unmarshal(status, &liveStatus); err != nil || liveStatus.Mode != "live" {
		t.Fatalf("encrypted daemon RPC mode=%q err=%v", liveStatus.Mode, err)
	}
	view := liveStatus.StorageView
	t.Logf("real encrypted view binding: mountID=%s profileRoot=%s runtimeDir=%s ciphertextRoot=%s", view.MountID, view.ProfileRoot, view.RuntimeDir, view.CiphertextRoot)
	if view.Version != 1 || view.Mode != "encrypted" || view.CiphertextRoot != root ||
		view.RuntimeDir != runtimeDir || view.ProfileRoot != firstMount ||
		len(view.ViewID) != 32 || view.MountID == "" {
		t.Fatalf("first encrypted storage view does not bind its ciphertext/runtime/mount: %+v", view)
	}

	ctx := context.Background()
	historyMarker := "wf-synthetic-history-plaintext-marker-7e31b80e"
	draftMarker := "wf-synthetic-rpc-draft-marker-3cb1a22d"
	mediaMarker := []byte("wf-synthetic-media-plaintext-marker-9d401ee7")
	if err := first.app.Record(ctx, app.Message{
		ID: "synthetic-history-1", ChatID: "synthetic-chat", Body: historyMarker,
		Timestamp: 1770000000000, Status: "received", Kind: "document",
		Attachment:   &app.Attachment{Name: "synthetic.txt", Size: int64(len(mediaMarker)), MIME: "text/plain", Kind: "document", State: "remote"},
		MediaPayload: string(mediaMarker),
	}, "Synthetic test conversation"); err != nil {
		stopRealEncryptedTestDaemon(t, firstStop)
		t.Fatalf("record synthetic history inside encrypted mount: %v", err)
	}
	runNativeEncryptedProfileController(t, nativeTestBin, "write", socket, root,
		runtimeDir, firstMount, view.MountID, decoyRoot, integrationStickerFixture(t))
	if result, callErr := callTestRPC(socket, "draft.set", map[string]any{"chat_id": "synthetic-chat", "text": draftMarker}); callErr != nil {
		stopRealEncryptedTestDaemon(t, firstStop)
		t.Fatalf("write synthetic draft through RPC: %v (reply=%s)", callErr, result)
	}
	if _, callErr := callTestRPC(socket, "message.download", map[string]string{"chat_id": "synthetic-chat", "id": "synthetic-history-1"}); callErr != nil {
		stopRealEncryptedTestDaemon(t, firstStop)
		t.Fatalf("download synthetic media through RPC: %v", callErr)
	}
	firstMessages := callMessageList(t, socket)
	if len(firstMessages) != 1 || firstMessages[0].Body != historyMarker {
		t.Fatalf("first RPC history projection=%#v", firstMessages)
	}
	firstURL := attachmentURLForTest(t, firstMessages, "synthetic-history-1")
	firstMediaPath := pathFromFileURLForTest(t, firstURL)
	if !strings.HasPrefix(firstMediaPath, filepath.Join(firstMount, "media")+string(filepath.Separator)) {
		t.Fatalf("first RPC projection points outside the mounted encrypted media tree: %q", firstMediaPath)
	}
	if got, err := os.ReadFile(firstMediaPath); err != nil || !bytes.Equal(got, mediaMarker) {
		t.Fatalf("first encrypted media copy mismatch: bytes=%d err=%v", len(got), err)
	}
	assertCiphertextHasNoSyntheticPlaintext(t, root, historyMarker, draftMarker,
		string(mediaMarker), "🧪", "🛡️", "Native FUSE fixture")
	assertNoPlaintextProfileFiles(t, root)
	if firstTransport.connectCount() != 0 {
		t.Fatalf("native Controller acceptance connected transport %d times; expected zero", firstTransport.connectCount())
	}
	stopRealEncryptedTestDaemon(t, firstStop)
	if _, err := os.Lstat(firstMount); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal close left the first owned mountpoint: %v", err)
	}

	second, secondTransport, secondStop := startRealEncryptedTestDaemon(t, root, runtimeDir, socket, keyring, engine, unmount)
	secondMount := second.vault.MountPath()
	if secondMount == firstMount {
		t.Fatalf("reopen reused projected mount path %q; expected a fresh decrypted view", secondMount)
	}
	if secondTransport.connectCount() != 0 {
		t.Fatalf("reopened bootstrap connected transport %d times; expected zero", secondTransport.connectCount())
	}
	secondStatusBytes, statusErr := callTestRPC(socket, "app.status", map[string]any{})
	if statusErr != nil {
		t.Fatalf("read reopened encrypted app status: %v", statusErr)
	}
	var secondStatus struct {
		StorageView struct {
			Mode        string `json:"mode"`
			ProfileRoot string `json:"profile_root"`
			ViewID      string `json:"view_id"`
			MountID     string `json:"mount_id"`
		} `json:"storage_view"`
	}
	if err := json.Unmarshal(secondStatusBytes, &secondStatus); err != nil ||
		secondStatus.StorageView.Mode != "encrypted" ||
		secondStatus.StorageView.ProfileRoot != secondMount ||
		secondStatus.StorageView.ViewID == view.ViewID ||
		secondStatus.StorageView.MountID == "" {
		t.Fatalf("reopened encrypted storage view retained stale scope: %+v err=%v", secondStatus.StorageView, err)
	}
	runNativeEncryptedProfileController(t, nativeTestBin, "verify", socket, root,
		runtimeDir, secondMount, secondStatus.StorageView.MountID, decoyRoot,
		integrationStickerFixture(t))
	secondMessages := callMessageList(t, socket)
	if len(secondMessages) != 1 || secondMessages[0].Body != historyMarker {
		t.Fatalf("reopened RPC history projection=%#v", secondMessages)
	}
	secondURL := attachmentURLForTest(t, secondMessages, "synthetic-history-1")
	secondMediaPath := pathFromFileURLForTest(t, secondURL)
	if !strings.HasPrefix(secondMediaPath, filepath.Join(secondMount, "media")+string(filepath.Separator)) {
		t.Fatalf("reopened RPC projection did not change to the new mount: %q", secondMediaPath)
	}
	if secondMediaPath == firstMediaPath {
		t.Fatalf("reopened media projection retained stale mount path %q", secondMediaPath)
	}
	if got, err := os.ReadFile(secondMediaPath); err != nil || !bytes.Equal(got, mediaMarker) {
		t.Fatalf("reopened encrypted media mismatch: bytes=%d err=%v", len(got), err)
	}
	draft, callErr := callTestRPC(socket, "draft.get", map[string]string{"chat_id": "synthetic-chat"})
	if callErr != nil {
		t.Fatalf("read persisted synthetic draft through RPC: %v", callErr)
	}
	var draftResult struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(draft, &draftResult); err != nil || draftResult.Text != draftMarker {
		t.Fatalf("reopened RPC draft=%q err=%v", draftResult.Text, err)
	}
	assertCiphertextHasNoSyntheticPlaintext(t, root, historyMarker, draftMarker,
		string(mediaMarker), "🧪", "🛡️", "Native FUSE fixture")
	assertNoPlaintextProfileFiles(t, root)
	if secondTransport.connectCount() != 0 {
		t.Fatalf("reopened native Controller acceptance connected transport %d times; expected zero", secondTransport.connectCount())
	}

	// Stop only the app's SQLite handle while retaining this real encrypted
	// mount. A synchronized child leaves a genuine hot rollback journal, then
	// the wrong-mode inspection must copy only inside the mounted profile view.
	if err = second.app.Close(); err != nil {
		t.Fatalf("close app database before hot-journal fixture: %v", err)
	}
	externalTMP := filepath.Join(base, "external-tmp")
	if err = os.Mkdir(externalTMP, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", externalTMP)
	hotReady := filepath.Join(base, "hot-journal.ready")
	hotRelease := filepath.Join(base, "hot-journal.release")
	hotCommand := exec.Command(os.Args[0], "-test.run=^TestEncryptedDaemonRPCMediaSurvivesFUSECloseAndReopen$", "-test.count=1")
	hotCommand.Env = os.Environ()
	for key, value := range map[string]string{
		"WHATSFEATHER_HOT_JOURNAL_FUSE_CHILD": "1",
		"WHATSFEATHER_HOT_JOURNAL_DB":         filepath.Join(secondMount, "history.db"),
		"WHATSFEATHER_HOT_JOURNAL_READY":      hotReady,
		"WHATSFEATHER_HOT_JOURNAL_RELEASE":    hotRelease,
	} {
		hotCommand.Env = replaceEnvironment(hotCommand.Env, key, value)
	}
	if err = hotCommand.Start(); err != nil {
		t.Fatal(err)
	}
	childFinished := false
	defer func() {
		if !childFinished {
			_ = os.WriteFile(hotRelease, []byte("release\n"), 0600)
			_ = hotCommand.Wait()
		}
	}()
	if !waitForIntegrationFile(hotReady, 10*time.Second) {
		t.Fatal("FUSE hot-journal writer did not publish its synchronized crash point")
	}
	dbPath := filepath.Join(secondMount, "history.db")
	journalPath := dbPath + "-journal"
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil || len(journalBefore) < 8 || !bytes.Equal(journalBefore[:8], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
		t.Fatalf("FUSE fixture did not leave a real hot-journal header: size=%d err=%v", len(journalBefore), err)
	}
	modeErr := assertRecoverySnapshotInsideMountedView(t, externalTMP, secondMount, func() error {
		_, openErr := app.Open(secondMount, true)
		return openErr
	})
	if modeErr == nil || !strings.Contains(modeErr.Error(), "profile mode mismatch") {
		t.Fatalf("wrong-mode FUSE hot-journal inspection error = %v, want profile mode mismatch", modeErr)
	}
	dbAfterWrongMode, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	journalAfterWrongMode, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dbAfterWrongMode, dbBefore) || !bytes.Equal(journalAfterWrongMode, journalBefore) {
		t.Fatal("wrong-mode FUSE hot-journal inspection mutated the original database or journal")
	}
	t.Log("wrong-mode FUSE hot-journal inspection preserved original database and journal bytes")
	if entries, readErr := os.ReadDir(secondMount); readErr != nil {
		t.Fatal(readErr)
	} else {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".whatsfeather-recovery-") {
				t.Fatalf("FUSE recovery inspection left protected snapshot workspace %q", entry.Name())
			}
		}
	}
	if err = os.WriteFile(hotRelease, []byte("release\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = hotCommand.Wait(); err != nil {
		t.Fatalf("FUSE hot-journal crash writer: %v", err)
	}
	childFinished = true

	recovered, err := app.Open(secondMount, false)
	if err != nil {
		t.Fatalf("matching-mode FUSE hot-journal recovery: %v", err)
	}
	page, err := recovered.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-chat","limit":10}`))
	if err != nil {
		recovered.Close()
		t.Fatalf("read recovered FUSE history: %v", err)
	}
	pageBytes, err := json.Marshal(page)
	if err != nil || !bytes.Contains(pageBytes, []byte(historyMarker)) || bytes.Contains(pageBytes, []byte("UNCOMMITTED-FUSE-HOT-JOURNAL")) {
		recovered.Close()
		t.Fatalf("matching-mode FUSE recovery did not preserve committed history: %s (%v)", pageBytes, err)
	}
	statusValue, statusErr := recovered.Handle(context.Background(), "app.status", []byte(`{}`))
	statusBytes, marshalErr := json.Marshal(statusValue)
	if statusErr != nil || marshalErr != nil || !bytes.Contains(statusBytes, []byte(`"mode":"live"`)) {
		recovered.Close()
		t.Fatalf("matching-mode FUSE recovery did not retain committed live mode: %s (%v, %v)", statusBytes, statusErr, marshalErr)
	}
	if err = recovered.Close(); err != nil {
		t.Fatalf("close recovered FUSE profile: %v", err)
	}
	t.Log("matching-mode FUSE hot-journal recovery restored committed live history and mode")
	assertCiphertextHasNoSyntheticPlaintext(t, root, historyMarker, draftMarker,
		string(mediaMarker), "UNCOMMITTED-FUSE-HOT-JOURNAL", "🧪", "🛡️", "Native FUSE fixture")
	assertNoPlaintextProfileFiles(t, root)

	stopRealEncryptedTestDaemon(t, secondStop)
	if _, err := os.Lstat(secondMount); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal reopen close left the second owned mountpoint: %v", err)
	}
	if entries, err := os.ReadDir(runtimeDir); err != nil || len(entries) != 0 {
		t.Fatalf("runtime directory not clean after normal unmount: entries=%v err=%v", entryNamesForTest(entries), err)
	}
	t.Logf("synthetic RPC state across real FUSE close/reopen verified: mount %s -> %s; media projection %s -> %s; encrypted backing has no SQLite/plaintext markers", firstMount, secondMount, firstMediaPath, secondMediaPath)
}

func runEncryptedFUSEHotJournalWriter(t *testing.T) {
	t.Helper()
	databasePath := os.Getenv("WHATSFEATHER_HOT_JOURNAL_DB")
	ready := os.Getenv("WHATSFEATHER_HOT_JOURNAL_READY")
	release := os.Getenv("WHATSFEATHER_HOT_JOURNAL_RELEASE")
	db, err := sql.Open("sqlite", "file:"+databasePath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=DELETE; PRAGMA cache_size=5; BEGIN IMMEDIATE"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE meta SET value='demo' WHERE key='mode'"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	largeBody := strings.Repeat("UNCOMMITTED-FUSE-HOT-JOURNAL-", 2048)
	if _, err = db.Exec("UPDATE messages SET body=? WHERE id='synthetic-history-1' AND chat_id='synthetic-chat'", largeBody); err != nil {
		db.Close()
		t.Fatal(err)
	}
	journal, err := os.ReadFile(databasePath + "-journal")
	if err != nil || len(journal) < 8 || !bytes.Equal(journal[:8], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
		db.Close()
		t.Fatalf("expected actual FUSE hot rollback journal, read error=%v size=%d", err, len(journal))
	}
	if err = os.WriteFile(ready, []byte("hot journal verified\n"), 0600); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if !waitForIntegrationFile(release, 10*time.Second) {
		db.Close()
		t.Fatal("parent did not release synchronized FUSE hot-journal writer")
	}
	os.Exit(0)
}

func waitForIntegrationFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func assertRecoverySnapshotInsideMountedView(t *testing.T, externalTMP, profileRoot string, open func() error) error {
	t.Helper()
	watch := func(path string) int {
		fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
		if err != nil {
			t.Fatalf("watch recovery location %q: %v", path, err)
		}
		if _, err = syscall.InotifyAddWatch(fd, path, syscall.IN_CREATE|syscall.IN_MOVED_TO); err != nil {
			syscall.Close(fd)
			t.Fatalf("watch recovery location entries %q: %v", path, err)
		}
		return fd
	}
	externalFD := watch(externalTMP)
	defer syscall.Close(externalFD)
	profileFD := watch(profileRoot)
	defer syscall.Close(profileFD)
	openErr := open()
	readEvents := func(fd int) int {
		buffer := make([]byte, 64*1024)
		total := 0
		for {
			n, err := syscall.Read(fd, buffer)
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				break
			}
			if err != nil {
				t.Fatalf("read recovery location events: %v", err)
			}
			total += n
		}
		return total
	}
	externalEvents := readEvents(externalFD)
	profileEvents := readEvents(profileFD)
	if externalEvents != 0 {
		t.Errorf("FUSE recovery inspection created %d filesystem event bytes in external TMPDIR %q", externalEvents, externalTMP)
	}
	if profileEvents == 0 {
		t.Errorf("FUSE recovery inspection created no snapshot entry inside mounted profile view %q", profileRoot)
	}
	t.Logf("FUSE recovery snapshot event bytes: mounted profile=%d external TMPDIR=%d", profileEvents, externalEvents)
	return openErr
}

type integrationMemoryKeyring struct {
	mu  sync.Mutex
	key []byte
}

func (k *integrationMemoryKeyring) Lookup(context.Context, string, string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.key) == 0 {
		return nil, profilevault.ErrKeyNotFound
	}
	return append([]byte(nil), k.key...), nil
}

func (k *integrationMemoryKeyring) Store(_ context.Context, _, _ string, key []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.key) != 0 {
		return profilevault.ErrKeyringConflict
	}
	k.key = append([]byte(nil), key...)
	return nil
}

func (k *integrationMemoryKeyring) wipe() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := range k.key {
		k.key[i] = 0
	}
	k.key = nil
}

type integrationMediaTransport struct {
	mu           sync.Mutex
	connectCalls int
}

func (t *integrationMediaTransport) Connect(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connectCalls++
	return nil
}
func (*integrationMediaTransport) Disconnect() {}
func (*integrationMediaTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("synthetic integration transport never sends")
}
func (*integrationMediaTransport) DownloadMedia(_ context.Context, payload []byte, path string) error {
	return os.WriteFile(path, payload, 0600)
}
func (t *integrationMediaTransport) connectCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.connectCalls
}

func integrationGocryptfs(t *testing.T) string {
	t.Helper()
	path := os.Getenv("WHATSFEATHER_TEST_GCRYPTFS")
	if path == "" {
		t.Fatal("real native/FUSE acceptance requires explicit WHATSFEATHER_TEST_GCRYPTFS")
	}
	if err := validateIntegrationExecutable(path); err != nil {
		t.Fatalf("vetted gocryptfs executable is invalid: %v", err)
	}
	return path
}

func validateIntegrationExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("executable path must be clean and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 || info.Mode()&0022 != 0 {
		return errors.New("executable must be regular, executable, and not group/world writable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != 0 && int(stat.Uid) != os.Geteuid()) {
		return errors.New("executable has an unexpected owner")
	}
	return nil
}

func integrationStickerFixture(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "internal", "app", "testdata", "sticker-static.webp"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() == 0 {
		t.Fatalf("synthetic sticker fixture is missing or unsafe: %v", err)
	}
	return path
}

func runNativeEncryptedProfileController(t *testing.T, executable, action, socket,
	ciphertextRoot, runtimeDir, profileRoot, mountID, decoyRoot, fixture string) {
	t.Helper()
	command := exec.Command(executable, "realMountedEncryptedProfileControllerAcceptance")
	command.Env = os.Environ()
	for key, value := range map[string]string{
		"WHATSFEATHER_TEST_PROFILE_SOCKET":          socket,
		"WHATSFEATHER_TEST_PROFILE_CIPHERTEXT_ROOT": ciphertextRoot,
		"WHATSFEATHER_TEST_PROFILE_RUNTIME_DIR":     runtimeDir,
		"WHATSFEATHER_TEST_PROFILE_MOUNT_ROOT":      profileRoot,
		"WHATSFEATHER_TEST_PROFILE_MOUNT_ID":        mountID,
		"WHATSFEATHER_TEST_PROFILE_DECOY_ROOT":      decoyRoot,
		"WHATSFEATHER_TEST_PROFILE_STICKER_FIXTURE": fixture,
		"WHATSFEATHER_TEST_PROFILE_ACTION":          action,
		"QT_QPA_PLATFORM":                           "offscreen",
		"QT_FORCE_STDERR_LOGGING":                   "1",
	} {
		command.Env = replaceEnvironment(command.Env, key, value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real native Controller %q acceptance failed: %v\n%s", action, err, output)
	}
	marker := "PASS   : ProfileStorageTest::realMountedEncryptedProfileControllerAcceptance()"
	if !bytes.Contains(output, []byte(marker)) {
		t.Fatalf("native Controller %q did not report its acceptance slot; output:\n%s", action, output)
	}
	t.Logf("native Controller %q acceptance: %s", action, strings.TrimSpace(string(output)))
}

func replaceEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, prefix+value)
}

func integrationTempDir(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("TMPDIR")
	if parent == "" {
		parent = os.TempDir()
	}
	base, err := os.MkdirTemp(parent, "wf-e-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		runtime := filepath.Join(base, "runtime")
		if entries, err := os.ReadDir(runtime); err == nil && len(entries) != 0 {
			t.Errorf("do not remove synthetic test files while owned runtime entries remain: %v", entryNamesForTest(entries))
			return
		}
		if err := os.RemoveAll(base); err != nil {
			t.Errorf("remove closed synthetic encrypted test profile: %v", err)
		}
	})
	return base
}

func startRealEncryptedTestDaemon(t *testing.T, root, runtimeDir, socket string, keyring profilevault.Keyring, engine, unmount string) (*daemonRuntime, *integrationMediaTransport, *testDaemonStopper) {
	t.Helper()
	deps := productionBootstrapDependencies()
	deps.keyring = keyring
	deps.gocryptfsPath = engine
	deps.fuseUnmountPath = unmount
	transport := &integrationMediaTransport{}
	deps.newAdapter = func(*app.App, string, *whatsapp.LiveTestScope) app.Transport { return transport }
	ctx := context.Background()
	runtime, err := bootstrap(ctx, bootstrapOptions{dataDir: root, runtimeDir: runtimeDir, socketPath: socket, live: true}, deps)
	if err != nil {
		t.Fatalf("open real encrypted daemon: %v", err)
	}
	if runtime.vault == nil {
		t.Fatal("live daemon returned without an encrypted vault owner")
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(runCtx) }()
	waitForTestSocket(t, socket)
	stopper := &testDaemonStopper{cancel: cancel, done: done}
	t.Cleanup(func() {
		if err := stopper.Stop(); err != nil {
			t.Errorf("cleanup encrypted daemon: %v", err)
		}
	})
	return runtime, transport, stopper
}

type testDaemonStopper struct {
	cancel context.CancelFunc
	done   <-chan error
	once   sync.Once
	err    error
}

func (s *testDaemonStopper) Stop() error {
	s.once.Do(func() {
		s.cancel()
		select {
		case s.err = <-s.done:
		case <-time.After(shutdownCloseBudget + 5*time.Second):
			s.err = errors.New("encrypted daemon did not finish bounded normal teardown")
		}
	})
	return s.err
}

func stopRealEncryptedTestDaemon(t *testing.T, stopper *testDaemonStopper) {
	t.Helper()
	if err := stopper.Stop(); err != nil {
		t.Errorf("close encrypted daemon and normally unmount FUSE: %v", err)
	}
}

func waitForTestSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("RPC socket %q did not become available", socket)
}

func callTestRPC(socket, method string, params any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"v": 1, "id": "synthetic-test-request", "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *app.Error      `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	return response.Result, nil
}

func callMessageList(t *testing.T, socket string) []struct {
	ID         string `json:"id"`
	Body       string `json:"body"`
	Attachment *struct {
		URL   string `json:"url"`
		State string `json:"state"`
	} `json:"attachment"`
} {
	t.Helper()
	result, err := callTestRPC(socket, "message.list", map[string]any{"chat_id": "synthetic-chat", "limit": 10})
	if err != nil {
		t.Fatalf("read persisted history through RPC: %v", err)
	}
	var response struct {
		Messages []struct {
			ID         string `json:"id"`
			Body       string `json:"body"`
			Attachment *struct {
				URL   string `json:"url"`
				State string `json:"state"`
			} `json:"attachment"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		t.Fatalf("decode message.list result: %v", err)
	}
	return response.Messages
}

func attachmentURLForTest(t *testing.T, messages []struct {
	ID         string `json:"id"`
	Body       string `json:"body"`
	Attachment *struct {
		URL   string `json:"url"`
		State string `json:"state"`
	} `json:"attachment"`
}, id string) string {
	t.Helper()
	for _, message := range messages {
		if message.ID == id {
			if message.Attachment == nil || message.Attachment.State != "ready" {
				t.Fatalf("message %q has no ready projected attachment: %#v", id, message.Attachment)
			}
			return message.Attachment.URL
		}
	}
	t.Fatalf("message %q missing from RPC history: %#v", id, messages)
	return ""
}

func pathFromFileURLForTest(t *testing.T, value string) string {
	t.Helper()
	if !strings.HasPrefix(value, "file://") {
		t.Fatalf("attachment projection is not a local file URL: %q", value)
	}
	path := strings.TrimPrefix(value, "file://")
	if !filepath.IsAbs(path) {
		t.Fatalf("attachment file URL path is not absolute: %q", value)
	}
	return path
}

func assertCiphertextHasNoSyntheticPlaintext(t *testing.T, root string, markers ...string) {
	t.Helper()
	cipher := filepath.Join(root, "cipher")
	files := 0
	err := filepath.WalkDir(cipher, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in encrypted backing")
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("SQLite format 3\x00")) {
			return errors.New("SQLite header appears in cipher backing")
		}
		for _, marker := range markers {
			if marker != "" && bytes.Contains(data, []byte(marker)) {
				return errors.New("synthetic plaintext marker appears in cipher backing")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspect encrypted backing: %v", err)
	}
	if files < 3 {
		t.Fatalf("encrypted backing contains only %d files; expected manifest/cipher payload", files)
	}
}

func assertNoPlaintextProfileFiles(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"history.db", "device.db", "media", "local-expressions"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plaintext persistent profile path %q exists outside cipher root: %v", name, err)
		}
	}
	cipher := filepath.Join(root, "cipher")
	for _, name := range []string{"history.db", "device.db", "media", "local-expressions", "preferences.json", "stickers"} {
		if _, err := os.Lstat(filepath.Join(cipher, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plaintext filename %q exists in encrypted backing: %v", name, err)
		}
	}
}

func snapshotSyntheticLegacyProfile(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("synthetic legacy source contains an unsafe entry")
		}
		rel := "."
		if path != root {
			rel, err = filepath.Rel(root, path)
			if err != nil {
				return err
			}
		}
		digest := "directory"
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash := sha256.Sum256(data)
			digest = hex.EncodeToString(hash[:])
		} else if !info.IsDir() {
			return errors.New("synthetic legacy source contains a non-regular entry")
		}
		result[rel] = fmt.Sprintf("%#o:%d:%d:%s", info.Mode().Perm(), info.Size(), info.ModTime().UnixNano(), digest)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot synthetic legacy source: %v", err)
	}
	return result
}

func equalSyntheticSnapshots(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if right[name] != value {
			return false
		}
	}
	return true
}

func readRequiredTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expected synthetic fixture file: %v", err)
	}
	return data
}
