package profilemigration

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	stateFilename = ".whatsfeather-migration.json"
	stateFormat   = "whatsfeather-profile-migration"
	stateVersion  = 1
	maxStateBytes = 4096
)

var (
	ErrIncompleteMigration = errors.New("profile migration is incomplete; this encrypted profile cannot be opened")
	ErrMigrationState      = errors.New("profile migration state is invalid or bound to another encrypted profile")
)

type migrationState struct {
	Format              string `json:"format"`
	Version             int    `json:"version"`
	State               string `json:"state"`
	SourceIdentity      string `json:"source_identity"`
	DestinationIdentity string `json:"destination_identity"`
	SourceDevice        uint64 `json:"source_device"`
	SourceInode         uint64 `json:"source_inode"`
	SourceTreeSHA256    string `json:"source_tree_sha256,omitempty"`
	Files               int64  `json:"files,omitempty"`
	Bytes               int64  `json:"bytes,omitempty"`
	ModeVerified        string `json:"mode_verified,omitempty"`
	SQLiteQuickCheck    bool   `json:"sqlite_quick_check,omitempty"`
	MediaRebased        bool   `json:"media_rebased,omitempty"`
}

// CheckStartupState preserves compatibility for ordinary encrypted profiles
// that were not created by the migration coordinator.
func CheckStartupState(mountRoot, destinationRoot string) error {
	return CheckStartupStateWithIntent(mountRoot, destinationRoot, false)
}

// CheckStartupStateWithIntent verifies the copied-tree marker against the
// destination-bound outer intent. Missing inner state is accepted only when
// the outer vault records that migration was never started.
func CheckStartupStateWithIntent(mountRoot, destinationRoot string, migrationStarted bool) error {
	if !cleanAbsolute(mountRoot) || !cleanAbsolute(destinationRoot) {
		return ErrMigrationState
	}
	path := filepath.Join(mountRoot, stateFilename)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if migrationStarted {
			return ErrIncompleteMigration
		}
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
		return ErrMigrationState
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || info.Size() < 2 || info.Size() > maxStateBytes {
		return ErrMigrationState
	}
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) != info.Size() {
		return ErrMigrationState
	}
	state, err := decodeState(data)
	if err != nil || validateState(state) != nil {
		return ErrMigrationState
	}
	if state.DestinationIdentity != destinationIdentity(destinationRoot) {
		return ErrMigrationState
	}
	if state.State != "complete" {
		return ErrIncompleteMigration
	}
	return nil
}

func decodeState(data []byte) (migrationState, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state migrationState
	if err := decoder.Decode(&state); err != nil {
		return migrationState{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return migrationState{}, fmt.Errorf("trailing migration state data")
	}
	return state, nil
}

func validateState(state migrationState) error {
	if state.Format != stateFormat || state.Version != stateVersion ||
		(state.State != "copying" && state.State != "complete") ||
		!validDigest(state.SourceIdentity) || !strings.HasPrefix(state.DestinationIdentity, "profile-path-sha256-") ||
		!validDigest(strings.TrimPrefix(state.DestinationIdentity, "profile-path-sha256-")) ||
		state.SourceDevice == 0 || state.SourceInode == 0 {
		return ErrMigrationState
	}
	if state.State == "complete" && (!validDigest(state.SourceTreeSHA256) || state.Files < 1 || state.Bytes < 1 || state.ModeVerified != "live" || !state.SQLiteQuickCheck || !state.MediaRebased) {
		return ErrMigrationState
	}
	if state.State == "copying" && (state.SourceTreeSHA256 != "" || state.Files != 0 || state.Bytes != 0 || state.ModeVerified != "" || state.SQLiteQuickCheck || state.MediaRebased) {
		return ErrMigrationState
	}
	return nil
}

func destinationIdentity(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return "profile-path-sha256-" + hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func writeMigrationState(mountRoot string, state migrationState, create bool) error {
	if !cleanAbsolute(mountRoot) || validateState(state) != nil {
		return ErrMigrationState
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > maxStateBytes {
		return ErrMigrationState
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrMigrationState
	}
	temp := filepath.Join(mountRoot, ".whatsfeather-migration.tmp."+hex.EncodeToString(nonce[:]))
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	file, err := os.OpenFile(temp, flags, 0600)
	if err != nil {
		return ErrMigrationState
	}
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(temp)
		return ErrMigrationState
	}
	if create {
		err = os.Link(temp, filepath.Join(mountRoot, stateFilename))
		if err == nil {
			err = os.Remove(temp)
		}
	} else {
		err = os.Rename(temp, filepath.Join(mountRoot, stateFilename))
	}
	if err != nil {
		_ = os.Remove(temp)
		return ErrMigrationState
	}
	dir, err := os.Open(mountRoot)
	if err != nil {
		return ErrMigrationState
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil || closeErr != nil {
		return ErrMigrationState
	}
	return nil
}
