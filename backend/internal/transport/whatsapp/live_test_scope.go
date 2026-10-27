package whatsapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	liveTestScopeMaxBytes            = 16 << 10
	liveTestScopeMaxChats            = 4
	liveTestScopeMaxCommunityInfoIDs = 1
	liveTestScopeMaxMediaItems       = 5
	liveTestScopeMaxMediaBytes       = 20 << 20
	liveTestScopeMaxReceiptIDs       = 10000
	liveTestScopeWindowDuration      = 7 * 24 * time.Hour
)

var errInvalidLiveTestScope = errors.New("invalid live-test scope")

type liveTestScopeConfig struct {
	Since            string   `json:"since"`
	Chats            []string `json:"chats"`
	CommunityInfoIDs []string `json:"community_info_ids,omitempty"`
	IncludeSelf      *bool    `json:"include_self"`
}

type liveTestMessageKey struct {
	chat string
	id   string
}

// LiveTestScope is immutable except for bounded per-process receipt/media state.
// A scope is loaded once by the daemon, before it opens the profile or device DB.
type LiveTestScope struct {
	since            time.Time
	chats            map[string]struct{}
	communityInfoIDs map[string]struct{}
	includeSelf      bool

	mu               sync.Mutex
	acceptedMessages map[liveTestMessageKey]struct{}
	acceptedOrder    []liveTestMessageKey
	mutationTargets  map[liveTestMessageKey]struct{}
	mutationOrder    []liveTestMessageKey
	manualMediaItems int
}

func LoadLiveTestScope(path string) (*LiveTestScope, error) {
	return loadLiveTestScopeAt(path, time.Now())
}

func loadLiveTestScopeAt(path string, now time.Time) (*LiveTestScope, error) {
	if path == "" {
		return nil, errInvalidLiveTestScope
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errInvalidLiveTestScope
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errInvalidLiveTestScope
	}
	defer file.Close()
	info, err := file.Stat()
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	openedStat, openedOK := func() (*syscall.Stat_t, bool) {
		if err != nil || info == nil {
			return nil, false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		return stat, ok
	}()
	if err != nil || !beforeOK || !openedOK || !os.SameFile(before, info) ||
		beforeStat.Dev != openedStat.Dev || beforeStat.Ino != openedStat.Ino ||
		!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0111 != 0 || info.Mode().Perm()&0400 == 0 {
		return nil, errInvalidLiveTestScope
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Size() <= 0 || info.Size() > liveTestScopeMaxBytes {
		return nil, errInvalidLiveTestScope
	}
	data, err := io.ReadAll(io.LimitReader(file, liveTestScopeMaxBytes+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(info, after) || after.Size() != info.Size() ||
		len(data) == 0 || len(data) > liveTestScopeMaxBytes || int64(len(data)) != info.Size() {
		return nil, errInvalidLiveTestScope
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, errInvalidLiveTestScope
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config liveTestScopeConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, errInvalidLiveTestScope
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errInvalidLiveTestScope
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawConfig); err != nil {
		return nil, errInvalidLiveTestScope
	}
	if rawIDs, present := rawConfig["community_info_ids"]; present {
		var explicitIDs []json.RawMessage
		if json.Unmarshal(rawIDs, &explicitIDs) != nil || explicitIDs == nil {
			return nil, errInvalidLiveTestScope
		}
	}
	if config.Since == "" || !strings.HasSuffix(config.Since, "Z") || config.IncludeSelf == nil ||
		len(config.Chats) > liveTestScopeMaxChats || len(config.CommunityInfoIDs) > liveTestScopeMaxCommunityInfoIDs {
		return nil, errInvalidLiveTestScope
	}
	since, err := time.Parse(time.RFC3339Nano, config.Since)
	if err != nil || since.After(now) || since.Before(now.Add(-liveTestScopeWindowDuration)) {
		return nil, errInvalidLiveTestScope
	}
	chats := make(map[string]struct{}, len(config.Chats))
	for _, value := range config.Chats {
		jid, parseErr := types.ParseJID(value)
		if parseErr != nil || jid.User == "" || jid.Device != 0 || jid.RawAgent != 0 || jid.Integrator != 0 ||
			jid.String() != value || value != strings.ToLower(value) || strings.TrimSpace(value) != value ||
			!allowedScopeChatServer(jid.Server) {
			return nil, errInvalidLiveTestScope
		}
		if _, duplicate := chats[value]; duplicate {
			return nil, errInvalidLiveTestScope
		}
		chats[value] = struct{}{}
	}
	communityInfoIDs := make(map[string]struct{}, len(config.CommunityInfoIDs))
	for _, value := range config.CommunityInfoIDs {
		if !validScopedCommunityInfoID(value) {
			return nil, errInvalidLiveTestScope
		}
		if _, duplicate := communityInfoIDs[value]; duplicate {
			return nil, errInvalidLiveTestScope
		}
		communityInfoIDs[value] = struct{}{}
	}
	metadataOnly := len(chats) == 0 && len(communityInfoIDs) > 0 && !*config.IncludeSelf
	if len(chats) == 0 {
		if !metadataOnly {
			return nil, errInvalidLiveTestScope
		}
		rawChats, present := rawConfig["chats"]
		var explicitChats []json.RawMessage
		if !present || json.Unmarshal(rawChats, &explicitChats) != nil || explicitChats == nil || len(explicitChats) != 0 {
			return nil, errInvalidLiveTestScope
		}
	}
	return &LiveTestScope{
		since: since, chats: chats, communityInfoIDs: communityInfoIDs, includeSelf: *config.IncludeSelf,
		acceptedMessages: make(map[liveTestMessageKey]struct{}),
		mutationTargets:  make(map[liveTestMessageKey]struct{}),
	}, nil
}

func allowedScopeChatServer(server string) bool {
	switch server {
	case types.DefaultUserServer, types.LegacyUserServer, types.GroupServer,
		types.HiddenUserServer, types.HostedServer, types.HostedLIDServer:
		return true
	default:
		return false
	}
}

func validScopedCommunityInfoID(value string) bool {
	if value == "" || len(value) > 256 || value != strings.ToLower(value) || strings.TrimSpace(value) != value {
		return false
	}
	jid, err := types.ParseJID(value)
	return err == nil && jid.User != "" && jid.Device == 0 && jid.RawAgent == 0 && jid.Integrator == 0 &&
		jid.Server == types.GroupServer && jid.String() == value
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errInvalidLiveTestScope
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errInvalidLiveTestScope
			}
			if _, duplicate := seen[key]; duplicate {
				return errInvalidLiveTestScope
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errInvalidLiveTestScope
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errInvalidLiveTestScope
		}
	default:
		return errInvalidLiveTestScope
	}
	return nil
}

func (s *LiveTestScope) allowsChat(chat string, connectedSelf ...string) bool {
	if s == nil {
		return true
	}
	knownSelf := false
	for _, self := range connectedSelf {
		if self == "" {
			continue
		}
		knownSelf = true
		if chat == self {
			return s.includeSelf
		}
	}
	if !knownSelf {
		return false
	}
	if _, ok := s.chats[chat]; ok {
		return true
	}
	return false
}

func (s *LiveTestScope) allowsCommunityInfo(id string) bool {
	if s == nil || !validScopedCommunityInfoID(id) {
		return false
	}
	_, ok := s.communityInfoIDs[id]
	return ok
}

func (s *LiveTestScope) allowsTimestamp(timestamp, now time.Time) bool {
	if s == nil {
		return true
	}
	return !timestamp.IsZero() && !timestamp.Before(s.since) && !timestamp.After(now)
}

func (s *LiveTestScope) acceptsMessage(chat, connectedSelf string, timestamp, now time.Time) bool {
	return s.allowsChat(chat, connectedSelf) && s.allowsTimestamp(timestamp, now)
}

func (s *LiveTestScope) rememberAcceptedMessage(chat, id string) {
	if s == nil || chat == "" || id == "" {
		return
	}
	key := liveTestMessageKey{chat: chat, id: id}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.acceptedMessages[key]; exists {
		return
	}
	s.acceptedMessages[key] = struct{}{}
	s.acceptedOrder = append(s.acceptedOrder, key)
	if len(s.acceptedOrder) > liveTestScopeMaxReceiptIDs {
		oldest := s.acceptedOrder[0]
		s.acceptedOrder = s.acceptedOrder[1:]
		delete(s.acceptedMessages, oldest)
	}
}

func (s *LiveTestScope) acceptedReceiptIDs(chat string, ids []string) []string {
	if s == nil {
		return ids
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	accepted := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := s.acceptedMessages[liveTestMessageKey{chat: chat, id: id}]; ok {
			accepted = append(accepted, id)
		}
	}
	return accepted
}

func (s *LiveTestScope) reserveManualMediaItem() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manualMediaItems >= liveTestScopeMaxMediaItems {
		return false
	}
	s.manualMediaItems++
	return true
}

func (s *LiveTestScope) mediaByteLimit() int64 {
	if s == nil {
		return liveTestScopeMaxMediaBytes
	}
	return liveTestScopeMaxMediaBytes
}

func validScopedMessageID(id string) bool {
	if len(id) == 0 || len(id) > 128 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if r <= 0x20 || r == 0x7f || !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (s *LiveTestScope) rememberSuccessfulSend(chat, id string, current bool) {
	if s == nil || !current || chat == "" || !validScopedMessageID(id) {
		return
	}
	key := liveTestMessageKey{chat: chat, id: id}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutationTargets == nil {
		s.mutationTargets = make(map[liveTestMessageKey]struct{})
	}
	if _, exists := s.mutationTargets[key]; exists {
		return
	}
	s.mutationTargets[key] = struct{}{}
	s.mutationOrder = append(s.mutationOrder, key)
	if len(s.mutationOrder) > liveTestScopeMaxReceiptIDs {
		oldest := s.mutationOrder[0]
		s.mutationOrder = s.mutationOrder[1:]
		delete(s.mutationTargets, oldest)
	}
}

func (s *LiveTestScope) allowsMutationTarget(chat, id string) bool {
	if s == nil {
		return true
	}
	if chat == "" || !validScopedMessageID(id) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.mutationTargets[liveTestMessageKey{chat: chat, id: id}]
	return ok
}

func (s *LiveTestScope) allowsManualMedia(chat, id string, timestamp time.Time) bool {
	if s == nil {
		return true
	}
	if chat == "" || id == "" || !s.allowsTimestamp(timestamp, time.Now()) {
		return false
	}
	_, ok := s.chats[chat]
	return ok
}

// normalizedConfig is the stable, validated representation bound to a scoped
// profile. Chat ordering in the input file cannot change profile identity.
func (s *LiveTestScope) normalizedConfig() []byte {
	if s == nil {
		return nil
	}
	chats := make([]string, 0, len(s.chats))
	for chat := range s.chats {
		chats = append(chats, chat)
	}
	sort.Strings(chats)
	communityInfoIDs := make([]string, 0, len(s.communityInfoIDs))
	for id := range s.communityInfoIDs {
		communityInfoIDs = append(communityInfoIDs, id)
	}
	sort.Strings(communityInfoIDs)
	data, _ := json.Marshal(liveTestScopeConfig{
		Since: s.since.UTC().Format(time.RFC3339Nano), Chats: chats, CommunityInfoIDs: communityInfoIDs, IncludeSelf: &s.includeSelf,
	})
	return data
}

const liveTestProfileMarker = ".live-test-scope"

var (
	errScopedProfileInvalid  = errors.New("scoped profile must be new, empty or already bound")
	errScopedProfileMismatch = errors.New("scope profile mismatch")
	errMarkedProfileOrdinary = errors.New("scoped profile cannot be opened in ordinary mode")
)

func privateOwnedDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func readScopeMarker(dir string) ([]byte, error) {
	path := filepath.Join(dir, liveTestProfileMarker)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 ||
		before.Mode().Perm()&0077 != 0 || before.Mode().Perm()&0111 != 0 || before.Size() <= 0 || before.Size() > liveTestScopeMaxBytes {
		return nil, errScopedProfileInvalid
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errScopedProfileInvalid
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errScopedProfileInvalid
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() != before.Size() {
		return nil, errScopedProfileInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, liveTestScopeMaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > liveTestScopeMaxBytes || int64(len(data)) != before.Size() {
		return nil, errScopedProfileInvalid
	}
	return data, nil
}

// PrepareScopedLiveTestProfile permits only a fresh private profile or one
// already bound to the same validated scope. The marker is owner-only and
// atomically linked into place only after its bytes have been synced.
func PrepareScopedLiveTestProfile(dir string, scope *LiveTestScope) (string, error) {
	if dir == "" || scope == nil || !filepath.IsAbs(dir) {
		return "", errScopedProfileInvalid
	}
	dir = filepath.Clean(dir)
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return "", errScopedProfileInvalid
		}
	} else if err != nil {
		return "", errScopedProfileInvalid
	}
	if !privateOwnedDirectory(dir) {
		return "", errScopedProfileInvalid
	}
	marker := filepath.Join(dir, liveTestProfileMarker)
	if _, err := os.Lstat(marker); err == nil {
		stored, readErr := readScopeMarker(dir)
		if readErr != nil {
			return "", errScopedProfileInvalid
		}
		if !bytes.Equal(stored, scope.normalizedConfig()) {
			return "", errScopedProfileMismatch
		}
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errScopedProfileInvalid
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		return "", errScopedProfileInvalid
	}
	tmp, err := os.OpenFile(filepath.Join(dir, ".scope-marker.tmp"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", errScopedProfileInvalid
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(scope.normalizedConfig()); err != nil {
		tmp.Close()
		return "", errScopedProfileInvalid
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return "", errScopedProfileInvalid
	}
	if err = tmp.Close(); err != nil {
		return "", errScopedProfileInvalid
	}
	if err = os.Link(tmpPath, marker); err != nil {
		return "", errScopedProfileInvalid
	}
	dirFile, err := os.Open(dir)
	if err == nil {
		err = dirFile.Sync()
		_ = dirFile.Close()
	}
	if err != nil {
		return "", errScopedProfileInvalid
	}
	return dir, nil
}

// RejectOrdinaryProfileMode prevents demo or unscoped live mode from opening
// a profile that has been explicitly bound to scoped live-test mode.
func RejectOrdinaryProfileMode(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errScopedProfileInvalid
	}
	if _, err = os.Lstat(filepath.Join(dir, liveTestProfileMarker)); err == nil {
		return errMarkedProfileOrdinary
	} else if !errors.Is(err, os.ErrNotExist) {
		return errScopedProfileInvalid
	}
	return nil
}
