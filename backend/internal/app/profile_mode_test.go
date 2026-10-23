package app

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type profileSnapshot struct {
	bytesHash   string
	schema      []string
	meta        []string
	messagePK   []string
	messages    []string
	attachments []string
}

func testSQLiteURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

func createLegacyProfile(t *testing.T, dir, mode string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	db, err := sql.Open("sqlite", testSQLiteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
INSERT INTO meta(key,value) VALUES('mode',?);
CREATE TABLE chats(id TEXT PRIMARY KEY,title TEXT NOT NULL,initials TEXT NOT NULL,color TEXT NOT NULL,is_group INTEGER NOT NULL DEFAULT 0);
CREATE TABLE messages(id TEXT PRIMARY KEY,chat_id TEXT NOT NULL,body TEXT NOT NULL,timestamp INTEGER NOT NULL,outgoing INTEGER NOT NULL,status TEXT NOT NULL,client_id TEXT UNIQUE);
CREATE INDEX timeline ON messages(chat_id,timestamp,id);
CREATE TABLE drafts(chat_id TEXT PRIMARY KEY,text TEXT NOT NULL);
INSERT INTO chats VALUES('chat','Chat','C','#123456',0);
INSERT INTO messages VALUES('sentinel','chat','preserved',123,1,'queued','client-sentinel');
INSERT INTO messages VALUES('sending','chat','also preserved',124,1,'sending',NULL);
INSERT INTO drafts VALUES('chat','draft survives');`, mode)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func createUnmarkedProfile(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	db, err := sql.Open("sqlite", testSQLiteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE foreign_data(value TEXT); INSERT INTO foreign_data VALUES('do not adopt');"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func snapshotProfile(t *testing.T, path string) profileSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := profileSnapshot{}
	hash := sha256.Sum256(data)
	s.bytesHash = hex.EncodeToString(hash[:])
	db, err := sql.Open("sqlite", testSQLiteURI(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type||':'||name||':'||COALESCE(sql,'') FROM sqlite_schema ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		s.schema = append(s.schema, value)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	rows, err = db.Query("SELECT name||':'||pk FROM pragma_table_info('messages') WHERE pk>0 ORDER BY pk")
	if err == nil {
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			s.messagePK = append(s.messagePK, value)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatal(err)
	}
	for table, dst := range map[string]*[]string{"meta": &s.meta, "messages": &s.messages, "attachments": &s.attachments} {
		if table == "meta" {
			rows, err = db.Query("SELECT key||'='||value FROM meta ORDER BY key")
		} else if table == "messages" {
			rows, err = db.Query("SELECT id||'|'||chat_id||'|'||body||'|'||timestamp||'|'||outgoing||'|'||status||'|'||COALESCE(client_id,'') FROM messages ORDER BY rowid")
		} else {
			rows, err = db.Query("SELECT chat_id||'|'||message_id||'|'||meta FROM attachments ORDER BY rowid")
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err = rows.Scan(&value); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			*dst = append(*dst, value)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return s
}

func TestWrongModeRejectsBeforeAnyMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		storedMode string
		requested  bool
	}{
		{name: "demo legacy opened live", storedMode: "demo", requested: false},
		{name: "live legacy opened demo", storedMode: "live", requested: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile", "fixture")
			path := createLegacyProfile(t, dir, tc.storedMode)
			before := snapshotProfile(t, path)
			_, err := Open(dir, tc.requested)
			if err == nil || !strings.Contains(err.Error(), "profile mode mismatch") {
				t.Fatalf("Open() error = %v, want profile mode mismatch", err)
			}
			after := snapshotProfile(t, path)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected open mutated profile:\nbefore=%+v\nafter=%+v", before, after)
			}
			if after.messages[0] == "" || !strings.Contains(strings.Join(after.messages, "\n"), "queued") || !strings.Contains(strings.Join(after.messages, "\n"), "sending") {
				t.Fatalf("sentinel statuses missing after rejection: %#v", after.messages)
			}
		})
	}
}

func TestWrongModePathSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile with spaces #?%&=", "fixture")
	path := createLegacyProfile(t, dir, "demo")
	before := snapshotProfile(t, path)
	if _, err := Open(dir, false); err == nil || !strings.Contains(err.Error(), "profile mode mismatch") {
		t.Fatalf("Open() error = %v, want profile mode mismatch", err)
	}
	after := snapshotProfile(t, path)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("special-path rejection mutated profile:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestWrongModeCurrentSchemaPreservesAttachmentMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "current profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO attachments(chat_id,message_id,meta) VALUES(?,?,?)", "demo:alex", "seed-0-000", `{"name":"sentinel"}`); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	before := snapshotProfile(t, path)
	if _, err = Open(dir, false); err == nil || !strings.Contains(err.Error(), "profile mode mismatch") {
		t.Fatalf("Open() error = %v, want profile mode mismatch", err)
	}
	after := snapshotProfile(t, path)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected current-schema open mutated profile:\nbefore=%+v\nafter=%+v", before, after)
	}
	if len(after.attachments) != 1 || !strings.Contains(after.attachments[0], "sentinel") {
		t.Fatalf("attachment metadata missing: %#v", after.attachments)
	}
}

func TestCorrectModeLegacyOpenMigratesHistoryDraftAndMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "legacy profile")
	createLegacyProfile(t, dir, "live")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var primaryColumns int
	if err = a.db.QueryRow("SELECT count(*) FROM pragma_table_info('messages') WHERE pk>0").Scan(&primaryColumns); err != nil {
		t.Fatal(err)
	}
	if primaryColumns != 2 {
		t.Fatalf("legacy schema was not migrated, primary columns=%d", primaryColumns)
	}
	var body, draft, mode string
	if err = a.db.QueryRow("SELECT body FROM messages WHERE id='sentinel' AND chat_id='chat'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT text FROM drafts WHERE chat_id='chat'").Scan(&draft); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT value FROM meta WHERE key='mode'").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if body != "preserved" || draft != "draft survives" || mode != "live" {
		t.Fatalf("history/draft/mode not preserved: body=%q draft=%q mode=%q", body, draft, mode)
	}
}

func TestFreshEmptyProfileInitializes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty profile [special]?%")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var mode string
	if err = a.db.QueryRow("SELECT value FROM meta WHERE key='mode'").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "demo" {
		t.Fatalf("mode=%q, want demo", mode)
	}
}

func TestUnmarkedNonemptyDatabaseFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unmarked")
	path := createUnmarkedProfile(t, dir)
	before := snapshotProfile(t, path)
	if _, err := Open(dir, false); err == nil {
		t.Fatal("unmarked nonempty database was adopted")
	}
	after := snapshotProfile(t, path)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("unmarked database was mutated:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestUnknownMarkerFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "unknown marker")
	path := createLegacyProfile(t, dir, "future")
	before := snapshotProfile(t, path)
	if _, err := Open(dir, false); err == nil || !strings.Contains(err.Error(), "unmarked or unknown profile") {
		t.Fatalf("Open() error = %v, want unknown-profile rejection", err)
	}
	after := snapshotProfile(t, path)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("unknown-marker database was mutated:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestSymlinkDatabaseRejectedBeforeRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "symlink profile")
	targetDir := filepath.Join(t.TempDir(), "target")
	target := createLegacyProfile(t, targetDir, "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir, false); err == nil || !strings.Contains(err.Error(), "unsafe database path") {
		t.Fatalf("Open() error = %v, want unsafe database path", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("symlink rejection changed target database")
	}
}

func TestSQLiteURIEncodingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "name with spaces #hash?query%percent&equals.db")
	db, err := sql.Open("sqlite", testSQLiteURI(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE marker(value TEXT); INSERT INTO marker VALUES('ok')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite", testSQLiteURI(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var value string
	if err = ro.QueryRow("SELECT value FROM marker").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "ok" {
		t.Fatalf("value=%q", value)
	}
}

func TestHotJournalModeGuardRecoversCommittedView(t *testing.T) {
	if os.Getenv("WHATSFEATHER_HOT_JOURNAL_CHILD") == "1" {
		runHotJournalWriter(t)
		return
	}

	dir := filepath.Join(t.TempDir(), "hot profile")
	a, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	const originalBody = "committed original body"
	if _, err = a.db.Exec("UPDATE messages SET body=? WHERE id='seed-0-119' AND chat_id='demo:alex'", originalBody); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT INTO drafts(chat_id,text) VALUES('demo:alex','committed draft')"); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	tmpRoot := filepath.Join(t.TempDir(), "recovery-tmp")
	if err = os.Mkdir(tmpRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpRoot)
	ready := filepath.Join(t.TempDir(), "writer.ready")
	release := filepath.Join(t.TempDir(), "writer.release")
	cmd := exec.Command(os.Args[0], "-test.run=TestHotJournalModeGuardRecoversCommittedView", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"WHATSFEATHER_HOT_JOURNAL_CHILD=1",
		"WHATSFEATHER_HOT_JOURNAL_DB="+filepath.Join(dir, "history.db"),
		"WHATSFEATHER_HOT_JOURNAL_READY="+ready,
		"WHATSFEATHER_HOT_JOURNAL_RELEASE="+release,
	)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !waitForTestFile(ready, 10*time.Second) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("crash writer did not publish its synchronized hot-journal point")
	}

	dbPath := filepath.Join(dir, "history.db")
	journalPath := dbPath + "-journal"
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("read hot rollback journal: %v", err)
	}
	if len(journalBefore) == 0 || !bytes.Equal(journalBefore[:minTest(len(journalBefore), 8)], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("writer did not leave the expected hot-journal header: size=%d header=%x", len(journalBefore), journalBefore[:minTest(len(journalBefore), 8)])
	}
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	dbBeforeHash := sha256.Sum256(dbBefore)
	journalBeforeHash := sha256.Sum256(journalBefore)
	entriesBefore, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = Open(dir, false); err == nil || !strings.Contains(err.Error(), "profile mode mismatch") {
		t.Fatalf("wrong-mode hot-journal Open() error = %v, want profile mode mismatch", err)
	}
	dbAfterWrong, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	journalAfterWrong, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(dbAfterWrong) != dbBeforeHash || sha256.Sum256(journalAfterWrong) != journalBeforeHash {
		t.Fatal("wrong-mode hot-journal rejection changed the original database or journal bytes")
	}

	if err = os.WriteFile(release, []byte("exit now\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatalf("crash writer: %v", err)
	}

	a, err = Open(dir, true)
	if err != nil {
		t.Fatalf("correct-mode hot-journal recovery: %v", err)
	}
	var body, draft, mode string
	if err = a.db.QueryRow("SELECT body FROM messages WHERE id='seed-0-119' AND chat_id='demo:alex'").Scan(&body); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT text FROM drafts WHERE chat_id='demo:alex'").Scan(&draft); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT value FROM meta WHERE key='mode'").Scan(&mode); err != nil {
		a.Close()
		t.Fatal(err)
	}
	if body != originalBody || strings.Contains(body, "UNCOMMITTED") || draft != "committed draft" || mode != "demo" {
		a.Close()
		t.Fatalf("recovered view = body %q draft %q mode %q", body, draft, mode)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback journal after correct recovery: stat error=%v", err)
	}
	entriesAfter, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(entriesAfter, entriesBefore) {
		t.Fatalf("recovery snapshot temporary files were not cleaned: before=%v after=%v", entriesBefore, entriesAfter)
	}
}

func runHotJournalWriter(t *testing.T) {
	t.Helper()
	dbPath := os.Getenv("WHATSFEATHER_HOT_JOURNAL_DB")
	ready := os.Getenv("WHATSFEATHER_HOT_JOURNAL_READY")
	release := os.Getenv("WHATSFEATHER_HOT_JOURNAL_RELEASE")
	db, err := sql.Open("sqlite", testSQLiteURI(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA journal_mode=DELETE; PRAGMA cache_size=5; BEGIN IMMEDIATE"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	largeBody := strings.Repeat("UNCOMMITTED-BODY-", 2048)
	if _, err = db.Exec("UPDATE meta SET value='live' WHERE key='mode'"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE messages SET body=?", largeBody); err != nil {
		db.Close()
		t.Fatal(err)
	}
	journalPath := dbPath + "-journal"
	journal, err := os.ReadFile(journalPath)
	if err != nil || len(journal) < 8 || !bytes.Equal(journal[:8], []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}) {
		db.Close()
		t.Fatalf("expected actual hot rollback journal, read error=%v size=%d", err, len(journal))
	}
	if err = os.WriteFile(ready, []byte("journal header verified\n"), 0600); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if !waitForTestFile(release, 10*time.Second) {
		db.Close()
		t.Fatal("parent did not release deterministic crash point")
	}
	os.Exit(0)
}

func waitForTestFile(path string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

func minTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestProfileSnapshotIsStable(t *testing.T) {
	// Keep the test helper honest: a snapshot includes bytes, schema, and rows.
	dir := filepath.Join(t.TempDir(), "snapshot")
	path := createLegacyProfile(t, dir, "demo")
	one := snapshotProfile(t, path)
	two := snapshotProfile(t, path)
	if !reflect.DeepEqual(one, two) {
		t.Fatalf("identical snapshots differ:\n%+v\n%+v", one, two)
	}
	if one.bytesHash == "" || len(one.schema) == 0 || len(one.messagePK) != 1 || len(one.messages) != 2 {
		t.Fatalf("incomplete snapshot: %+v", one)
	}
}
