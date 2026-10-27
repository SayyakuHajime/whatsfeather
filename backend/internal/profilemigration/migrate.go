package profilemigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	_ "modernc.org/sqlite"
)

const (
	MaxFileBytes          int64 = 256 << 20
	MaxProfileBytes       int64 = 1 << 30
	MaxProfileFiles             = 20000
	maxMediaMetadataBytes       = 64 << 10
)

var (
	ErrUnsafeSource         = errors.New("legacy profile is unsafe or changed during migration")
	ErrUnsupportedSource    = errors.New("legacy profile format is not supported for offline migration")
	ErrWrongSourceMode      = errors.New("migration source must be an explicitly marked live profile")
	ErrSQLiteSidecars       = errors.New("SQLite WAL or journal sidecars are unsupported; close the old application cleanly first")
	ErrSourceBusy           = errors.New("legacy profile is owned by another process")
	ErrSourceLockRequired   = errors.New("offline migration requires the cooperative profile owner lock to prove the normal writer is stopped")
	ErrSourceFileTooLarge   = errors.New("legacy profile contains a file above the 256 MiB migration limit")
	ErrSourceAggregateLimit = errors.New("legacy profile exceeds the 1 GiB aggregate migration limit")
	ErrSourceFileCountLimit = errors.New("legacy profile exceeds the 20,000-file migration limit")
	ErrSourceEntryLimit     = errors.New("legacy profile exceeds the bounded migration entry limit")
	ErrDestinationNotEmpty  = errors.New("encrypted destination is not a new empty profile")
	ErrMigrationInterrupted = errors.New("offline profile migration interrupted; destination remains inactive")
)

type MigrationPoint string

const (
	PointIncompletePublished MigrationPoint = "incomplete-published"
	PointFileCopied          MigrationPoint = "file-copied"
	PointBeforeComplete      MigrationPoint = "before-complete-publish"
	PointCompletePublished   MigrationPoint = "complete-published"
	PointFileCopyChunk       MigrationPoint = "file-copy-chunk"
)

type Options struct {
	// Failpoint is a test seam for process-kill acceptance; production passes nil.
	Failpoint func(MigrationPoint) error
}

type plannedEntry struct {
	rel     string
	isDir   bool
	size    int64
	mode    os.FileMode
	modTime int64
	device  uint64
	inode   uint64
	digest  string
}

type Plan struct {
	sourceRoot      string
	destinationRoot string
	rootDevice      uint64
	rootInode       uint64
	sourceIdentity  string
	treeDigest      string
	entries         []plannedEntry
	files           int64
	bytes           int64
	lock            *os.File
	closed          bool
}

// Prepare validates source mode, supported SQLite state, source/destination
// path separation, bounded tree types and ownership before the caller opens or
// initializes the encrypted destination. It only reads source data and holds
// the existing private profile.lock through completion; sources without the
// cooperative daemon-writer lock are rejected. This excludes compliant daemon
// writers, not arbitrary same-UID code that ignores the lock.
func Prepare(sourceRoot, destinationRoot string) (*Plan, error) {
	return PrepareContext(context.Background(), sourceRoot, destinationRoot)
}

// PrepareContext performs the read-only, bounded preflight with cancellation
// checks during source-tree hashing. The existing profile lock is held until
// the returned Plan is closed.
func PrepareContext(ctx context.Context, sourceRoot, destinationRoot string) (*Plan, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrMigrationInterrupted
	}
	source, err := normalizeRoot(sourceRoot)
	if err != nil {
		return nil, ErrUnsafeSource
	}
	destination, err := normalizeRoot(destinationRoot)
	if err != nil || overlaps(source, destination) {
		return nil, ErrUnsafeSource
	}
	if err := validateNoSymlinkPath(source); err != nil {
		return nil, ErrUnsafeSource
	}
	if err := validateNoSymlinkPath(destination); err != nil {
		return nil, ErrUnsafeSource
	}
	rootInfo, err := os.Lstat(source)
	if err != nil || !privateOwnedDirectory(rootInfo) {
		return nil, ErrUnsafeSource
	}
	if err := validateDestinationRoot(destination); err != nil {
		return nil, err
	}
	if err := rejectCipherRoot(source); err != nil {
		return nil, err
	}
	lock, err := acquireExistingProfileLock(source)
	if err != nil {
		return nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock && lock != nil {
			_ = releaseLock(lock)
		}
	}()
	if ctx.Err() != nil {
		return nil, ErrMigrationInterrupted
	}
	if err := rejectActiveSourceProcess(source); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrMigrationInterrupted
	}
	if err := validateSourceDatabasesContext(ctx, source); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrMigrationInterrupted
	}
	entries, files, total, digest, err := scanSourceContext(ctx, source)
	if err != nil {
		return nil, err
	}
	st, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || st.Dev == 0 || st.Ino == 0 {
		return nil, ErrUnsafeSource
	}
	plan := &Plan{
		sourceRoot: source, destinationRoot: destination,
		rootDevice: uint64(st.Dev), rootInode: st.Ino,
		sourceIdentity: pathIdentity(source), treeDigest: digest,
		entries: entries, files: files, bytes: total, lock: lock,
	}
	keepLock = true
	return plan, nil
}

// Close releases only the migration's source ownership lock. It never removes
// or rewrites a source file.
func (p *Plan) Close() error {
	if p == nil || p.closed {
		return nil
	}
	p.closed = true
	if p.lock != nil {
		return releaseLock(p.lock)
	}
	return nil
}

// Migrate copies a prepared, offline legacy live profile into an already-open
// distinct encrypted mount. Call MigrateWithIntent in production so outer vault
// intent is durable before any profile data is copied.
func (p *Plan) Migrate(ctx context.Context, mountRoot string, options Options) error {
	return p.MigrateWithIntent(ctx, mountRoot, options, nil)
}

// MigrateWithIntent copies profile data after publishing the destination-bound
// outer intent, then atomically publishes the inner complete marker last.
func (p *Plan) MigrateWithIntent(ctx context.Context, mountRoot string, options Options, publishIntent func() error) error {
	if p == nil || p.closed || ctx == nil {
		return ErrUnsafeSource
	}
	mount, err := normalizeRoot(mountRoot)
	if err != nil || overlaps(mount, p.sourceRoot) || overlaps(mount, p.destinationRoot) {
		return ErrUnsafeSource
	}
	if p.destinationRoot != "" && destinationIdentity(p.destinationRoot) == "" {
		return ErrUnsafeSource
	}
	if err := p.verifySourceStable(ctx); err != nil {
		return err
	}
	if err := validateMountRoot(mount); err != nil {
		return err
	}
	entries, err := os.ReadDir(mount)
	if err != nil || len(entries) != 0 {
		return ErrDestinationNotEmpty
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	if publishIntent != nil {
		if err := publishIntent(); err != nil {
			return fmt.Errorf("publish destination-bound migration intent: %w", err)
		}
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	state := migrationState{
		Format: stateFormat, Version: stateVersion, State: "copying",
		SourceIdentity: p.sourceIdentity, DestinationIdentity: destinationIdentity(p.destinationRoot),
		SourceDevice: p.rootDevice, SourceInode: p.rootInode,
	}
	if err := writeMigrationState(mount, state, true); err != nil {
		return ErrMigrationState
	}
	if err := trigger(options, PointIncompletePublished); err != nil {
		return ErrMigrationInterrupted
	}
	for _, entry := range p.entries {
		if err := ctx.Err(); err != nil {
			return ErrMigrationInterrupted
		}
		if err := p.copyEntry(ctx, mount, entry, options); err != nil {
			return err
		}
		if !entry.isDir {
			if err := trigger(options, PointFileCopied); err != nil {
				return ErrMigrationInterrupted
			}
		}
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
	}
	if err := p.verifySourceStable(ctx); err != nil {
		return err
	}
	if err := checkCopiedDatabasesContext(ctx, mount); err != nil {
		return err
	}
	if err := rebaseOwnedMediaContext(ctx, mount, p.sourceRoot); err != nil {
		return err
	}
	if err := checkCopiedDatabasesContext(ctx, mount); err != nil {
		return err
	}
	if err := syncCopiedTreeContext(ctx, mount); err != nil {
		if errors.Is(err, ErrMigrationInterrupted) {
			return ErrMigrationInterrupted
		}
		return ErrUnsafeSource
	}
	if err := trigger(options, PointBeforeComplete); err != nil {
		return ErrMigrationInterrupted
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	if err := p.verifySourceStable(ctx); err != nil {
		if errors.Is(err, ErrMigrationInterrupted) {
			return ErrMigrationInterrupted
		}
		return ErrUnsafeSource
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	state.State = "complete"
	state.SourceTreeSHA256 = p.treeDigest
	state.Files = p.files
	state.Bytes = p.bytes
	state.ModeVerified = "live"
	state.SQLiteQuickCheck = true
	state.MediaRebased = true
	if err := writeMigrationState(mount, state, false); err != nil {
		return ErrMigrationState
	}
	if err := trigger(options, PointCompletePublished); err != nil {
		return ErrMigrationInterrupted
	}
	return nil
}

func trigger(options Options, point MigrationPoint) error {
	if options.Failpoint == nil {
		return nil
	}
	return options.Failpoint(point)
}

func normalizeRoot(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return "", ErrUnsafeSource
	}
	return path, nil
}

func overlaps(a, b string) bool {
	return containsPath(a, b) || containsPath(b, a)
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func validateNoSymlinkPath(path string) error {
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
			return ErrUnsafeSource
		}
	}
	return nil
}

func validateDestinationRoot(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !privateOwnedDirectory(info) {
		return ErrDestinationNotEmpty
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return ErrDestinationNotEmpty
	}
	return nil
}

func validateMountRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !privateOwnedDirectory(info) {
		return ErrUnsafeSource
	}
	return nil
}

func privateOwnedDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

func privateOwnedRegular(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid() && st.Nlink == 1
}

func rejectCipherRoot(source string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return ErrUnsafeSource
	}
	for _, entry := range entries {
		switch entry.Name() {
		case ".profilevault.json", ".profilevault.lock", "cipher":
			return ErrUnsupportedSource
		}
	}
	return nil
}

func acquireExistingProfileLock(source string) (*os.File, error) {
	path := filepath.Join(source, "profile.lock")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, ErrSourceLockRequired
	}
	if err != nil {
		return nil, ErrUnsafeSource
	}
	file := os.NewFile(uintptr(fd), "legacy-profile-lock")
	st := &syscall.Stat_t{}
	if syscall.Fstat(fd, st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || st.Mode&0077 != 0 {
		_ = file.Close()
		return nil, ErrUnsafeSource
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrSourceBusy
		}
		return nil, ErrUnsafeSource
	}
	return file, nil
}

func releaseLock(file *os.File) error {
	fd := int(file.Fd())
	unlockErr := syscall.Flock(fd, syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil || closeErr != nil {
		return ErrUnsafeSource
	}
	return nil
}

func rejectActiveSourceProcess(source string) error {
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return ErrSourceBusy
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		processPath := filepath.Join("/proc", process.Name())
		info, err := os.Stat(processPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		}
		processStat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(processStat.Uid) != os.Geteuid() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(processPath, "comm"))
		if err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		applicationProcess := strings.Contains(name, "feather")
		fds, err := os.ReadDir(filepath.Join(processPath, "fd"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			if applicationProcess {
				return ErrSourceBusy
			}
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(processPath, "fd", fd.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				if applicationProcess {
					return ErrSourceBusy
				}
				continue
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if filepath.IsAbs(target) && containsPath(source, filepath.Clean(target)) {
				return ErrSourceBusy
			}
		}
	}
	return nil
}

func validateSourceDatabases(root string) error {
	return validateSourceDatabasesContext(context.Background(), root)
}

func validateSourceDatabasesContext(ctx context.Context, root string) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	for _, name := range []string{"history.db", "device.db"} {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(filepath.Join(root, name+suffix)); err == nil {
				return ErrSQLiteSidecars
			} else if !errors.Is(err, os.ErrNotExist) {
				return ErrUnsafeSource
			}
		}
	}
	history := filepath.Join(root, "history.db")
	if err := checkSQLiteDatabaseContext(ctx, history, true, true); err != nil {
		if errors.Is(err, ErrWrongSourceMode) || errors.Is(err, ErrSourceFileTooLarge) || errors.Is(err, ErrMigrationInterrupted) {
			return err
		}
		return ErrUnsupportedSource
	}
	device := filepath.Join(root, "device.db")
	if info, err := os.Lstat(device); err == nil {
		if !privateOwnedRegular(info) || info.Size() <= 0 {
			return ErrUnsafeSource
		}
		if info.Size() > MaxFileBytes {
			return ErrSourceFileTooLarge
		}
		if err := checkSQLiteDatabaseContext(ctx, device, false, true); err != nil {
			if errors.Is(err, ErrSourceFileTooLarge) || errors.Is(err, ErrMigrationInterrupted) {
				return err
			}
			return ErrUnsupportedSource
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeSource
	}
	return nil
}

func checkSQLiteDatabase(path string, liveMode, readOnly bool) error {
	return checkSQLiteDatabaseContext(context.Background(), path, liveMode, readOnly)
}

func checkSQLiteDatabaseContext(ctx context.Context, path string, liveMode, readOnly bool) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	info, err := os.Lstat(path)
	if err != nil || !privateOwnedRegular(info) || info.Size() <= 0 {
		return ErrUnsupportedSource
	}
	if info.Size() > MaxFileBytes {
		return ErrSourceFileTooLarge
	}
	db, err := openProfileSQLite(path, readOnly)
	if err != nil {
		return ErrUnsupportedSource
	}
	db.SetMaxOpenConns(1)
	if err := quickCheckContext(ctx, db); err != nil {
		_ = db.Close()
		if errors.Is(err, ErrMigrationInterrupted) {
			return err
		}
		return ErrUnsupportedSource
	}
	if liveMode {
		var mode string
		if err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='mode'").Scan(&mode); err != nil || mode != "live" {
			_ = db.Close()
			if ctx.Err() != nil {
				return ErrMigrationInterrupted
			}
			return ErrWrongSourceMode
		}
	}
	if err := db.Close(); err != nil {
		return ErrUnsupportedSource
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	return nil
}

func openProfileSQLite(path string, readOnly bool) (*sql.DB, error) {
	query := url.Values{}
	query.Add("_pragma", "temp_store(MEMORY)")
	query.Add("_pragma", "busy_timeout(1000)")
	if readOnly {
		query.Set("mode", "ro")
	}
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	return sql.Open("sqlite", uri)
}

func quickCheck(db *sql.DB) error {
	return quickCheckContext(context.Background(), db)
}

func quickCheckContext(ctx context.Context, db *sql.DB) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return ErrUnsupportedSource
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		var result string
		if err := rows.Scan(&result); err != nil || result != "ok" {
			return ErrUnsupportedSource
		}
		count++
		if count > 1 {
			return ErrUnsupportedSource
		}
	}
	if rows.Err() != nil {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return ErrUnsupportedSource
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	if count != 1 {
		return ErrUnsupportedSource
	}
	return nil
}

func boundedSourceTotal(total, size, maxFileBytes, maxProfileBytes int64) (int64, error) {
	if total < 0 || size < 0 || maxFileBytes < 0 || maxProfileBytes < 0 {
		return total, ErrUnsafeSource
	}
	if size > maxFileBytes {
		return total, ErrSourceFileTooLarge
	}
	if total > maxProfileBytes || size > maxProfileBytes-total {
		return total, ErrSourceAggregateLimit
	}
	return total + size, nil
}

func scanSource(root string) ([]plannedEntry, int64, int64, string, error) {
	return scanSourceContext(context.Background(), root)
}

func scanSourceContext(ctx context.Context, root string) ([]plannedEntry, int64, int64, string, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, 0, 0, "", ErrMigrationInterrupted
	}
	var entries []plannedEntry
	var files, total int64
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if walkErr != nil {
			return ErrUnsafeSource
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ErrUnsafeSource
		}
		if rel == "profile.lock" {
			info, statErr := os.Lstat(path)
			if statErr != nil || !privateOwnedRegular(info) {
				return ErrUnsafeSource
			}
			return nil
		}
		for _, database := range []string{"history.db", "device.db"} {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if rel == database+suffix {
					return ErrSQLiteSidecars
				}
			}
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 0 {
			return ErrUnsafeSource
		}
		switch parts[0] {
		case "history.db", "device.db":
			if len(parts) != 1 || entry.IsDir() {
				return ErrUnsupportedSource
			}
		case "media", "local-expressions":
		default:
			return ErrUnsupportedSource
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeSource
		}
		if info.IsDir() {
			if parts[0] != "media" && parts[0] != "local-expressions" {
				return ErrUnsupportedSource
			}
			if !privateOwnedDirectory(info) {
				return ErrUnsafeSource
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return ErrUnsafeSource
			}
			entries = append(entries, plannedEntry{rel: rel, isDir: true, mode: info.Mode(), modTime: info.ModTime().UnixNano(), device: uint64(st.Dev), inode: st.Ino})
		} else {
			if !privateOwnedRegular(info) {
				return ErrUnsafeSource
			}
			nextTotal, limitErr := boundedSourceTotal(total, info.Size(), MaxFileBytes, MaxProfileBytes)
			if limitErr != nil {
				return limitErr
			}
			files++
			total = nextTotal
			if files > MaxProfileFiles {
				return ErrSourceFileCountLimit
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return ErrUnsafeSource
			}
			digest, err := hashRegularFileContext(ctx, path, info)
			if err != nil {
				if errors.Is(err, ErrMigrationInterrupted) {
					return err
				}
				return ErrUnsafeSource
			}
			entries = append(entries, plannedEntry{rel: rel, size: info.Size(), mode: info.Mode(), modTime: info.ModTime().UnixNano(), device: uint64(st.Dev), inode: st.Ino, digest: digest})
		}
		if len(entries) > MaxProfileFiles*2 {
			return ErrSourceEntryLimit
		}
		return nil
	})
	if walkErr != nil {
		for _, reason := range []error{
			ErrMigrationInterrupted, ErrSQLiteSidecars, ErrUnsupportedSource, ErrSourceFileTooLarge,
			ErrSourceAggregateLimit, ErrSourceFileCountLimit, ErrSourceEntryLimit,
		} {
			if errors.Is(walkErr, reason) {
				return nil, 0, 0, "", reason
			}
		}
		return nil, 0, 0, "", ErrUnsafeSource
	}
	sort.Slice(entries, func(i, j int) bool {
		depthI, depthJ := strings.Count(entries[i].rel, string(filepath.Separator)), strings.Count(entries[j].rel, string(filepath.Separator))
		if depthI != depthJ {
			return depthI < depthJ
		}
		return entries[i].rel < entries[j].rel
	})
	if files == 0 {
		return nil, 0, 0, "", ErrUnsafeSource
	}
	return entries, files, total, treeDigest(entries), nil
}

func hashRegularFile(path string, expected os.FileInfo) (string, error) {
	return hashRegularFileContext(context.Background(), path, expected)
}

func hashRegularFileContext(ctx context.Context, path string, expected os.FileInfo) (string, error) {
	if ctx == nil || ctx.Err() != nil {
		return "", ErrMigrationInterrupted
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", ErrUnsafeSource
	}
	before, err := file.Stat()
	if err != nil || !samePlannedFile(expected, before) {
		_ = file.Close()
		return "", ErrUnsafeSource
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	var copied int64
	var hashErr error
	for copied < expected.Size() {
		if ctx.Err() != nil {
			hashErr = ErrMigrationInterrupted
			break
		}
		want := int64(len(buffer))
		if remaining := expected.Size() - copied; remaining < want {
			want = remaining
		}
		n, readErr := file.Read(buffer[:int(want)])
		if n > 0 {
			_, _ = h.Write(buffer[:n])
			copied += int64(n)
		}
		if readErr != nil {
			if readErr != io.EOF || copied != expected.Size() {
				hashErr = readErr
				if readErr == io.EOF {
					hashErr = io.ErrUnexpectedEOF
				}
			}
			break
		}
		if n == 0 {
			hashErr = io.ErrNoProgress
			break
		}
	}
	if hashErr == nil && ctx.Err() != nil {
		hashErr = ErrMigrationInterrupted
	}
	if hashErr == nil {
		var extra [1]byte
		n, readErr := file.Read(extra[:])
		if n != 0 || (readErr != nil && readErr != io.EOF) {
			hashErr = ErrUnsafeSource
		} else if n == 0 && readErr == nil {
			hashErr = io.ErrNoProgress
		}
	}
	after, statErr := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	closeErr := file.Close()
	if ctx.Err() != nil || errors.Is(hashErr, ErrMigrationInterrupted) {
		return "", ErrMigrationInterrupted
	}
	if hashErr != nil || statErr != nil || pathErr != nil || closeErr != nil || copied != expected.Size() ||
		!samePlannedFile(expected, after) || !samePlannedFile(expected, pathAfter) {
		return "", ErrUnsafeSource
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func samePlannedFile(expected, actual os.FileInfo) bool {
	if expected == nil || actual == nil || expected.Mode() != actual.Mode() || expected.Size() != actual.Size() || !expected.ModTime().Equal(actual.ModTime()) || !os.SameFile(expected, actual) {
		return false
	}
	st, ok := actual.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid() && st.Nlink == 1
}

func samePlannedEntry(expected plannedEntry, actual os.FileInfo) bool {
	if actual == nil || actual.Mode() != expected.mode || (!expected.isDir && actual.Size() != expected.size) || actual.ModTime().UnixNano() != expected.modTime {
		return false
	}
	st, ok := actual.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == expected.device && st.Ino == expected.inode && int(st.Uid) == os.Geteuid() && (expected.isDir || st.Nlink == 1)
}

func treeDigest(entries []plannedEntry) string {
	h := sha256.New()
	for _, entry := range entries {
		name := filepath.ToSlash(entry.rel)
		fmt.Fprintf(h, "%d:", len(name))
		_, _ = io.WriteString(h, name)
		fmt.Fprintf(h, ":%t:%d:%d:%d:%d:%d:%s\n", entry.isDir, entry.mode, entry.size, entry.modTime, entry.device, entry.inode, entry.digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Plan) copyEntry(ctx context.Context, mount string, entry plannedEntry, options Options) error {
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	source := filepath.Join(p.sourceRoot, entry.rel)
	destination := filepath.Join(mount, entry.rel)
	if !pathWithinRoot(mount, destination) {
		return ErrUnsafeSource
	}
	info, err := os.Lstat(source)
	if err != nil || !samePlannedEntry(entry, info) {
		return fmt.Errorf("source entry changed during bounded copy: %w", ErrUnsafeSource)
	}
	if entry.isDir {
		if err := os.Mkdir(destination, 0700); err != nil {
			return fmt.Errorf("create private destination directory: %w", ErrUnsafeSource)
		}
		dirInfo, err := os.Lstat(destination)
		if err != nil || !privateOwnedDirectory(dirInfo) {
			return fmt.Errorf("validate private destination directory: %w", ErrUnsafeSource)
		}
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return nil
	}
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open source entry without following links: %w", ErrUnsafeSource)
	}
	opened, err := in.Stat()
	if err != nil || !samePlannedEntry(entry, opened) {
		_ = in.Close()
		return fmt.Errorf("source entry identity changed while opening: %w", ErrUnsafeSource)
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		_ = in.Close()
		return fmt.Errorf("create private destination file: %w", ErrUnsafeSource)
	}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	var copied int64
	var copyErr error
	for copied < entry.size {
		if ctx.Err() != nil {
			copyErr = ErrMigrationInterrupted
			break
		}
		want := int64(len(buffer))
		if remaining := entry.size - copied; remaining < want {
			want = remaining
		}
		n, readErr := in.Read(buffer[:int(want)])
		if n > 0 {
			written, writeErr := out.Write(buffer[:n])
			if writeErr != nil {
				copyErr = writeErr
			} else if written != n {
				copyErr = io.ErrShortWrite
			}
			if written > 0 {
				_, _ = h.Write(buffer[:written])
				copied += int64(written)
			}
			if copyErr == nil {
				if err := trigger(options, PointFileCopyChunk); err != nil {
					copyErr = ErrMigrationInterrupted
				}
			}
			if ctx.Err() != nil {
				copyErr = ErrMigrationInterrupted
			}
		}
		if copyErr != nil {
			break
		}
		if readErr != nil {
			if readErr != io.EOF || copied != entry.size {
				copyErr = readErr
				if readErr == io.EOF {
					copyErr = io.ErrUnexpectedEOF
				}
			}
			break
		}
		if n == 0 {
			copyErr = io.ErrNoProgress
			break
		}
	}
	if copyErr == nil && ctx.Err() != nil {
		copyErr = ErrMigrationInterrupted
	}
	if copyErr == nil {
		var extra [1]byte
		n, readErr := in.Read(extra[:])
		if n != 0 || (readErr != nil && readErr != io.EOF) {
			copyErr = ErrUnsafeSource
		} else if n == 0 && readErr == nil {
			copyErr = io.ErrNoProgress
		}
	}
	finalInfo, finalStatErr := in.Stat()
	pathInfo, pathErr := os.Lstat(source)
	inErr := in.Close()
	var syncErr error
	if copyErr == nil && ctx.Err() == nil {
		// File.Sync is not cancellable; cancellation is checked immediately
		// afterward and prevents completion publication.
		syncErr = out.Sync()
	}
	if ctx.Err() != nil && copyErr == nil {
		copyErr = ErrMigrationInterrupted
	}
	closeErr := out.Close()
	if ctx.Err() != nil || errors.Is(copyErr, ErrMigrationInterrupted) {
		return ErrMigrationInterrupted
	}
	if copyErr != nil || finalStatErr != nil || pathErr != nil || inErr != nil || syncErr != nil || closeErr != nil ||
		copied != entry.size || hex.EncodeToString(h.Sum(nil)) != entry.digest ||
		!samePlannedEntry(entry, finalInfo) || !samePlannedEntry(entry, pathInfo) {
		return fmt.Errorf("source entry changed or failed digest verification during copy: %w", ErrUnsafeSource)
	}
	return nil
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (p *Plan) verifySourceStable(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	info, err := os.Lstat(p.sourceRoot)
	if err != nil {
		return ErrUnsafeSource
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Dev) != p.rootDevice || st.Ino != p.rootInode || !privateOwnedDirectory(info) {
		return ErrUnsafeSource
	}
	entries, files, total, digest, err := scanSourceContext(ctx, p.sourceRoot)
	if err != nil {
		return err
	}
	if files != p.files || total != p.bytes || digest != p.treeDigest || !sameEntries(p.entries, entries) {
		return ErrUnsafeSource
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	return nil
}

func sameEntries(a, b []plannedEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validatePersistedMediaMetadata(ctx context.Context, root string) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	db, err := openProfileSQLite(filepath.Join(root, "history.db"), true)
	if err != nil {
		return ErrUnsupportedSource
	}
	defer db.Close()
	for _, spec := range []struct {
		table, column string
		validate      func([]byte) error
	}{
		{"attachments", "meta", validateAttachmentMetadata},
		{"message_albums", "items", validateAlbumMetadata},
	} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", spec.table).Scan(&count); err != nil {
			if ctx.Err() != nil {
				return ErrMigrationInterrupted
			}
			return ErrUnsupportedSource
		}
		if count == 0 {
			continue
		}
		rows, err := db.QueryContext(ctx, "SELECT "+spec.column+" FROM "+spec.table)
		if err != nil {
			if ctx.Err() != nil {
				return ErrMigrationInterrupted
			}
			return ErrUnsupportedSource
		}
		for rows.Next() {
			if ctx.Err() != nil {
				_ = rows.Close()
				return ErrMigrationInterrupted
			}
			var raw []byte
			if err := rows.Scan(&raw); err != nil || spec.validate(raw) != nil {
				_ = rows.Close()
				if ctx.Err() != nil {
					return ErrMigrationInterrupted
				}
				return ErrUnsupportedSource
			}
			if ctx.Err() != nil {
				_ = rows.Close()
				return ErrMigrationInterrupted
			}
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if rowsErr != nil || closeErr != nil {
			return ErrUnsupportedSource
		}
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	return nil
}

func validateAttachmentMetadata(raw []byte) error {
	var object map[string]json.RawMessage
	if len(raw) == 0 || len(raw) > maxMediaMetadataBytes || json.Unmarshal(raw, &object) != nil || object == nil {
		return ErrUnsupportedSource
	}
	var metadata app.Attachment
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Size < 0 || metadata.OriginalSize < 0 ||
		metadata.PreparedSize < 0 || metadata.Duration < 0 || metadata.Width < 0 || metadata.Height < 0 {
		return ErrUnsupportedSource
	}
	return nil
}

func validateAlbumMetadata(raw []byte) error {
	if len(raw) == 0 || len(raw) > maxMediaMetadataBytes {
		return ErrUnsupportedSource
	}
	var items []app.Attachment
	if err := json.Unmarshal(raw, &items); err != nil || len(items) < 2 || len(items) > 10 {
		return ErrUnsupportedSource
	}
	for _, item := range items {
		if item.Kind != "image" || item.Mode != "photo" || item.PreparedPath != "" || item.State != "ready" ||
			!strings.HasPrefix(item.MIME, "image/") || item.MIME == "image/gif" || item.Size < 0 || item.PreparedSize < 0 {
			return ErrUnsupportedSource
		}
	}
	return nil
}

func checkCopiedDatabases(root string) error {
	return checkCopiedDatabasesContext(context.Background(), root)
}

func checkCopiedDatabasesContext(ctx context.Context, root string) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	for _, name := range []string{"history.db", "device.db"} {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && name == "device.db" {
			continue
		} else if err != nil {
			return ErrUnsupportedSource
		}
		if err := checkSQLiteDatabaseContext(ctx, path, name == "history.db", true); err != nil {
			if errors.Is(err, ErrMigrationInterrupted) {
				return err
			}
			return ErrUnsupportedSource
		}
	}
	return validatePersistedMediaMetadata(ctx, root)
}

func rebaseOwnedMedia(profileRoot, sourceRoot string) error {
	return rebaseOwnedMediaContext(context.Background(), profileRoot, sourceRoot)
}

func rebaseOwnedMediaContext(ctx context.Context, profileRoot, sourceRoot string) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	db, err := openProfileSQLite(filepath.Join(profileRoot, "history.db"), false)
	if err != nil {
		return ErrUnsupportedSource
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return ErrUnsupportedSource
	}
	if err = rebaseAttachmentTable(ctx, tx, profileRoot, sourceRoot); err == nil {
		err = rebaseAlbumTable(ctx, tx, profileRoot, sourceRoot)
	}
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		if errors.Is(err, ErrMigrationInterrupted) || ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return ErrUnsupportedSource
	}
	if err = tx.Commit(); err != nil {
		_ = db.Close()
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		return ErrUnsupportedSource
	}
	if err = db.Close(); err != nil {
		return ErrUnsupportedSource
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	return nil
}

func hasTable(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count); err != nil {
		return false, err
	}
	return count == 1, nil
}

func rebaseAttachmentTable(ctx context.Context, tx *sql.Tx, profileRoot, sourceRoot string) error {
	exists, err := hasTable(ctx, tx, "attachments")
	if err != nil || !exists {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT chat_id,message_id,meta FROM attachments")
	if err != nil {
		return err
	}
	type row struct {
		chat, message string
		meta          []byte
	}
	var updates []row
	for rows.Next() {
		if ctx.Err() != nil {
			_ = rows.Close()
			return ErrMigrationInterrupted
		}
		var value row
		if err := rows.Scan(&value.chat, &value.message, &value.meta); err != nil {
			_ = rows.Close()
			return err
		}
		changed, ok, err := rebaseAttachmentJSONContext(ctx, value.meta, profileRoot, sourceRoot)
		if err != nil {
			_ = rows.Close()
			return err
		}
		if ok {
			value.meta = changed
			updates = append(updates, value)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, value := range updates {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if _, err := tx.ExecContext(ctx, "UPDATE attachments SET meta=? WHERE chat_id=? AND message_id=?", string(value.meta), value.chat, value.message); err != nil {
			return err
		}
	}
	return nil
}

func rebaseAlbumTable(ctx context.Context, tx *sql.Tx, profileRoot, sourceRoot string) error {
	exists, err := hasTable(ctx, tx, "message_albums")
	if err != nil || !exists {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT chat_id,message_id,items FROM message_albums")
	if err != nil {
		return err
	}
	type row struct {
		chat, message string
		items         []byte
	}
	var updates []row
	for rows.Next() {
		if ctx.Err() != nil {
			_ = rows.Close()
			return ErrMigrationInterrupted
		}
		var value row
		if err := rows.Scan(&value.chat, &value.message, &value.items); err != nil {
			_ = rows.Close()
			return err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value.items, &items); err != nil {
			_ = rows.Close()
			return err
		}
		changed := false
		for i, item := range items {
			next, ok, err := rebaseAttachmentJSONContext(ctx, item, profileRoot, sourceRoot)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if ok {
				items[i], changed = next, true
			}
		}
		if changed {
			value.items, err = json.Marshal(items)
			if err != nil {
				_ = rows.Close()
				return err
			}
			updates = append(updates, value)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, value := range updates {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if _, err := tx.ExecContext(ctx, "UPDATE message_albums SET items=? WHERE chat_id=? AND message_id=?", string(value.items), value.chat, value.message); err != nil {
			return err
		}
	}
	return nil
}

func rebaseAttachmentJSON(raw []byte, profileRoot, sourceRoot string) ([]byte, bool) {
	updated, changed, err := rebaseAttachmentJSONContext(context.Background(), raw, profileRoot, sourceRoot)
	return updated, changed && err == nil
}

func rebaseAttachmentJSONContext(ctx context.Context, raw []byte, profileRoot, sourceRoot string) ([]byte, bool, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, false, ErrMigrationInterrupted
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false, ErrUnsupportedSource
	}
	var state, rawURL, digest, mediaType, name string
	var size int64
	if json.Unmarshal(fields["state"], &state) != nil || state != "ready" ||
		json.Unmarshal(fields["url"], &rawURL) != nil || json.Unmarshal(fields["sha256"], &digest) != nil ||
		json.Unmarshal(fields["size"], &size) != nil || json.Unmarshal(fields["mime"], &mediaType) != nil ||
		json.Unmarshal(fields["name"], &name) != nil {
		return nil, false, nil
	}
	fileName, ok := ownedMediaFilename(rawURL, sourceRoot, digest, size, mediaType, name)
	if !ok {
		return nil, false, nil
	}
	verified, err := verifyCopiedMediaContext(ctx, sourceRoot, profileRoot, fileName, digest, size, mediaType)
	if err != nil {
		return nil, false, err
	}
	if !verified {
		return nil, false, nil
	}
	newURL := (&url.URL{Scheme: "file", Path: "/media/v1/" + fileName}).String()
	encoded, err := json.Marshal(newURL)
	if err != nil {
		return nil, false, err
	}
	fields["url"] = encoded
	updated, err := json.Marshal(fields)
	return updated, err == nil, err
}

func ownedMediaFilename(rawURL, sourceRoot, digest string, size int64, mediaType, name string) (string, bool) {
	if !validDigest(digest) || size <= 0 || size > MaxFileBytes {
		return "", false
	}
	ext := mediaExtension(mediaType, name)
	if ext == "" {
		return "", false
	}
	fileName := digest + ext
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "file" || parsed.Opaque != "" || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Path != filepath.Join(sourceRoot, "media", fileName) && parsed.Path != "/media/v1/"+fileName {
		return "", false
	}
	if parsed.String() != rawURL {
		return "", false
	}
	return fileName, true
}

func mediaExtension(mediaType, name string) string {
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = parsed
	}
	switch strings.ToLower(mediaType) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "application/pdf":
		return ".pdf"
	}
	ext := strings.ToLower(filepath.Ext(filepath.Base(name)))
	switch ext {
	case ".txt", ".csv", ".json", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".zip":
		if mediaType == "text/plain" || mediaType == "text/csv" || mediaType == "application/json" || mediaType == "application/msword" || strings.HasPrefix(mediaType, "application/vnd.openxmlformats-officedocument.") || mediaType == "application/zip" {
			return ext
		}
	}
	return ""
}

func verifyCopiedMedia(sourceRoot, profileRoot, name, digest string, size int64, mediaType string) bool {
	verified, err := verifyCopiedMediaContext(context.Background(), sourceRoot, profileRoot, name, digest, size, mediaType)
	return verified && err == nil
}

func verifyCopiedMediaContext(ctx context.Context, sourceRoot, profileRoot, name, digest string, size int64, mediaType string) (bool, error) {
	for _, root := range []string{sourceRoot, profileRoot} {
		if ctx == nil || ctx.Err() != nil {
			return false, ErrMigrationInterrupted
		}
		path := filepath.Join(root, "media", name)
		info, err := os.Lstat(path)
		if err != nil || !privateOwnedRegular(info) || info.Size() != size {
			return false, nil
		}
		actual, err := hashRegularFileContext(ctx, path, info)
		if err != nil {
			if errors.Is(err, ErrMigrationInterrupted) {
				return false, err
			}
			return false, nil
		}
		if actual != digest || !mediaContentMatches(path, mediaType) {
			return false, nil
		}
	}
	if ctx.Err() != nil {
		return false, ErrMigrationInterrupted
	}
	return true, nil
}

func mediaContentMatches(path, mediaType string) bool {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		return false
	}
	contentType, _, _ := mime.ParseMediaType(http.DetectContentType(buffer[:n]))
	expected, _, parseErr := mime.ParseMediaType(mediaType)
	if parseErr != nil {
		expected = mediaType
	}
	if expected == "application/pdf" {
		return bytes.HasPrefix(buffer[:n], []byte("%PDF-"))
	}
	if strings.HasPrefix(expected, "application/vnd.openxmlformats-officedocument.") || expected == "application/zip" {
		return n >= 4 && bytes.Equal(buffer[:4], []byte{'P', 'K', 3, 4})
	}
	if expected == "application/msword" {
		return n >= 4 && bytes.Equal(buffer[:4], []byte{0xd0, 0xcf, 0x11, 0xe0})
	}
	if expected == "text/plain" || expected == "text/csv" || expected == "application/json" {
		return contentType == "text/plain; charset=utf-8" || contentType == "text/plain" || contentType == "application/json"
	}
	return contentType == expected
}

func pathIdentity(path string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(digest[:])
}

func (p *Plan) verifySourceRootIdentity() bool {
	info, err := os.Lstat(p.sourceRoot)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Dev) == p.rootDevice && st.Ino == p.rootInode
}

func syncCopiedTree(root string) error {
	return syncCopiedTreeContext(context.Background(), root)
}

func syncCopiedTreeContext(ctx context.Context, root string) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeSource
		}
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if syncErr != nil || closeErr != nil {
			return ErrUnsafeSource
		}
		return nil
	})
	if errors.Is(err, ErrMigrationInterrupted) || ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	if err != nil {
		return ErrUnsafeSource
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		file, err := os.Open(dir)
		if err != nil {
			return ErrUnsafeSource
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if ctx.Err() != nil {
			return ErrMigrationInterrupted
		}
		if syncErr != nil || closeErr != nil {
			return ErrUnsafeSource
		}
	}
	if ctx.Err() != nil {
		return ErrMigrationInterrupted
	}
	return nil
}
