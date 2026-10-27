package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow/types"
	"io"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string    { return e.Message }
func fail(code, msg string) error { return &Error{code, msg} }

type Chat struct {
	ID                      string   `json:"id"`
	Title                   string   `json:"title"`
	Preview                 string   `json:"preview"`
	Timestamp               int64    `json:"timestamp"`
	Unread                  int      `json:"unread"`
	Initials                string   `json:"initials"`
	Color                   string   `json:"color"`
	IsGroup                 bool     `json:"is_group"`
	Archived                bool     `json:"archived,omitempty"`
	Pinned                  bool     `json:"pinned,omitempty"`
	Muted                   bool     `json:"muted,omitempty"`
	UnreadMark              bool     `json:"marked_unread,omitempty"`
	Favorite                bool     `json:"favorite,omitempty"`
	ListIDs                 []string `json:"list_ids,omitempty"`
	OrganizationProvenance  string   `json:"organization_provenance,omitempty"`
	CommunityID             string   `json:"community_id,omitempty"`
	CommunityTitle          string   `json:"community_title,omitempty"`
	IsCommunityAnnouncement *bool    `json:"is_community_announcement,omitempty"`
}
type Message struct {
	ID                   string                  `json:"id"`
	ChatID               string                  `json:"chat_id"`
	Body                 string                  `json:"body"`
	Timestamp            int64                   `json:"timestamp"`
	Outgoing             bool                    `json:"outgoing"`
	Status               string                  `json:"status"`
	Kind                 string                  `json:"kind,omitempty"`
	SenderName           string                  `json:"sender_name,omitempty"`
	SenderID             string                  `json:"sender_id,omitempty"`
	QuotedBody           string                  `json:"quoted_body,omitempty"`
	QuotedSender         string                  `json:"quoted_sender,omitempty"`
	Attachment           *Attachment             `json:"attachment,omitempty"`
	Album                *[]Attachment           `json:"album,omitempty"`
	ContactCards         *[]ContactCard          `json:"contacts,omitempty"`
	Location             *LocationProjection     `json:"location,omitempty"`
	Poll                 *PollProjection         `json:"poll,omitempty"`
	PollResults          *PollResultsProjection  `json:"poll_results,omitempty"`
	PollSelfVote         *PollSelfVoteProjection `json:"poll_self_vote,omitempty"`
	PollVoteState        string                  `json:"poll_vote_state,omitempty"`
	List                 *ListProjection         `json:"list,omitempty"`
	ListActionRows       *[]ListActionRow        `json:"-"`
	ListReplyAvailable   bool                    `json:"list_reply_available,omitempty"`
	ListReplyState       string                  `json:"list_reply_state,omitempty"`
	Interactive          *TextCardProjection     `json:"interactive,omitempty"`
	Template             *TextCardProjection     `json:"template,omitempty"`
	LinkPreview          *LinkPreviewProjection  `json:"link_preview,omitempty"`
	Buttons              *ButtonsProjection      `json:"buttons,omitempty"`
	ButtonActionRows     *[]ButtonActionRow      `json:"-"`
	ButtonReplyAvailable bool                    `json:"button_reply_available,omitempty"`
	ButtonReplyState     string                  `json:"button_reply_state,omitempty"`
	MediaPayload         string                  `json:"-"`
	ReplyToID            string                  `json:"reply_to_id,omitempty"`
	Reactions            *[]Reaction             `json:"reactions,omitempty"`
	Edited               bool                    `json:"edited,omitempty"`
	Deleted              bool                    `json:"deleted,omitempty"`
	Starred              bool                    `json:"starred,omitempty"`
	Pinned               bool                    `json:"pinned,omitempty"`
	Forwarded            bool                    `json:"forwarded,omitempty"`
	MutationProvenance   string                  `json:"mutation_provenance,omitempty"`
}

type Reaction struct {
	SenderID   string `json:"sender_id"`
	Emoji      string `json:"emoji"`
	Timestamp  int64  `json:"timestamp"`
	Provenance string `json:"provenance"`
}

type Contact struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	Phone    string `json:"phone,omitempty"`
	Verified bool   `json:"verified"`
}

type ContactDirectory interface {
	ResolveContact(context.Context, string) (Contact, bool, error)
	ListContacts(context.Context) ([]Contact, error)
}

// CommunityGroupInfo is the privacy-safe metadata projection of one group
// returned by the pinned SDK. It deliberately excludes participant details.
type CommunityGroupInfo struct {
	ID                  string
	Title               string
	IsParent            bool
	LinkedParentID      string
	IsAnnouncement      bool
	IsAnnouncementKnown bool
}

// CommunityDirectory is an optional explicit-refresh transport capability.
// Implementations must read the authenticated joined-groups snapshot only.
type CommunityDirectory interface {
	GetJoinedGroups(context.Context) ([]CommunityGroupInfo, error)
}

// CommunityInfo is the minimal projection for one explicitly scoped parent.
// It intentionally has no participant fields.
type CommunityInfo struct {
	ID       string `json:"community_id"`
	Title    string `json:"title"`
	IsParent bool   `json:"is_parent"`
}

// CommunityInfoProvider is an optional target-only metadata capability.
type CommunityInfoProvider interface {
	GetCommunityInfo(context.Context, string) (CommunityInfo, error)
}

func isScopedLiveTestTransport(transport Transport) bool {
	provider, ok := transport.(ScopedLiveTestProvider)
	return ok && provider.ScopedLiveTest()
}

// ScopedCapabilityBlocker reports a static privacy-boundary reason that must
// disable scoped capabilities until the transport can enforce them safely.
type ScopedCapabilityBlocker interface {
	ScopedCapabilityBlockReason() string
}

func scopedCapabilityBlockReason(transport Transport) string {
	blocker, ok := transport.(ScopedCapabilityBlocker)
	if !ok {
		return ""
	}
	return strings.TrimSpace(blocker.ScopedCapabilityBlockReason())
}

type MessageActionTransport interface {
	SendReply(context.Context, string, string, bool, string, string, string, string) (int64, error)
	SendReaction(context.Context, string, string, bool, string, string, string) (int64, error)
	EditMessage(context.Context, string, string, string, string) (int64, error)
	RevokeMessage(context.Context, string, string, string) (int64, error)
	ForwardText(context.Context, string, string, string) (int64, error)
}

// ScopedLiveTestProvider exposes whether history-derived forwarding is disabled.
type ScopedLiveTestProvider interface {
	ScopedLiveTest() bool
}

type ListReplySender interface {
	SendListReply(context.Context, Message, string, string, string, string) (int64, error)
}

type ButtonReplySender interface {
	SendButtonReply(context.Context, Message, string, string, string) (int64, error)
}

type Account struct {
	Available   bool   `json:"available"`
	DisplayName string `json:"display_name"`
	Phone       string `json:"phone"`
	JID         string `json:"jid"`
	Username    string `json:"username"`
	About       string `json:"about"`
	AvatarURL   string `json:"avatar_url"`
	State       string `json:"state"`
}

type AccountProvider interface {
	Account(context.Context) (Account, error)
}

type MediaDownloader interface {
	DownloadMedia(context.Context, []byte, string) error
}

// MediaDownloaderWithKind is an optional compatibility path for descriptors
// written before media payloads were wrapped in waE2E.Message. Kind comes from
// the trusted stored attachment metadata, never from the RPC request.
type MediaDownloaderWithKind interface {
	DownloadMediaKind(context.Context, []byte, string, string) error
}

// ScopedMediaAccess ties a manual downloader invocation to the exact message
// looked up in the selected profile after chat, message ID and timestamp
// policy have passed. RPC callers never provide media descriptors directly.
type ScopedMediaAccess interface {
	AllowManualMedia(chat, id string, timestamp int64) bool
	DownloadScopedMedia(context.Context, string, string, int64, []byte, string, string) error
}

type HistoryAnchor struct {
	ChatID    string
	ID        string
	Timestamp int64
	Outgoing  bool
}

type HistoryRequester interface {
	RequestHistory(context.Context, string, HistoryAnchor, int) error
}

type MediaSender interface {
	SendMedia(context.Context, string, string, Attachment, string, string) (int64, error)
}

type ContactCardSender interface {
	SendContacts(context.Context, string, []ContactCard, string) (int64, error)
}

type LocationSender interface {
	SendLocation(context.Context, string, LocationProjection, string) (int64, error)
}

type unknownOutcome interface {
	UnknownOutcome() bool
}
type Cursor struct {
	Timestamp int64  `json:"timestamp"`
	ID        string `json:"id"`
}
type Transport interface {
	Connect(context.Context) error
	Disconnect()
	Send(context.Context, string, string, string) (int64, error)
}
type connectAttempt struct {
	ctx       context.Context
	cancel    context.CancelFunc
	transport Transport
}

type App struct {
	mu                  sync.Mutex
	communityMu         sync.Mutex
	mediaMu             sync.Mutex
	historyMu           sync.Mutex
	activeOps           int
	db                  *sql.DB
	demo                bool
	state, info, qr     string
	revision            int64
	transport           Transport
	sessionEpoch        uint64
	connectAttempt      *connectAttempt
	lastConnectCancel   context.CancelFunc
	disconnectRequested bool
	teardownActive      bool
	lifecycleDone       chan struct{}
	closing             bool
	closed              bool
	closeDone           chan struct{}
	closeErr            error
	transportCloseErr   error
	dir                 string
	storageView         map[string]any
	gifMu               sync.Mutex
	gifCache            map[string]gifResult
	prepared            map[string]Attachment
	notificationEpoch   string
	notificationSeq     uint64
	notificationEvents  []notificationEvent
}

func sqliteProfileURI(path string, readOnly bool) string {
	query := url.Values{}
	query.Add("_pragma", "temp_store(MEMORY)")
	if readOnly {
		query.Set("mode", "ro")
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

// Keep the preflight copy bounded. This is an inspection budget, not a claim
// that larger profiles are supported for hot-journal mode inspection.
const recoverySnapshotBudget int64 = 256 << 20

type profileFileMetadata struct {
	info os.FileInfo
}

func regularProfileFile(path string) (profileFileMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return profileFileMetadata{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return profileFileMetadata{}, fmt.Errorf("unsafe database path")
	}
	return profileFileMetadata{info: info}, nil
}

func sameProfileFile(a, b os.FileInfo) bool {
	return a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() && os.SameFile(a, b)
}

func copyRecoveryFile(src, dst string, budget *int64) error {
	before, err := regularProfileFile(src)
	if err != nil {
		return err
	}
	if before.info.Size() > *budget {
		return fmt.Errorf("rollback-journal recovery inspection exceeds the %d MiB temporary snapshot budget", recoverySnapshotBudget>>20)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy recovery snapshot source: %w", err)
	}
	opened, err := in.Stat()
	if err != nil {
		in.Close()
		return fmt.Errorf("stat recovery snapshot source: %w", err)
	}
	if !sameProfileFile(before.info, opened) {
		in.Close()
		return fmt.Errorf("profile changed while preparing recovery inspection")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		in.Close()
		return fmt.Errorf("create recovery snapshot: %w", err)
	}
	if err = os.Chmod(dst, 0600); err != nil {
		in.Close()
		out.Close()
		return fmt.Errorf("secure recovery snapshot: %w", err)
	}
	n, copyErr := io.Copy(out, io.LimitReader(in, before.info.Size()+1))
	inErr := in.Close()
	outErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy recovery snapshot: %w", copyErr)
	}
	if inErr != nil || outErr != nil {
		return fmt.Errorf("close recovery snapshot: source=%v destination=%v", inErr, outErr)
	}
	if n != before.info.Size() {
		return fmt.Errorf("profile changed while copying recovery snapshot")
	}
	after, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("recheck recovery snapshot source: %w", err)
	}
	if !sameProfileFile(before.info, after) {
		return fmt.Errorf("profile changed while preparing recovery inspection")
	}
	*budget -= n
	return nil
}

func rollbackJournalNeedsInspection(path string) (bool, error) {
	info, err := os.Lstat(path + "-journal")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("unsafe rollback journal path")
	}
	// A non-empty rollback journal is kept out of the direct read-only path.
	// SQLite decides whether it is hot while opening the private copy.
	return info.Size() > 0, nil
}

func profileModeFromDB(db *sql.DB, expected string) error {
	var stored string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='mode'").Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unmarked or unknown profile: refusing to adopt existing database")
		}
		return fmt.Errorf("inspect profile mode: %w", err)
	}
	if stored != "demo" && stored != "live" {
		return fmt.Errorf("unmarked or unknown profile: refusing to adopt existing database")
	}
	if stored != expected {
		return fmt.Errorf("profile mode mismatch: use separate demo and live directories")
	}
	return nil
}

func recoverySnapshotDirectory(profileDir, dbPath string) (string, error) {
	if !filepath.IsAbs(profileDir) || filepath.Clean(profileDir) != profileDir ||
		filepath.Clean(dbPath) != dbPath || filepath.Dir(dbPath) != profileDir || filepath.Base(dbPath) != "history.db" {
		return "", fmt.Errorf("recovery snapshot path is not bound to the current profile view")
	}
	var rootInfo os.FileInfo
	for current := profileDir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("validate protected profile view for recovery inspection: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("unsafe symlink in protected profile view path")
		}
		if current == profileDir {
			rootInfo = info
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("protected profile view is not a directory")
	}
	stat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return "", fmt.Errorf("protected profile view has a foreign owner")
	}
	tempDir, err := os.MkdirTemp(profileDir, ".whatsfeather-recovery-")
	if err != nil {
		return "", fmt.Errorf("create recovery inspection directory in protected profile view: %w", err)
	}
	if err = os.Chmod(tempDir, 0700); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", fmt.Errorf("secure recovery inspection directory: %w", err)
	}
	return tempDir, nil
}

func inspectRecoverySnapshot(profileDir, path, expected string) (result error) {
	if filepath.Clean(path) != filepath.Join(filepath.Clean(profileDir), "history.db") {
		return fmt.Errorf("recovery snapshot path does not match profile database")
	}
	dbSource, err := regularProfileFile(path)
	if err != nil {
		return err
	}
	journalSource, err := regularProfileFile(path + "-journal")
	if err != nil {
		return fmt.Errorf("inspect rollback journal: %w", err)
	}
	if dbSource.info.Size()+journalSource.info.Size() > recoverySnapshotBudget {
		return fmt.Errorf("rollback-journal recovery inspection exceeds the %d MiB temporary snapshot budget", recoverySnapshotBudget>>20)
	}

	tempDir, err := recoverySnapshotDirectory(profileDir, path)
	if err != nil {
		return err
	}
	cleanup := func() error { return os.RemoveAll(tempDir) }
	defer func() {
		if cleanupErr := cleanup(); result == nil && cleanupErr != nil {
			result = fmt.Errorf("clean recovery inspection: %w", cleanupErr)
		}
	}()
	copyBudget := recoverySnapshotBudget
	snapshotDB := filepath.Join(tempDir, "history.db")
	if err = copyRecoveryFile(path, snapshotDB, &copyBudget); err != nil {
		return err
	}
	snapshotJournal := snapshotDB + "-journal"
	if err = copyRecoveryFile(path+"-journal", snapshotJournal, &copyBudget); err != nil {
		return err
	}
	currentDB, err := regularProfileFile(path)
	if err != nil || !sameProfileFile(dbSource.info, currentDB.info) {
		if err != nil {
			return fmt.Errorf("recheck profile before recovery inspection: %w", err)
		}
		return fmt.Errorf("profile changed while preparing recovery inspection")
	}
	currentJournal, err := regularProfileFile(path + "-journal")
	if err != nil || !sameProfileFile(journalSource.info, currentJournal.info) {
		if err != nil {
			return fmt.Errorf("recheck rollback journal before recovery inspection: %w", err)
		}
		return fmt.Errorf("rollback journal changed while preparing recovery inspection")
	}

	snapshot, err := sql.Open("sqlite", sqliteProfileURI(snapshotDB, false))
	if err != nil {
		return fmt.Errorf("open recovery inspection snapshot: %w", err)
	}
	snapshot.SetMaxOpenConns(1)
	modeErr := profileModeFromDB(snapshot, expected)
	closeErr := snapshot.Close()
	if modeErr != nil {
		return modeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close recovery inspection snapshot: %w", closeErr)
	}
	return nil
}

func checkExistingProfileMode(profileDir, path, expected string) error {
	needsInspection, err := rollbackJournalNeedsInspection(path)
	if err != nil {
		return err
	}
	if needsInspection {
		// Never let SQLite recover the original before the mode is known. The
		// private rollback copy is discarded for both matching and mismatching
		// modes; only a matching mode reaches Open's normal writable path.
		return inspectRecoverySnapshot(profileDir, path, expected)
	}

	// Clean databases retain the direct read-only fast path. It is opened before
	// any writable SQLite connection, migration, recovery, or PRAGMA can run.
	ro, err := sql.Open("sqlite", sqliteProfileURI(path, true))
	if err != nil {
		return fmt.Errorf("inspect profile mode: %w", err)
	}
	modeErr := profileModeFromDB(ro, expected)
	closeErr := ro.Close()
	if modeErr != nil {
		return modeErr
	}
	if closeErr != nil {
		return fmt.Errorf("inspect profile mode: %w", closeErr)
	}
	return nil
}

func Open(dir string, demo bool) (*App, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("data directory must be absolute")
	}

	path := filepath.Join(dir, "history.db")
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe profile path")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	fileExists := false
	var fileInfo os.FileInfo
	if st, err := os.Lstat(path); err == nil {
		fileExists = true
		fileInfo = st
		if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe database path")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	mode := "live"
	if demo {
		mode = "demo"
	}
	if fileExists && fileInfo.Size() > 0 {
		// Do this while the existing profile is still untouched. In particular,
		// do not chmod the directory/database or let SQLite run migrations first.
		if err := checkExistingProfileMode(dir, path, mode); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteProfileURI(path, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	a := &App{dir: dir, db: db, demo: demo, state: "disconnected", info: "Not connected. Linking requires explicit approval.", revision: time.Now().UnixMilli(), closeDone: make(chan struct{}), gifCache: make(map[string]gifResult), prepared: make(map[string]Attachment)}
	if demo {
		a.state = "demo"
		a.info = "Synthetic demo · messages stay on this computer"
	}
	_, err = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS chats(id TEXT PRIMARY KEY,title TEXT NOT NULL,initials TEXT NOT NULL,color TEXT NOT NULL,is_group INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS messages(id TEXT NOT NULL,chat_id TEXT NOT NULL REFERENCES chats(id),body TEXT NOT NULL,timestamp INTEGER NOT NULL,outgoing INTEGER NOT NULL,status TEXT NOT NULL,client_id TEXT UNIQUE,kind TEXT NOT NULL DEFAULT '',sender_name TEXT NOT NULL DEFAULT '',sender_id TEXT NOT NULL DEFAULT '',quoted_body TEXT NOT NULL DEFAULT '',quoted_sender TEXT NOT NULL DEFAULT '',PRIMARY KEY(id,chat_id));
 CREATE INDEX IF NOT EXISTS timeline ON messages(chat_id,timestamp,id);
 CREATE TABLE IF NOT EXISTS drafts(chat_id TEXT PRIMARY KEY REFERENCES chats(id),text TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	var primaryColumns int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('messages') WHERE pk>0").Scan(&primaryColumns); err != nil {
		db.Close()
		return nil, err
	}
	if primaryColumns == 1 {
		tx, txErr := db.Begin()
		if txErr != nil {
			db.Close()
			return nil, txErr
		}
		_, txErr = tx.Exec(`CREATE TABLE messages_v2(id TEXT NOT NULL,chat_id TEXT NOT NULL REFERENCES chats(id),body TEXT NOT NULL,timestamp INTEGER NOT NULL,outgoing INTEGER NOT NULL,status TEXT NOT NULL,client_id TEXT UNIQUE,kind TEXT NOT NULL DEFAULT '',sender_name TEXT NOT NULL DEFAULT '',sender_id TEXT NOT NULL DEFAULT '',quoted_body TEXT NOT NULL DEFAULT '',quoted_sender TEXT NOT NULL DEFAULT '',PRIMARY KEY(id,chat_id));
  INSERT INTO messages_v2(id,chat_id,body,timestamp,outgoing,status,client_id) SELECT id,chat_id,body,timestamp,outgoing,status,client_id FROM messages;
  DROP TABLE messages; ALTER TABLE messages_v2 RENAME TO messages;
  CREATE INDEX timeline ON messages(chat_id,timestamp,id);`)
		if txErr != nil {
			tx.Rollback()
			db.Close()
			return nil, txErr
		}
		if txErr = tx.Commit(); txErr != nil {
			db.Close()
			return nil, txErr
		}
	}
	var stored string
	err = db.QueryRow("SELECT value FROM meta WHERE key='mode'").Scan(&stored)
	if err == nil && stored != mode {
		db.Close()
		return nil, fmt.Errorf("profile mode mismatch: use separate demo and live directories")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	for _, column := range []string{"kind", "sender_name", "sender_id", "quoted_body", "quoted_sender"} {
		var present int
		if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('messages') WHERE name=?", column).Scan(&present); err != nil {
			db.Close()
			return nil, err
		}
		if present == 0 {
			if _, err = db.Exec("ALTER TABLE messages ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO meta(key,value) VALUES('mode',?)", mode); err != nil {
		db.Close()
		return nil, err
	}
	// Unknown outcomes survive restarts and are never resent automatically.
	if _, err = db.Exec("UPDATE messages SET status='unknown' WHERE status IN ('queued','sending')"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS attachments(chat_id TEXT NOT NULL,message_id TEXT NOT NULL,meta TEXT NOT NULL,payload BLOB,PRIMARY KEY(chat_id,message_id),FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS message_albums(
		chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
		items TEXT NOT NULL CHECK(length(CAST(items AS BLOB))<=65536),
		source_tokens TEXT NOT NULL CHECK(length(CAST(source_tokens AS BLOB))<=2048),
		PRIMARY KEY(chat_id,message_id),
		FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE)`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS message_state(
		chat_id TEXT NOT NULL, message_id TEXT NOT NULL, reply_to_id TEXT NOT NULL DEFAULT '',
		edited INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0,
		starred INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0,
		forwarded INTEGER NOT NULL DEFAULT 0, provenance TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(chat_id,message_id),
		FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_reactions(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL, sender_id TEXT NOT NULL,
			emoji TEXT NOT NULL, timestamp INTEGER NOT NULL, provenance TEXT NOT NULL,
			PRIMARY KEY(chat_id,message_id,sender_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_contacts(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL, projection TEXT NOT NULL,
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_locations(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL, latitude REAL NOT NULL,
			longitude REAL NOT NULL, is_live INTEGER NOT NULL, caption TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', address TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS poll_latest_votes(
 chat_id TEXT NOT NULL, message_id TEXT NOT NULL, voter TEXT NOT NULL CHECK(length(voter)<=128), event_id TEXT NOT NULL CHECK(length(event_id)<=128), revision INTEGER NOT NULL, choices TEXT NOT NULL CHECK(length(choices)<=64),
 PRIMARY KEY(chat_id,message_id,voter), FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_polls(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_poll_results(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_lists(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_list_actions(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			section_index INTEGER NOT NULL CHECK(section_index BETWEEN 0 AND 9),
			row_index INTEGER NOT NULL CHECK(row_index BETWEEN 0 AND 9),
			row_id TEXT NOT NULL CHECK(length(CAST(row_id AS BLOB)) BETWEEN 1 AND 256),
			PRIMARY KEY(chat_id,message_id,section_index,row_index),
			UNIQUE(chat_id,message_id,row_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_text_cards(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('interactive','template')),
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_link_previews(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_buttons(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			projection TEXT NOT NULL CHECK(length(CAST(projection AS BLOB)) <= 4096),
			PRIMARY KEY(chat_id,message_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS message_button_actions(
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			button_index INTEGER NOT NULL CHECK(button_index BETWEEN 0 AND 2),
			button_id TEXT NOT NULL CHECK(length(CAST(button_id AS BLOB)) BETWEEN 1 AND 256),
			PRIMARY KEY(chat_id,message_id,button_index),
			UNIQUE(chat_id,message_id,button_id),
			FOREIGN KEY(message_id,chat_id) REFERENCES messages(id,chat_id) ON DELETE CASCADE);
		CREATE TABLE IF NOT EXISTS chat_organization(
			chat_id TEXT PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
			archived INTEGER NOT NULL DEFAULT 0, pinned INTEGER NOT NULL DEFAULT 0,
			muted INTEGER NOT NULL DEFAULT 0, unread_mark INTEGER NOT NULL DEFAULT 0,
			favorite INTEGER NOT NULL DEFAULT 0, list_ids TEXT NOT NULL DEFAULT '[]',
			provenance TEXT NOT NULL DEFAULT 'local_only');
		CREATE TABLE IF NOT EXISTS message_actions(
			client_id TEXT PRIMARY KEY, chat_id TEXT NOT NULL, target_id TEXT NOT NULL,
			method TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()), actor_id TEXT NOT NULL DEFAULT '',
			dispatched_at_ms INTEGER NOT NULL DEFAULT 0);
		CREATE TABLE IF NOT EXISTS poll_vote_dispatches(
			sequence INTEGER PRIMARY KEY, client_id TEXT NOT NULL UNIQUE,
			chat_id TEXT NOT NULL, message_id TEXT NOT NULL,
			FOREIGN KEY(client_id) REFERENCES message_actions(client_id) ON DELETE CASCADE);
		CREATE INDEX IF NOT EXISTS poll_vote_dispatch_target ON poll_vote_dispatches(chat_id,message_id,sequence);
		INSERT OR IGNORE INTO poll_vote_dispatches(client_id,chat_id,message_id)
			SELECT client_id,chat_id,target_id FROM message_actions
			WHERE method='poll.vote' AND status IN ('accepted','unknown','pending','dispatched') ORDER BY rowid;
		CREATE TABLE IF NOT EXISTS communities(
			id TEXT PRIMARY KEY, title TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS community_members(
			chat_id TEXT PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
			chat_title TEXT NOT NULL DEFAULT '',
			community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
			is_announcement INTEGER NULL);
		CREATE INDEX IF NOT EXISTS community_members_parent ON community_members(community_id,chat_id);
		CREATE TABLE IF NOT EXISTS community_sync_state(
			id INTEGER PRIMARY KEY CHECK(id=1), refreshed_at TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '');
		INSERT OR IGNORE INTO community_sync_state(id,refreshed_at,source) VALUES(1,'','');
		CREATE TABLE IF NOT EXISTS demo_groups(
			group_id TEXT PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
			description TEXT NOT NULL DEFAULT '' CHECK(length(CAST(description AS BLOB))<=2048),
			membership_state TEXT NOT NULL CHECK(membership_state IN ('joined','left')));
		CREATE TABLE IF NOT EXISTS demo_group_participants(
			group_id TEXT NOT NULL REFERENCES demo_groups(group_id) ON DELETE CASCADE,
			participant_id TEXT NOT NULL CHECK(participant_id IN ('demo:account','demo:participant:alex','demo:participant:maya','demo:participant:sam')),
			display_name TEXT NOT NULL CHECK(length(CAST(display_name AS BLOB))<=128),
			role TEXT NOT NULL CHECK(role IN ('owner','admin','member')),
			PRIMARY KEY(group_id,participant_id));`); err != nil {
		db.Close()
		return nil, err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{{"actor_id", "TEXT NOT NULL DEFAULT ''"}, {"dispatched_at_ms", "INTEGER NOT NULL DEFAULT 0"}} {
		var present int
		if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('message_actions') WHERE name=?", column.name).Scan(&present); err != nil {
			db.Close()
			return nil, err
		}
		if present == 0 {
			if _, err = db.Exec("ALTER TABLE message_actions ADD COLUMN " + column.name + " " + column.definition); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if _, err = db.Exec("UPDATE message_actions SET status='unknown' WHERE status IN ('pending','dispatched')"); err != nil {
		db.Close()
		return nil, err
	}
	// Reserved actions are persisted before dispatch. Since the transport only
	// runs after the reserved->dispatched transaction commits, a leftover
	// reservation proves that the external action was never called.
	if _, err = db.Exec("UPDATE message_actions SET status='not_dispatched' WHERE status='reserved'"); err != nil {
		db.Close()
		return nil, err
	}
	var communityChatTitleColumn int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('community_members') WHERE name='chat_title'").Scan(&communityChatTitleColumn); err != nil {
		db.Close()
		return nil, err
	}
	if communityChatTitleColumn == 0 {
		if _, err = db.Exec("ALTER TABLE community_members ADD COLUMN chat_title TEXT NOT NULL DEFAULT ''"); err != nil {
			db.Close()
			return nil, err
		}
	}
	var payloadColumn int
	if err = db.QueryRow("SELECT count(*) FROM pragma_table_info('attachments') WHERE name='payload'").Scan(&payloadColumn); err != nil {
		db.Close()
		return nil, err
	}
	if payloadColumn == 0 {
		if _, err = db.Exec("ALTER TABLE attachments ADD COLUMN payload BLOB"); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS media_library_order
		ON messages(timestamp DESC,chat_id ASC,id DESC)
		WHERE kind IN ('image','video','audio','document','gif','sticker')`); err != nil {
		db.Close()
		return nil, err
	}
	if demo {
		err = a.seed()
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// SetEncryptedStorageView records the vault-owned profile view created by the
// daemon bootstrap. A plain app.Open is deliberately not enough to assert that
// its data directory is encrypted.
func (a *App) SetEncryptedStorageView(ciphertextRoot, runtimeDir, profileRoot string, mountID int) error {
	if a == nil || mountID <= 0 {
		return fmt.Errorf("encrypted profile view is incomplete")
	}
	for _, path := range []string{ciphertextRoot, runtimeDir, profileRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("encrypted profile view contains an unsafe path")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("encrypted profile view directory is unsafe")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("encrypted profile view directory has a foreign owner")
		}
	}
	if filepath.Dir(profileRoot) != runtimeDir ||
		storagePathContains(ciphertextRoot, runtimeDir) ||
		storagePathContains(runtimeDir, ciphertextRoot) {
		return fmt.Errorf("encrypted profile view is not bound to the private runtime")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("create encrypted profile view identity")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if filepath.Clean(a.dir) != profileRoot {
		return fmt.Errorf("encrypted profile view does not contain this app's storage directory")
	}
	if a.demo || a.closing || a.closed || a.storageView != nil {
		return fmt.Errorf("encrypted profile view cannot be configured for this app")
	}
	a.storageView = map[string]any{
		"version": 1, "mode": "encrypted", "ciphertext_root": ciphertextRoot,
		"runtime_dir": runtimeDir, "profile_root": profileRoot,
		"view_id": hex.EncodeToString(token[:]), "mount_id": fmt.Sprintf("%d", mountID),
	}
	return nil
}

func storagePathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func (a *App) beginLifecycleLocked() {
	if a.lifecycleDone == nil {
		a.lifecycleDone = make(chan struct{})
	}
}

func (a *App) beginOperationLocked() {
	a.beginLifecycleLocked()
	a.activeOps++
}

func (a *App) endOperationLocked() {
	if a.activeOps > 0 {
		a.activeOps--
	}
	a.endLifecycleLocked()
}

func (a *App) endLifecycleLocked() {
	if a.connectAttempt == nil && !a.teardownActive && a.activeOps == 0 && a.lifecycleDone != nil {
		close(a.lifecycleDone)
		a.lifecycleDone = nil
	}
}

func (a *App) transitionStateLocked(state string) {
	if a.state != state {
		a.sessionEpoch++
	}
	a.state = state
}

func (a *App) runTeardown(transport Transport) {
	var closeErr error
	if transport != nil {
		if closer, ok := transport.(interface{ DisconnectWithError() error }); ok {
			closeErr = closer.DisconnectWithError()
		} else {
			transport.Disconnect()
		}
	}
	a.mu.Lock()
	a.transportCloseErr = closeErr
	a.teardownActive = false
	a.disconnectRequested = false
	if !a.closing {
		a.transitionStateLocked("disconnected")
		a.info = "Disconnected"
		a.qr = ""
		a.revision++
	}
	a.endLifecycleLocked()
	a.mu.Unlock()
}

func (a *App) runConnect(attempt *connectAttempt) {
	err := attempt.transport.Connect(attempt.ctx)
	a.mu.Lock()
	if a.connectAttempt != attempt {
		// There is no valid path that replaces an attempt, but do not let a
		// stale transport callback mutate lifecycle state if that invariant is
		// ever violated.
		a.mu.Unlock()
		return
	}
	a.connectAttempt = nil
	a.lastConnectCancel = attempt.cancel
	canceled := attempt.ctx.Err() != nil || a.disconnectRequested || a.closing
	if canceled {
		a.teardownActive = true
		a.transitionStateLocked("disconnecting")
		a.info = "Disconnecting"
		a.qr = ""
		transport := attempt.transport
		a.mu.Unlock()
		a.runTeardown(transport)
		return
	}
	if err != nil {
		// Recheck the attempt, context, and closing state while holding mu;
		// an obsolete canceled Connect must not publish a late error.
		a.transitionStateLocked("error")
		a.info = "Connection failed; retry explicitly"
		a.qr = ""
		a.revision++
	}
	a.endLifecycleLocked()
	a.mu.Unlock()
}

func (a *App) Close() error {
	a.mu.Lock()
	if a.closed {
		err := a.closeErr
		a.mu.Unlock()
		return err
	}
	if a.closing {
		done := a.closeDone
		a.mu.Unlock()
		<-done
		a.mu.Lock()
		err := a.closeErr
		a.mu.Unlock()
		return err
	}
	a.closing = true
	a.disconnectRequested = true
	if a.connectAttempt != nil {
		a.connectAttempt.cancel()
	}
	if a.lastConnectCancel != nil {
		a.lastConnectCancel()
	}
	var transport Transport
	teardownStarted := false
	if a.connectAttempt == nil && !a.teardownActive {
		a.beginLifecycleLocked()
		a.teardownActive = true
		teardownStarted = true
		transport = a.transport
	}
	lifecycleDone := a.lifecycleDone
	db := a.db
	a.mu.Unlock()

	if teardownStarted {
		// A nil transport still gets an ordered lifecycle completion. Only the
		// Close call that acquired teardown ownership may start it.
		go a.runTeardown(transport)
	}
	if lifecycleDone != nil {
		<-lifecycleDone
	}
	dbErr := db.Close()
	a.mu.Lock()
	err := errors.Join(a.transportCloseErr, dbErr)
	a.closeErr = err
	a.closed = true
	close(a.closeDone)
	a.mu.Unlock()
	return err
}

func (a *App) SetTransport(t Transport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closing && !a.closed {
		a.transport = t
		a.sessionEpoch++
	}
}
func (a *App) SetStatus(state, info, qr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing || a.closed {
		return
	}
	a.transitionStateLocked(state)
	a.info = info
	a.qr = qr
	a.revision++
}

// SetStatusContext commits a transport-originated status only while its
// callback context is still live. The context check is deliberately inside
// the App mutex: checking before acquiring the mutex would allow a canceled
// stale callback to publish after a newer session has taken ownership.
func (a *App) SetStatusContext(ctx context.Context, state, info, qr string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing || a.closed || ctx == nil || ctx.Err() != nil {
		return false
	}
	a.transitionStateLocked(state)
	a.info = info
	a.qr = qr
	a.revision++
	return true
}

func messageTitle(title, sender string) string {
	title = strings.TrimSpace(title)
	if title != "" && !strings.Contains(title, "@") && !isNumericJID(title) {
		return title
	}
	if sender = strings.TrimSpace(sender); sender != "" && !strings.Contains(sender, "@") && !isNumericJID(sender) {
		return sender
	}
	return "Unknown contact"
}

func isNumericJID(value string) bool {
	value = strings.TrimSpace(value)
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a *App) Record(ctx context.Context, m Message, title string) error {
	return a.recordMessage(ctx, m, title, false)
}

// RecordLive is exclusively for positively classified non-history SDK events.
func (a *App) RecordLive(ctx context.Context, m Message, title string) error {
	return a.recordMessage(ctx, m, title, true)
}

func (a *App) recordMessage(ctx context.Context, m Message, title string, liveInbound bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx == nil || ctx.Err() != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return sql.ErrConnDone
	}
	title = messageTitle(title, m.SenderName)
	runes := []rune(title)
	initials := "?"
	if len(runes) > 0 {
		initials = string(runes[0])
	}
	tx, e := a.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var alreadyStored bool
	if liveInbound && !a.demo && !m.Outgoing {
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages WHERE chat_id=? AND id=?)", m.ChatID, m.ID).Scan(&alreadyStored); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,'#54656f',?)", m.ChatID, title, initials, strings.HasSuffix(m.ChatID, "@g.us"))
	if e != nil {
		return e
	}
	var currentTitle string
	if e = tx.QueryRowContext(ctx, "SELECT title FROM chats WHERE id=?", m.ChatID).Scan(&currentTitle); e != nil {
		return e
	}
	if currentTitle == "Unknown contact" || isNumericJID(currentTitle) || strings.Contains(currentTitle, "@") {
		if _, e = tx.ExecContext(ctx, "UPDATE chats SET title=?,initials=? WHERE id=?", title, initials, m.ChatID); e != nil {
			return e
		}
	}
	if m.ContactCards != nil {
		normalized, normalizeErr := normalizeContactCards(*m.ContactCards)
		if normalizeErr != nil || m.Kind != "contact" {
			return contactVCardError()
		}
		m.ContactCards = &normalized
	}
	if (m.Location != nil && (m.Kind != "location" || !LocationProjectionValid(*m.Location))) || (m.Location == nil && m.Kind == "location") {
		return fail("invalid_location", "Location projection is invalid or does not match the message kind")
	}
	if m.Poll != nil && (m.Kind != "poll" || !PollProjectionValid(*m.Poll)) {
		return fail("invalid_poll", "Poll projection is invalid or does not match the message kind")
	}
	if (m.PollResults != nil && (m.Kind != "poll_results" || !PollResultsProjectionValid(*m.PollResults))) || (m.PollResults == nil && m.Kind == "poll_results") {
		return fail("invalid_poll_results", "Poll results projection is invalid or does not match the message kind")
	}
	if (m.List != nil && (m.Kind != "list" || !ListProjectionValid(*m.List))) || (m.List == nil && m.Kind == "list") {
		return fail("invalid_list", "List projection is invalid or does not match the message kind")
	}
	if m.ListActionRows != nil && len(*m.ListActionRows) > 0 && (m.List == nil || m.Kind != "list" || m.Outgoing || !ListActionRowsValid(*m.List, *m.ListActionRows)) {
		return fail("invalid_list_actions", "List action mappings are invalid or do not match an incoming list")
	}
	if (m.Buttons != nil && (m.Kind != "buttons" || !ButtonsProjectionValid(*m.Buttons))) || (m.Buttons == nil && m.Kind == "buttons") {
		return fail("invalid_buttons", "Buttons projection is invalid or does not match message kind")
	}
	if m.ButtonActionRows != nil && len(*m.ButtonActionRows) > 0 &&
		(m.Buttons == nil || m.Kind != "buttons" || m.Outgoing || !ButtonActionRowsValid(*m.Buttons, *m.ButtonActionRows)) {
		return fail("invalid_button_actions", "Button action mappings are invalid or do not match an incoming ButtonsMessage")
	}
	if (m.Interactive != nil && (m.Kind != "interactive" || !TextCardProjectionValid(*m.Interactive) || len(m.Interactive.Labels) != 0)) ||
		(m.Template != nil && (m.Kind != "template" || !TextCardProjectionValid(*m.Template))) ||
		(m.Kind == "interactive" && m.Interactive == nil) || (m.Kind == "template" && m.Template == nil) {
		return fail("invalid_text_card", "Text card projection is invalid or does not match message kind")
	}
	if m.LinkPreview != nil && (m.Kind != "text" || !LinkPreviewProjectionValid(*m.LinkPreview)) {
		return fail("invalid_link_preview", "Link preview projection is invalid or does not match a text message")
	}
	card := m.Interactive
	if m.Kind == "template" {
		card = m.Template
	}
	if card != nil {
		m.Body = card.fallback()
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id,chat_id) DO UPDATE SET body=CASE WHEN excluded.body<>'' OR (messages.body='[Unsupported content — open the official app]' AND excluded.kind NOT IN ('','unsupported')) THEN excluded.body ELSE messages.body END,kind=CASE WHEN excluded.kind<>'' THEN excluded.kind ELSE messages.kind END,sender_name=CASE WHEN excluded.sender_name<>'' THEN excluded.sender_name ELSE messages.sender_name END,sender_id=CASE WHEN excluded.sender_id<>'' THEN excluded.sender_id ELSE messages.sender_id END,quoted_body=CASE WHEN excluded.quoted_body<>'' THEN excluded.quoted_body ELSE messages.quoted_body END,quoted_sender=CASE WHEN excluded.quoted_sender<>'' THEN excluded.quoted_sender ELSE messages.quoted_sender END,status=CASE WHEN messages.status IN ('read','delivered') THEN messages.status ELSE excluded.status END`, m.ID, m.ChatID, m.Body, m.Timestamp, m.Outgoing, m.Status, m.Kind, m.SenderName, m.SenderID, m.QuotedBody, m.QuotedSender)
	if e != nil {
		return e
	}
	if card != nil {
		encoded, err := json.Marshal(card)
		if err != nil {
			return err
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_text_cards(chat_id,message_id,kind,projection) VALUES(?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET kind=excluded.kind,projection=excluded.projection`, m.ChatID, m.ID, m.Kind, string(encoded)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_text_cards WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.LinkPreview != nil {
		projection, marshalErr := json.Marshal(*m.LinkPreview)
		if marshalErr != nil {
			return marshalErr
		}
		if len(projection) > LinkPreviewProjectionMaxBytes {
			return fail("invalid_link_preview", "Link preview projection exceeds the safe storage limit")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_link_previews(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_link_previews WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.ContactCards != nil {
		projection, marshalErr := json.Marshal(*m.ContactCards)
		if marshalErr != nil {
			return marshalErr
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_contacts(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	}
	if m.Location != nil {
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_locations(chat_id,message_id,latitude,longitude,is_live,caption,name,address)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET
			latitude=excluded.latitude,longitude=excluded.longitude,is_live=excluded.is_live,
			caption=excluded.caption,name=excluded.name,address=excluded.address`,
			m.ChatID, m.ID, m.Location.Latitude, m.Location.Longitude, m.Location.IsLive,
			m.Location.Caption, m.Location.Name, m.Location.Address); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_locations WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.Poll != nil {
		projection, marshalErr := json.Marshal(*m.Poll)
		if marshalErr != nil {
			return marshalErr
		}
		if len(projection) > PollProjectionMaxBytes {
			return fail("invalid_poll", "Poll projection exceeds the safe storage limit")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_polls(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_polls WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.PollResults != nil {
		projection, marshalErr := json.Marshal(*m.PollResults)
		if marshalErr != nil {
			return marshalErr
		}
		if len(projection) > PollResultsProjectionMaxBytes {
			return fail("invalid_poll_results", "Poll results projection exceeds the safe storage limit")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_poll_results(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_poll_results WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.List != nil {
		projection, marshalErr := json.Marshal(*m.List)
		if marshalErr != nil {
			return marshalErr
		}
		if len(projection) > ListProjectionMaxBytes {
			return fail("invalid_list", "List projection exceeds the safe storage limit")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_lists(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_lists WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM message_list_actions WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.ListActionRows != nil {
		for _, action := range *m.ListActionRows {
			if _, e = tx.ExecContext(ctx, "INSERT INTO message_list_actions(chat_id,message_id,section_index,row_index,row_id) VALUES(?,?,?,?,?)", m.ChatID, m.ID, action.SectionIndex, action.RowIndex, action.RowID); e != nil {
				return e
			}
		}
	}
	if m.Buttons != nil {
		projection, marshalErr := json.Marshal(*m.Buttons)
		if marshalErr != nil {
			return marshalErr
		}
		if len(projection) > ButtonsProjectionMaxBytes {
			return fail("invalid_buttons", "Buttons projection exceeds the safe storage limit")
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO message_buttons(chat_id,message_id,projection) VALUES(?,?,?)
			ON CONFLICT(chat_id,message_id) DO UPDATE SET projection=excluded.projection`, m.ChatID, m.ID, string(projection)); e != nil {
			return e
		}
	} else if _, e = tx.ExecContext(ctx, "DELETE FROM message_buttons WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
		return e
	}
	if m.ButtonActionRows != nil {
		if _, e = tx.ExecContext(ctx, "DELETE FROM message_button_actions WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
			return e
		}
		for _, action := range *m.ButtonActionRows {
			if _, e = tx.ExecContext(ctx, "INSERT INTO message_button_actions(chat_id,message_id,button_index,button_id) VALUES(?,?,?,?)", m.ChatID, m.ID, action.ButtonIndex, action.ButtonID); e != nil {
				return e
			}
		}
	} else if m.Kind != "buttons" || m.Buttons == nil {
		if _, e = tx.ExecContext(ctx, "DELETE FROM message_button_actions WHERE chat_id=? AND message_id=?", m.ChatID, m.ID); e != nil {
			return e
		}
	}
	if m.Attachment != nil {
		encoded, marshalErr := json.Marshal(m.Attachment)
		if marshalErr != nil {
			return marshalErr
		}
		var payload any
		if m.MediaPayload != "" {
			payload = []byte(m.MediaPayload)
		}
		// A later server re-delivery is authoritative for typed content, but it
		// must not turn a private downloaded copy back into a remote attachment.
		var previous string
		preserveLocal := false
		var old Attachment
		if scanErr := tx.QueryRowContext(ctx, "SELECT meta FROM attachments WHERE chat_id=? AND message_id=?", m.ChatID, m.ID).Scan(&previous); scanErr == nil {
			if json.Unmarshal([]byte(previous), &old) == nil && old.State == "ready" && isLocalFileURL(old.URL) {
				preserveLocal = true
				// Keep the fresh server descriptor while retaining the private
				// URL and ready state of the already downloaded bytes.
				if m.Attachment.Name == "" {
					m.Attachment.Name = old.Name
				}
				m.Attachment.URL, m.Attachment.State, m.Attachment.Error = old.URL, "ready", ""
				encoded, marshalErr = json.Marshal(m.Attachment)
				if marshalErr != nil {
					return marshalErr
				}
			}
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		if preserveLocal {
			_, e = tx.ExecContext(ctx, "UPDATE attachments SET meta=?,payload=COALESCE(?,payload) WHERE chat_id=? AND message_id=?", string(encoded), payload, m.ChatID, m.ID)
		} else {
			_, e = tx.ExecContext(ctx, `INSERT INTO attachments(chat_id,message_id,meta,payload) VALUES(?,?,?,?) ON CONFLICT(chat_id,message_id) DO UPDATE SET meta=excluded.meta,payload=COALESCE(excluded.payload,attachments.payload)`, m.ChatID, m.ID, string(encoded), payload)
		}
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(); e == nil {
		a.revision++
		if liveInbound && !a.demo && !m.Outgoing && !alreadyStored {
			a.rememberNotification(m.ChatID, m.ID)
		}
	}
	return e
}
func (a *App) Receipt(ctx context.Context, chat string, ids []string, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || ctx == nil || ctx.Err() != nil {
		return
	}
	for _, id := range ids {
		a.db.ExecContext(ctx, `UPDATE messages SET status=CASE
			WHEN status='read' THEN 'read'
			WHEN status='delivered' AND ? <> 'read' THEN 'delivered'
			ELSE ?
		END WHERE id=? AND chat_id=? AND outgoing=1`, status, status, id, chat)
	}
	a.revision++
}
func decode(raw json.RawMessage, p any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(p); e != nil {
		return fail("invalid_params", "Invalid parameters")
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return fail("invalid_params", "Invalid parameters")
	}
	return nil
}
func (a *App) chatExists(ctx context.Context, id string) error {
	var n int
	if e := a.db.QueryRowContext(ctx, "SELECT count(*) FROM chats WHERE id=?", id).Scan(&n); e != nil {
		return e
	}
	if n == 0 {
		return fail("not_found", "Chat not found")
	}
	return nil
}
func validSelfJID(value string) (string, error) {
	jid, err := types.ParseJID(strings.TrimSpace(value))
	if err != nil {
		return "", fail("invalid_account", "The connected account has an invalid user ID")
	}
	jid = jid.ToNonAD()
	if jid.User == "" || !isNumericJID(jid.User) {
		return "", fail("invalid_account", "The connected account is not a user account")
	}
	switch jid.Server {
	case types.DefaultUserServer, types.LegacyUserServer, types.HiddenUserServer, types.HostedServer, types.HostedLIDServer:
		return jid.String(), nil
	default:
		return "", fail("invalid_account", "The connected account is not a user account")
	}
}

// selfChat creates only conversation metadata. The account provider is the
// sole source of the target in live mode; callers never provide a phone/JID.
// The caller holds a.mu.
func (a *App) selfChat(ctx context.Context) (any, error) {
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	selfID := "demo:account"
	if !a.demo {
		if a.state != "connected" || a.transport == nil {
			return nil, fail("disconnected", "Connect WhatsApp before opening your self chat")
		}
		provider, ok := a.transport.(AccountProvider)
		if !ok {
			return nil, fail("unsupported", "The connected transport cannot identify this account")
		}
		a.mu.Unlock()
		account, err := provider.Account(ctx)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		if err != nil {
			return nil, fail("account_unavailable", "The connected account could not be verified")
		}
		if !account.Available || account.State == "disconnected" || account.State == "closing" {
			return nil, fail("disconnected", "Connect WhatsApp before opening your self chat")
		}
		selfID, err = validSelfJID(account.JID)
		if err != nil {
			return nil, err
		}
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM chats WHERE id=?", selfID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		if _, err = tx.ExecContext(ctx, "INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,'#54656f',0)", selfID, "(You)", "Y"); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		a.revision++
	} else if err = tx.Commit(); err != nil {
		return nil, err
	}
	var chat Chat
	err = a.db.QueryRowContext(ctx, `SELECT c.id,c.title,c.initials,c.color,c.is_group,
		COALESCE((SELECT substr(body,1,120) FROM messages WHERE chat_id=c.id ORDER BY timestamp DESC,id DESC LIMIT 1),''),
		COALESCE((SELECT MAX(timestamp) FROM messages WHERE chat_id=c.id),0)
		FROM chats c WHERE c.id=?`, selfID).Scan(&chat.ID, &chat.Title, &chat.Initials, &chat.Color, &chat.IsGroup, &chat.Preview, &chat.Timestamp)
	if err != nil {
		return nil, err
	}
	var unread int
	if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE chat_id=? AND outgoing=0 AND status NOT IN ('read','delivered')", selfID).Scan(&unread); err != nil {
		return nil, err
	}
	chat.Unread = unread
	return map[string]any{"chat": chat}, nil
}

func (a *App) syncHistory(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string `json:"chat_id"`
	}
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	if e := a.chatExists(ctx, p.ChatID); e != nil {
		return nil, e
	}
	if a.demo || a.transport == nil || a.state != "connected" {
		return nil, fail("disconnected", "Connect WhatsApp before requesting history")
	}
	requester, ok := a.transport.(HistoryRequester)
	if !ok {
		return nil, fail("unsupported", "History refresh is unavailable in this profile")
	}
	if !a.historyMu.TryLock() {
		return nil, fail("busy", "A history refresh is already in progress")
	}
	defer a.historyMu.Unlock()
	var anchor HistoryAnchor
	if e := a.db.QueryRowContext(ctx, "SELECT id,timestamp,outgoing FROM messages WHERE chat_id=? ORDER BY timestamp ASC,id ASC LIMIT 1", p.ChatID).Scan(&anchor.ID, &anchor.Timestamp, &anchor.Outgoing); e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return nil, fail("not_found", "History refresh needs a cached message anchor")
		}
		return nil, e
	}
	anchor.ChatID = p.ChatID
	a.beginOperationLocked()
	defer a.endOperationLocked()
	a.mu.Unlock()
	e := requester.RequestHistory(ctx, p.ChatID, anchor, 50)
	a.mu.Lock()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if e != nil {
		return nil, fail("history_request_failed", "History refresh could not be requested")
	}
	// The primary device returns messages before the anchor asynchronously;
	// completion is reported only by genuine history events handled by Record.
	return map[string]any{"chat_id": p.ChatID, "requested": true, "completed": false, "count": 50, "anchor_id": anchor.ID, "anchor_timestamp": anchor.Timestamp}, nil
}

func (a *App) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx = ensureMediaHydrationScope(ctx)
	switch method {
	case "notification.poll":
		return a.pollNotifications(ctx, raw)
	case "app.capabilities":
		if e := decode(raw, &struct{}{}); e != nil {
			return nil, e
		}
		return a.capabilities(), nil
	case "poll.vote":
		return a.pollVoteMessage(ctx, raw)
	case "message.reply":
		return a.messageReply(ctx, raw)
	case "list.reply":
		return a.messageListReply(ctx, raw)
	case "button.reply":
		return a.messageButtonReply(ctx, raw)
	case "message.react":
		return a.messageReact(ctx, raw)
	case "message.edit":
		return a.messageEdit(ctx, raw)
	case "message.delete":
		return a.messageDelete(ctx, raw)
	case "message.forward":
		return a.messageForward(ctx, raw)
	case "message.star", "message.pin":
		return a.messageFlag(ctx, method, raw)
	case "chat.update":
		return a.chatUpdate(ctx, raw)
	case "group.create":
		return a.createDemoGroup(ctx, raw)
	case "group.update":
		return a.updateDemoGroup(ctx, raw)
	case "group.participants":
		return a.manageDemoGroupParticipants(ctx, raw)
	case "group.join":
		return a.changeDemoGroupMembership(ctx, raw, "joined", method)
	case "group.leave":
		return a.changeDemoGroupMembership(ctx, raw, "left", method)
	case "chat.open":
		return a.chatOpen(ctx, raw)
	case "contact.list":
		return a.contactList(ctx, raw)
	case "contact.prepare":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		result, err := a.contactPrepare(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "message.contact.send":
		return a.contactSend(ctx, raw)
	case "message.location.send":
		return a.locationSend(ctx, raw)
	case "history.sync":
		return a.syncHistory(ctx, raw)
	case "gif.providers":
		return gifProviders(), nil
	case "gif.search":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		result, err := a.gifSearch(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "gif.prepare":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		result, err := a.gifPrepare(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "attachment.prepare":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		// prepare performs bounded CPU/file work and does not hold App.mu.
		a.mu.Unlock()
		result, err := a.prepare(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "attachment.album.prepare":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		result, err := a.prepareAlbum(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			if prepared, ok := result.(AlbumPrepareResult); ok {
				paths := make([]string, 0, len(prepared.Items))
				for _, item := range prepared.Items {
					paths = append(paths, item.PreparedPath)
				}
				a.releasePreparedPaths(paths)
			}
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "attachment.album.release":
		return a.releaseAlbum(raw)
	case "attachment.inspect":
		return a.inspect(raw)
	case "message.attach":
		return a.attach(ctx, raw)
	case "message.album.send":
		a.beginOperationLocked()
		defer a.endOperationLocked()
		a.mu.Unlock()
		result, err := a.sendAlbum(ctx, raw)
		a.mu.Lock()
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		return result, err
	case "message.download":
		return a.download(ctx, raw)
	case "account.get":
		if a.demo {
			return Account{Available: true, DisplayName: "Demo Account", Phone: "+1 555 0100", JID: "demo:account", Username: "demo", About: "Synthetic local identity", AvatarURL: "", State: "demo"}, nil
		}
		provider, ok := a.transport.(AccountProvider)
		state := a.state
		if !ok {
			return Account{Available: false, State: state}, nil
		}
		a.mu.Unlock()
		account, err := provider.Account(ctx)
		a.mu.Lock()
		if err != nil {
			return Account{Available: false, State: state}, nil
		}
		return account, nil
	case "message.search":
		return a.searchMessages(ctx, raw)
	case "message.context":
		return a.messageContext(ctx, raw)
	case "community.list":
		return a.communityListLocked(ctx, raw)
	case "community.info":
		return a.communityInfoLocked(ctx, raw)
	case "community.refresh":
		return a.communityRefreshLocked(ctx, raw)
	case "app.status":
		mode := "live"
		if a.demo {
			mode = "demo"
		}
		status := map[string]any{"mode": mode, "state": a.state, "message": a.info, "qr_png": a.qr, "revision": a.revision}
		if a.storageView != nil {
			view := make(map[string]any, len(a.storageView))
			for key, value := range a.storageView {
				view[key] = value
			}
			status["storage_view"] = view
		}
		return status, nil
	case "chat.self":
		if e := decode(raw, &struct{}{}); e != nil {
			return nil, e
		}
		return a.selfChat(ctx)
	case "chat.list":
		var p struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		}
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		if utf8.RuneCountInString(p.Query) > 128 {
			return nil, fail("invalid_params", "Chat search query is too long")
		}
		if p.Limit <= 0 {
			p.Limit = 1000
		}
		if p.Limit > 1000 {
			p.Limit = 1000
		}
		cursor, e := decodeChatListCursor(p.Cursor)
		if e != nil {
			return nil, e
		}
		hasCursor := 0
		if p.Cursor != "" {
			hasCursor = 1
		}
		rows, e := a.db.QueryContext(ctx, `WITH loaded AS (
			SELECT c.id,c.title,c.initials,c.color,c.is_group,
			COALESCE((SELECT substr(body,1,120) FROM messages WHERE chat_id=c.id ORDER BY timestamp DESC,id DESC LIMIT 1),'') AS preview,
			COALESCE((SELECT MAX(timestamp) FROM messages WHERE chat_id=c.id),0) AS chat_timestamp
			FROM chats c
			WHERE (?='' OR instr(lower(c.title),lower(?))>0 OR EXISTS(SELECT 1 FROM community_members cm JOIN communities co ON co.id=cm.community_id WHERE cm.chat_id=c.id AND instr(lower(co.title),lower(?))>0))
		)
		SELECT id,title,initials,color,is_group,preview,chat_timestamp FROM loaded
		WHERE (?=0 OR chat_timestamp<? OR (chat_timestamp=? AND id>?))
		ORDER BY chat_timestamp DESC,id ASC LIMIT ?`, p.Query, p.Query, p.Query, hasCursor, cursor.timestamp, cursor.timestamp, cursor.chatID, p.Limit+1)
		if e != nil {
			return nil, e
		}
		chats := []Chat{}
		for rows.Next() {
			var c Chat
			if e = rows.Scan(&c.ID, &c.Title, &c.Initials, &c.Color, &c.IsGroup, &c.Preview, &c.Timestamp); e != nil {
				rows.Close()
				return nil, e
			}
			chats = append(chats, c)
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return nil, e
		}
		if e = rows.Close(); e != nil {
			return nil, e
		}
		hasMore := len(chats) > p.Limit
		if hasMore {
			chats = chats[:p.Limit]
		}
		if e = a.fillChatOrganization(ctx, chats); e != nil {
			return nil, e
		}
		if e = a.fillChatCommunity(ctx, chats); e != nil {
			return nil, e
		}
		for {
			response := map[string]any{"chats": chats, "revision": a.revision, "has_more": hasMore, "next_cursor": ""}
			if hasMore && len(chats) > 0 {
				last := chats[len(chats)-1]
				response["next_cursor"] = encodeChatListCursor(chatListCursor{timestamp: last.Timestamp, chatID: last.ID})
			}
			encoded, marshalErr := json.Marshal(response)
			if marshalErr != nil {
				return nil, marshalErr
			}
			if len(encoded) <= pageResultBudget {
				return response, nil
			}
			if len(chats) <= 1 {
				return nil, fail("response_too_large", "A chat entry exceeds the safe response budget")
			}
			chats = chats[:len(chats)-1]
			hasMore = true
		}
	case "media.list":
		return a.mediaList(ctx, raw)
	case "media.library":
		return a.mediaLibrary(ctx, raw)
	case "chat.info":
		return a.chatInfo(ctx, raw)
	case "message.list":
		var p struct {
			ChatID string  `json:"chat_id"`
			Limit  int     `json:"limit"`
			Before *Cursor `json:"before"`
		}
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		if e := a.chatExists(ctx, p.ChatID); e != nil {
			return nil, e
		}
		if p.Limit <= 0 {
			p.Limit = 50
		}
		if p.Limit > 100 {
			p.Limit = 100
		}
		query := "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE chat_id=?"
		args := []any{p.ChatID}
		if p.Before != nil {
			query += " AND (timestamp<? OR (timestamp=? AND id<?))"
			args = append(args, p.Before.Timestamp, p.Before.Timestamp, p.Before.ID)
		}
		query += " ORDER BY timestamp DESC,id DESC LIMIT ?"
		args = append(args, p.Limit+1)
		rows, e := a.db.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		candidates := []Message{}
		rawRecords := [][]byte{}
		more := false
		for rows.Next() {
			var m Message
			if e = rows.Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender); e != nil {
				return nil, e
			}
			// The extra SQL row is only a sentinel. Do not collect or marshal it.
			if len(candidates) >= p.Limit {
				more = true
				break
			}
			encoded, marshalErr := json.Marshal(m)
			if marshalErr != nil {
				return nil, marshalErr
			}
			added := len(encoded)
			if len(rawRecords) > 0 {
				added++
			}
			// This unhydrated encoding is a conservative lower bound. Hydrated
			// attachment metadata can only make the eventual record larger.
			if pageResultSize(rawRecords)+added > pageResultBudget {
				if len(candidates) == 0 {
					return nil, messageTooLarge()
				}
				more = true
				break
			}
			candidates = append(candidates, m)
			rawRecords = append(rawRecords, encoded)
		}
		if e = rows.Err(); e != nil {
			return nil, e
		}
		rows.Close()

		// Hydrate before accounting: attachment metadata is part of the result.
		if e = a.fillAttachments(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillMessageState(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillContactCards(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillLocationProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillPollProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillVotingResults(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillPollSelfVotes(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillListProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillTextCardProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillLinkPreviewProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		if e = a.fillButtonsProjections(ctx, p.ChatID, candidates); e != nil {
			return nil, e
		}
		msgs := make([]Message, 0, len(candidates))
		used := pageResultSize(nil)
		for _, m := range candidates {
			encoded, marshalErr := json.Marshal(m)
			if marshalErr != nil {
				return nil, marshalErr
			}
			added := len(encoded)
			if len(msgs) > 0 {
				added++
			}
			if used+added > pageResultBudget {
				if len(msgs) == 0 {
					return nil, messageTooLarge()
				}
				more = true
				break
			}
			used += added
			msgs = append(msgs, m)
		}
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		return map[string]any{"messages": msgs, "has_more": more}, nil
	case "draft.get", "draft.set":
		var p struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		if e := a.chatExists(ctx, p.ChatID); e != nil {
			return nil, e
		}
		if utf8.RuneCountInString(p.Text) > 16000 {
			return nil, fail("invalid_params", "Draft exceeds 16000 characters")
		}
		if method == "draft.set" {
			_, e := a.db.ExecContext(ctx, "INSERT INTO drafts(chat_id,text) VALUES(?,?) ON CONFLICT(chat_id) DO UPDATE SET text=excluded.text", p.ChatID, p.Text)
			return map[string]any{"saved": e == nil}, e
		}
		var text string
		e := a.db.QueryRowContext(ctx, "SELECT text FROM drafts WHERE chat_id=?", p.ChatID).Scan(&text)
		if errors.Is(e, sql.ErrNoRows) {
			e = nil
		}
		return map[string]any{"text": text}, e
	case "message.send":
		var p struct {
			ChatID   string `json:"chat_id"`
			Text     string `json:"text"`
			ClientID string `json:"client_id"`
		}
		if e := decode(raw, &p); e != nil {
			return nil, e
		}
		if e := a.chatExists(ctx, p.ChatID); e != nil {
			return nil, e
		}
		if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(p.Text) > 16000 || p.ClientID == "" || len(p.ClientID) > 128 {
			return nil, fail("invalid_params", "Message or client ID is invalid")
		}
		var m Message
		e := a.db.QueryRowContext(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE client_id=?", p.ClientID).Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender)
		if e == nil {
			if m.ChatID != p.ChatID || m.Body != p.Text {
				return nil, fail("conflict", "Client ID already used for another message")
			}
			return map[string]any{"message": m}, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if !a.demo && (a.transport == nil || a.state != "connected") {
			return nil, fail("disconnected", "Connect WhatsApp before sending")
		}
		status := "demo_sent"
		if !a.demo {
			status = "queued"
		}
		m = Message{ID: p.ClientID, ChatID: p.ChatID, Body: p.Text, Timestamp: time.Now().UnixMilli(), Outgoing: true, Status: status, Kind: "text"}
		_, e = a.db.ExecContext(ctx, "INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status,client_id,kind) VALUES(?,?,?,?,?,?,?,?)", m.ID, m.ChatID, m.Body, m.Timestamp, true, m.Status, p.ClientID, m.Kind)
		if e != nil {
			return nil, e
		}
		a.revision++
		if !a.demo {
			a.db.ExecContext(ctx, "UPDATE messages SET status='sending' WHERE id=? AND chat_id=?", m.ID, m.ChatID)
			transport := a.transport
			a.mu.Unlock()
			sent, e := transport.Send(ctx, m.ChatID, m.Body, m.ID)
			a.mu.Lock()
			completionStatus := "accepted"
			if e != nil {
				completionStatus = "unknown"
			} else if sent > 0 {
				m.Timestamp = sent
			}
			if _, saveErr := a.db.ExecContext(ctx, `UPDATE messages
				SET status=CASE
					WHEN status='read' THEN 'read'
					WHEN status='delivered' THEN 'delivered'
					ELSE ?
				END,timestamp=? WHERE id=? AND chat_id=?`, completionStatus, m.Timestamp, m.ID, m.ChatID); saveErr != nil {
				return nil, fail("unknown_outcome", "Message may be accepted; check history before retrying")
			}
			if saveErr := a.db.QueryRowContext(ctx, "SELECT id,chat_id,body,timestamp,outgoing,status,kind,sender_name,sender_id,quoted_body,quoted_sender FROM messages WHERE id=? AND chat_id=?", m.ID, m.ChatID).
				Scan(&m.ID, &m.ChatID, &m.Body, &m.Timestamp, &m.Outgoing, &m.Status, &m.Kind, &m.SenderName, &m.SenderID, &m.QuotedBody, &m.QuotedSender); saveErr != nil {
				return nil, fail("unknown_outcome", "Message may be accepted; check history before retrying")
			}
			a.revision++
		}
		return map[string]any{"message": m}, nil
	case "auth.connect":
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		if a.demo || a.transport == nil {
			return nil, fail("unsupported", "Live transport is not enabled")
		}
		if a.disconnectRequested || a.teardownActive {
			return nil, fail("lifecycle_busy", "Disconnect is still in progress; retry explicitly")
		}
		if a.connectAttempt != nil || a.state == "connecting" || a.state == "qr" || a.state == "connected" {
			return map[string]any{"started": true}, nil
		}
		if a.lastConnectCancel != nil {
			a.lastConnectCancel()
			a.lastConnectCancel = nil
		}
		a.transitionStateLocked("connecting")
		a.info = "Connecting to WhatsApp"
		a.qr = ""
		a.revision++
		run, cancel := context.WithCancel(context.Background())
		attempt := &connectAttempt{ctx: run, cancel: cancel, transport: a.transport}
		a.beginLifecycleLocked()
		a.connectAttempt = attempt
		go a.runConnect(attempt)
		return map[string]any{"started": true}, nil
	case "auth.disconnect":
		if a.closing || a.closed {
			return nil, fail("app_closing", "App is closing")
		}
		if a.demo {
			return nil, fail("unsupported", "Demo has no WhatsApp connection")
		}
		if a.disconnectRequested || a.teardownActive {
			if a.connectAttempt != nil {
				a.connectAttempt.cancel()
			}
			if a.lastConnectCancel != nil {
				a.lastConnectCancel()
			}
			a.transitionStateLocked("disconnecting")
			a.info = "Disconnecting"
			a.revision++
			return map[string]any{"disconnected": false, "in_progress": true}, nil
		}
		if a.connectAttempt != nil {
			a.disconnectRequested = true
			a.connectAttempt.cancel()
			if a.lastConnectCancel != nil {
				a.lastConnectCancel()
			}
			a.transitionStateLocked("disconnecting")
			a.info = "Disconnecting"
			a.qr = ""
			a.revision++
			return map[string]any{"disconnected": false, "in_progress": true}, nil
		}
		if a.lastConnectCancel != nil {
			a.lastConnectCancel()
		}
		a.beginLifecycleLocked()
		a.teardownActive = true
		a.transitionStateLocked("disconnecting")
		a.info = "Disconnecting"
		a.qr = ""
		a.revision++
		transport := a.transport
		go a.runTeardown(transport)
		return map[string]any{"disconnected": false, "in_progress": true}, nil
	case "auth.unlink", "auth.logout":
		return nil, fail("unsupported", "Remote unlink/logout is not implemented; no remote request or local session/history deletion was performed. Manage linked devices in WhatsApp.")
	default:
		return nil, fail("method_not_found", "Unknown method")
	}
}
func (a *App) seed() error {
	var n int
	if e := a.db.QueryRow("SELECT count(*) FROM chats").Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return nil
	}
	tx, e := a.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	fixtures := []Chat{{ID: "demo:alex", Title: "Alex Morgan", Initials: "AM", Color: "#526a63"}, {ID: "demo:maya", Title: "Maya Chen", Initials: "MC", Color: "#6b617b"}, {ID: "demo:studio", Title: "Design Studio", Initials: "DS", Color: "#626f8a", IsGroup: true}, {ID: "demo:notes", Title: "Notes to Self", Initials: "ME", Color: "#8a7357"}}
	bodies := [][]string{{"Morning! How is the native client coming along?", "The first slice is running locally. No browser involved.", "Nice. Let's make the small details feel right.", "Agreed — drafts, scrolling, and clear message states first.", "This conversation is synthetic demo data. Nothing is sent to WhatsApp."}, {"I've put together the references for the interface.", "Let's keep it familiar and make the interactions more predictable.", "Sounds good. Simple, fast, and native."}, {"Welcome to Design Studio. This is a synthetic group.", "Keep the conversation at the center of the design.", "The next review is about keyboard navigation and contrast."}, {"A quiet place for your notes.", "Demo messages and drafts are stored locally in SQLite."}}
	now := time.Now().UnixMilli()
	for k, c := range fixtures {
		if _, e = tx.Exec("INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,?,?)", c.ID, c.Title, c.Initials, c.Color, c.IsGroup); e != nil {
			return e
		}
		count := len(bodies[k])
		if k == 0 {
			count = 120
		}
		for i := 0; i < count; i++ {
			body := bodies[k][i%len(bodies[k])]
			if k == 0 && i == count-1 {
				body += "\nPreview fixture: https://example.invalid/native-demo"
			}
			out := i%3 == 1
			status := "demo_received"
			if out {
				status = "demo_sent"
			}
			if _, e = tx.Exec("INSERT INTO messages(id,chat_id,body,timestamp,outgoing,status) VALUES(?,?,?,?,?,?)", fmt.Sprintf("seed-%d-%03d", k, i), c.ID, body, now-int64(k)*3600000-int64(count-i)*60000, out, status); e != nil {
				return e
			}
			if k == 0 && i == count-1 {
				projection := LinkPreviewProjection{Site: "example.invalid", Title: "Native demo preview", Description: "Synthetic message metadata only. No page or thumbnail is fetched."}
				encoded, marshalErr := json.Marshal(projection)
				if marshalErr != nil {
					return marshalErr
				}
				if _, e = tx.Exec("UPDATE messages SET kind='text' WHERE id=? AND chat_id=?", fmt.Sprintf("seed-%d-%03d", k, i), c.ID); e != nil {
					return e
				}
				if _, e = tx.Exec("INSERT INTO message_link_previews(chat_id,message_id,projection) VALUES(?,?,?)", c.ID, fmt.Sprintf("seed-%d-%03d", k, i), string(encoded)); e != nil {
					return e
				}
			}
		}
	}
	// Demo grouping is explicitly synthetic and is stored only in demo mode.
	if _, e = tx.Exec("INSERT INTO communities(id,title) VALUES('demo:community','Neighborhood Circle')"); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO community_members(chat_id,chat_title,community_id,is_announcement) VALUES('demo:studio','Design Studio','demo:community',NULL)"); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE community_sync_state SET source='demo_synthetic',refreshed_at='' WHERE id=1"); e != nil {
		return e
	}
	return tx.Commit()
}
