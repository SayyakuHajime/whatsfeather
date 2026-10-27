package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types"
)

const communityPageBudget = 192 << 10
const communityPageMax = 100

type communityFolder struct {
	ID     string          `json:"id"`
	Title  string          `json:"title"`
	Groups []communityChat `json:"groups"`
}

type communityChat struct {
	ID                      string `json:"id"`
	Title                   string `json:"title"`
	CommunityID             string `json:"community_id"`
	CommunityTitle          string `json:"community_title"`
	IsCommunityAnnouncement *bool  `json:"is_community_announcement,omitempty"`
}

type communityMember struct {
	chatID       string
	chatTitle    string
	communityID  string
	announcement *bool
}

type communitySnapshot struct {
	communities []communityFolder
	groups      []CommunityGroupInfo
	members     []communityMember
}

type communityCursor struct {
	communityID string
	chatID      string
}

type chatListCursor struct {
	timestamp int64
	chatID    string
}

func encodeChatListCursor(cursor chatListCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(cursor.timestamp, 10) + "\n" + cursor.chatID))
}

func decodeChatListCursor(value string) (chatListCursor, error) {
	if value == "" {
		return chatListCursor{}, nil
	}
	if len(value) > 1024 {
		return chatListCursor{}, fail("invalid_params", "Chat cursor is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return chatListCursor{}, fail("invalid_params", "Chat cursor is invalid")
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || len(parts[1]) == 0 || len(parts[1]) > 256 || strings.ContainsAny(parts[1], "\x00\r\n") {
		return chatListCursor{}, fail("invalid_params", "Chat cursor is invalid")
	}
	timestamp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return chatListCursor{}, fail("invalid_params", "Chat cursor is invalid")
	}
	return chatListCursor{timestamp: timestamp, chatID: parts[1]}, nil
}

func validCommunityJID(value string) bool {
	if value == "" || len(value) > 256 || value != strings.ToLower(value) || strings.TrimSpace(value) != value {
		return false
	}
	jid, err := types.ParseJID(value)
	return err == nil && jid.Server == types.GroupServer && jid.User != "" && jid.Device == 0 && jid.RawAgent == 0 && jid.Integrator == 0 && jid.String() == value
}

func decodeCommunityInfoRequest(raw json.RawMessage) (string, error) {
	invalid := func() (string, error) {
		return "", fail("invalid_params", "Community info request must contain only community_id")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return invalid()
	}
	key, err := decoder.Token()
	field, ok := key.(string)
	if err != nil || !ok || field != "community_id" {
		return invalid()
	}
	var communityID string
	if err = decoder.Decode(&communityID); err != nil {
		return invalid()
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return invalid()
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return invalid()
	}
	return communityID, nil
}

func (a *App) communityInfoLocked(ctx context.Context, raw json.RawMessage) (any, error) {
	communityID, err := decodeCommunityInfoRequest(raw)
	if err != nil {
		return nil, err
	}
	if !validCommunityJID(communityID) {
		return nil, fail("invalid_params", "Community ID must be a canonical group JID")
	}
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if a.demo || !isScopedLiveTestTransport(a.transport) {
		return nil, fail("unsupported", "Target-only community info is unavailable outside scoped live-test mode")
	}
	if reason := scopedCapabilityBlockReason(a.transport); reason != "" {
		return nil, fail("unsupported", reason)
	}
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, fail("canceled", "Community info request was canceled")
	}
	transport := a.transport
	provider, ok := transport.(CommunityInfoProvider)
	if !ok {
		return nil, fail("unsupported", "The scoped transport cannot read target-only community metadata")
	}
	if a.state != "connected" || transport == nil {
		return nil, fail("disconnected", "Connect the scoped live-test session before reading community info")
	}
	if !a.communityMu.TryLock() {
		return nil, fail("busy", "Community metadata work is already in progress")
	}
	epoch := a.sessionEpoch
	a.beginOperationLocked()
	a.mu.Unlock()
	info, providerErr := provider.GetCommunityInfo(ctx, communityID)
	a.mu.Lock()
	defer func() {
		a.communityMu.Unlock()
		a.endOperationLocked()
	}()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if epoch != a.sessionEpoch || a.transport != transport || a.state != "connected" || !isScopedLiveTestTransport(a.transport) {
		return nil, fail("stale_session", "Community info belongs to an obsolete WhatsApp session")
	}
	if providerErr != nil {
		return nil, fail("community_info_failed", "Community metadata could not be read")
	}
	if info.ID != communityID {
		return nil, fail("community_info_mismatch", "Community metadata did not match the requested target")
	}
	if !info.IsParent {
		return nil, fail("not_community_parent", "The requested group is not a confirmed community parent")
	}
	if !validCommunityTitle(info.Title) {
		return nil, fail("invalid_metadata", "Community title metadata was invalid")
	}
	return map[string]any{
		"community_id": communityID,
		"title":        strings.TrimSpace(info.Title),
		"is_parent":    true,
		"provenance":   "live_target",
	}, nil
}

func validCommunityTitle(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func initialsForCommunity(title string) string {
	for _, r := range title {
		return string(r)
	}
	return "?"
}

func projectCommunitySnapshot(input []CommunityGroupInfo) (communitySnapshot, error) {
	unique := make(map[string]CommunityGroupInfo, len(input))
	for _, group := range input {
		if !validCommunityJID(group.ID) || !validCommunityTitle(group.Title) {
			return communitySnapshot{}, fmt.Errorf("invalid group metadata")
		}
		group.Title = strings.TrimSpace(group.Title)
		if group.LinkedParentID != "" && !validCommunityJID(group.LinkedParentID) {
			// An unparseable parent cannot establish a membership relationship.
			group.LinkedParentID = ""
		}
		if old, ok := unique[group.ID]; ok {
			if old != group {
				return communitySnapshot{}, fmt.Errorf("conflicting duplicate group metadata")
			}
			continue
		}
		unique[group.ID] = group
	}

	ids := make([]string, 0, len(unique))
	parents := make(map[string]string)
	for id, group := range unique {
		ids = append(ids, id)
		if group.IsParent {
			parents[id] = group.Title
		}
	}
	sort.Strings(ids)
	snapshot := communitySnapshot{groups: make([]CommunityGroupInfo, 0, len(ids))}
	for _, id := range ids {
		group := unique[id]
		snapshot.groups = append(snapshot.groups, group)
		if group.IsParent {
			snapshot.communities = append(snapshot.communities, communityFolder{ID: id, Title: group.Title, Groups: []communityChat{}})
		}
	}
	for _, group := range snapshot.groups {
		communityID, parentTitle := "", ""
		if group.IsParent {
			// A verified parent is also its own actual group chat. Keep that
			// conversation in the folder under the same real chat ID once.
			communityID, parentTitle = group.ID, group.Title
		} else {
			var verified bool
			parentTitle, verified = parents[group.LinkedParentID]
			if group.LinkedParentID == "" || group.LinkedParentID == group.ID || !verified {
				continue
			}
			communityID = group.LinkedParentID
		}
		member := communityMember{chatID: group.ID, chatTitle: group.Title, communityID: communityID}
		if group.IsAnnouncementKnown {
			announcement := group.IsAnnouncement
			member.announcement = &announcement
		}
		snapshot.members = append(snapshot.members, member)
		for i := range snapshot.communities {
			if snapshot.communities[i].ID == communityID {
				chat := communityChat{ID: group.ID, Title: group.Title, CommunityID: communityID, CommunityTitle: parentTitle, IsCommunityAnnouncement: member.announcement}
				snapshot.communities[i].Groups = append(snapshot.communities[i].Groups, chat)
				break
			}
		}
	}
	return snapshot, nil
}

// commitCommunitySnapshotLocked replaces the authoritative cache transactionally.
func (a *App) commitCommunitySnapshotLocked(ctx context.Context, snapshot communitySnapshot) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM community_members"); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM communities"); err != nil {
		return "", err
	}
	for _, folder := range snapshot.communities {
		if _, err = tx.ExecContext(ctx, "INSERT INTO communities(id,title) VALUES(?,?)", folder.ID, folder.Title); err != nil {
			return "", err
		}
	}
	// Only verified community members belong to this refresh operation. Loose
	// groups returned by GetJoinedGroups must not be created or renamed here.
	for _, member := range snapshot.members {
		initials := initialsForCommunity(member.chatTitle)
		if _, err = tx.ExecContext(ctx, `INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,'#54656f',1)
			ON CONFLICT(id) DO UPDATE SET title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE chats.title END,
			initials=CASE WHEN excluded.title<>'' THEN excluded.initials ELSE chats.initials END,is_group=1`, member.chatID, member.chatTitle, initials); err != nil {
			return "", err
		}
	}
	for _, member := range snapshot.members {
		var announcement any
		if member.announcement != nil {
			announcement = *member.announcement
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO community_members(chat_id,chat_title,community_id,is_announcement) VALUES(?,?,?,?)", member.chatID, member.chatTitle, member.communityID, announcement); err != nil {
			return "", err
		}
	}
	refreshedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, "UPDATE community_sync_state SET refreshed_at=?,source='live_cache' WHERE id=1", refreshedAt); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return refreshedAt, nil
}

func (a *App) communityRefreshLocked(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if a.demo {
		return nil, fail("unsupported", "Demo community data is synthetic and cannot be refreshed from WhatsApp")
	}
	if isScopedLiveTestTransport(a.transport) {
		return nil, fail("unsupported", "Broad community refresh is disabled in scoped live-test mode")
	}
	if ctx == nil || ctx.Err() != nil {
		if ctx != nil {
			return nil, ctx.Err()
		}
		return nil, fail("canceled", "Community refresh was canceled")
	}
	if a.state != "connected" || a.transport == nil {
		return nil, fail("disconnected", "Connect WhatsApp before refreshing communities")
	}
	directory, ok := a.transport.(CommunityDirectory)
	if !ok {
		return nil, fail("unsupported", "The active transport cannot refresh community metadata")
	}
	if !a.communityMu.TryLock() {
		return nil, fail("busy", "A community refresh is already in progress")
	}
	a.beginOperationLocked()
	epoch := a.sessionEpoch
	a.mu.Unlock()
	groups, fetchErr := directory.GetJoinedGroups(ctx)
	a.mu.Lock()
	defer func() {
		a.communityMu.Unlock()
		a.endOperationLocked()
	}()
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if epoch != a.sessionEpoch || a.state != "connected" || a.transport == nil {
		return nil, fail("stale_session", "Community refresh belongs to an obsolete WhatsApp session")
	}
	if fetchErr != nil {
		return nil, fail("community_refresh_failed", "Community metadata could not be refreshed; cached data was preserved")
	}
	snapshot, err := projectCommunitySnapshot(groups)
	if err != nil {
		return nil, fail("invalid_metadata", "Community metadata was incomplete or invalid; cached data was preserved")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	refreshedAt, err := a.commitCommunitySnapshotLocked(ctx, snapshot)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fail("community_cache_failed", "Community metadata could not be saved; cached data was preserved")
	}
	a.revision++
	return map[string]any{"refreshed": true, "community_count": len(snapshot.communities), "refreshed_at": refreshedAt, "source": "live_cache"}, nil
}

func encodeCommunityCursor(cursor communityCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor.communityID + "\n" + cursor.chatID))
}

func decodeCommunityCursor(value string) (communityCursor, error) {
	if value == "" {
		return communityCursor{}, nil
	}
	if len(value) > 1024 {
		return communityCursor{}, fail("invalid_params", "Community cursor is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return communityCursor{}, fail("invalid_params", "Community cursor is invalid")
	}
	parts := strings.Split(string(decoded), "\n")
	if len(parts) != 2 || parts[0] == "" || len(parts[0]) > 256 || len(parts[1]) > 256 || strings.ContainsAny(parts[0]+parts[1], "\x00\r") {
		return communityCursor{}, fail("invalid_params", "Community cursor is invalid")
	}
	return communityCursor{communityID: parts[0], chatID: parts[1]}, nil
}

func (a *App) communityListLocked(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor"`
	}
	if err := decode(raw, &params); err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(params.Query) > 128 {
		return nil, fail("invalid_params", "Community search query is too long")
	}
	if params.Limit <= 0 {
		params.Limit = 50
	}
	if params.Limit > communityPageMax {
		params.Limit = communityPageMax
	}
	cursor, err := decodeCommunityCursor(params.Cursor)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT co.id,co.title,COALESCE(cm.chat_id,''),COALESCE(cm.chat_title,''),cm.is_announcement
		FROM communities co LEFT JOIN community_members cm ON cm.community_id=co.id
		WHERE (?='' OR instr(lower(co.title),lower(?))>0)
		AND (?='' OR co.id>? OR (co.id=? AND COALESCE(cm.chat_id,'')>?))
		ORDER BY co.id,COALESCE(cm.chat_id,'') LIMIT ?`, params.Query, params.Query, cursor.communityID, cursor.communityID, cursor.communityID, cursor.chatID, params.Limit+1)
	if err != nil {
		return nil, err
	}
	type row struct {
		communityID, communityTitle, chatID, chatTitle string
		announcement                                   sql.NullInt64
	}
	pageRows := make([]row, 0, params.Limit+1)
	for rows.Next() {
		var item row
		if err = rows.Scan(&item.communityID, &item.communityTitle, &item.chatID, &item.chatTitle, &item.announcement); err != nil {
			rows.Close()
			return nil, err
		}
		pageRows = append(pageRows, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	hasMore := len(pageRows) > params.Limit
	if hasMore {
		pageRows = pageRows[:params.Limit]
	}
	refreshedAt, source := "", "cache_empty"
	if err = a.db.QueryRowContext(ctx, "SELECT refreshed_at,source FROM community_sync_state WHERE id=1").Scan(&refreshedAt, &source); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if source == "" {
		source = "cache_empty"
	}
	for {
		folders := make([]communityFolder, 0, len(pageRows))
		for _, item := range pageRows {
			if len(folders) == 0 || folders[len(folders)-1].ID != item.communityID {
				folders = append(folders, communityFolder{ID: item.communityID, Title: item.communityTitle, Groups: []communityChat{}})
			}
			if item.chatID != "" {
				chat := communityChat{ID: item.chatID, Title: item.chatTitle, CommunityID: item.communityID, CommunityTitle: item.communityTitle}
				if item.announcement.Valid {
					flag := item.announcement.Int64 != 0
					chat.IsCommunityAnnouncement = &flag
				}
				folders[len(folders)-1].Groups = append(folders[len(folders)-1].Groups, chat)
			}
		}
		response := map[string]any{
			"communities":    folders,
			"has_more":       hasMore,
			"next_cursor":    "",
			"source":         source,
			"refreshed_at":   refreshedAt,
			"cached":         source != "cache_empty",
			"live_refreshed": source == "live_cache",
			"connected":      !a.demo && a.state == "connected",
		}
		if hasMore && len(pageRows) > 0 {
			last := pageRows[len(pageRows)-1]
			response["next_cursor"] = encodeCommunityCursor(communityCursor{communityID: last.communityID, chatID: last.chatID})
		}
		encoded, marshalErr := json.Marshal(response)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if len(encoded) <= communityPageBudget {
			return response, nil
		}
		if len(pageRows) <= 1 {
			return nil, fail("response_too_large", "A community entry exceeds the safe response budget")
		}
		// Long escaped names can make a requested page too large; shorten the
		// page and continue from its last included membership on the next call.
		pageRows = pageRows[:len(pageRows)-1]
		hasMore = true
	}
}

func (a *App) communityFeatureReason() string {
	if a.demo {
		return "Showing a synthetic demo community cache; these folders cannot be refreshed from WhatsApp."
	}
	if isScopedLiveTestTransport(a.transport) {
		return "Scoped live-test mode blocks broad community refresh; community.list remains a read-only local cache view."
	}
	_, supportsRefresh := a.transport.(CommunityDirectory)
	var refreshedAt, source string
	if err := a.db.QueryRow("SELECT refreshed_at,source FROM community_sync_state WHERE id=1").Scan(&refreshedAt, &source); err != nil {
		return "Community folders are read-only; cached state is unavailable, and live refresh requires a connected session."
	}
	if source == "live_cache" {
		if a.state == "connected" && supportsRefresh {
			return "Showing a live-refreshed community cache; community.list is read-only, and refresh remains explicitly requested."
		}
		if a.state == "connected" {
			return "Showing cached community folders; the active transport does not support refresh."
		}
		return "Showing offline cached community folders from the last live refresh; refresh requires a connected session."
	}
	if a.state == "connected" && supportsRefresh {
		return "No cached community folders; a connected session can refresh them explicitly."
	}
	if a.state == "connected" {
		return "No cached community folders; the active transport does not support refresh."
	}
	return "No cached community folders while offline; connect a session and explicitly refresh."
}

func (a *App) fillChatCommunity(ctx context.Context, chats []Chat) error {
	if len(chats) == 0 {
		return nil
	}
	placeholders := make([]string, len(chats))
	args := make([]any, len(chats))
	indices := make(map[string]int, len(chats))
	for i, chat := range chats {
		placeholders[i] = "?"
		args[i] = chat.ID
		indices[chat.ID] = i
	}
	rows, err := a.db.QueryContext(ctx, `SELECT cm.chat_id,cm.community_id,co.title,cm.is_announcement
		FROM community_members cm JOIN communities co ON co.id=cm.community_id
		WHERE cm.chat_id IN (`+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var chatID, communityID, title string
		var announcement sql.NullInt64
		if err = rows.Scan(&chatID, &communityID, &title, &announcement); err != nil {
			return err
		}
		if i, ok := indices[chatID]; ok {
			chats[i].CommunityID, chats[i].CommunityTitle = communityID, title
			if announcement.Valid {
				flag := announcement.Int64 != 0
				chats[i].IsCommunityAnnouncement = &flag
			}
		}
	}
	return rows.Err()
}
