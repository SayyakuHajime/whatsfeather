package app

import (
	"context"
	"encoding/json"
	"time"
)

type demoPresenceProjection struct {
	State        string `json:"state"`
	ExpiresAtMS  int64  `json:"expires_at_ms,omitempty"`
	LastSeenAtMS int64  `json:"last_seen_at_ms,omitempty"`
	Provenance   string `json:"provenance"`
}

func syntheticDemoPresence(state string, now time.Time) demoPresenceProjection {
	projection := demoPresenceProjection{State: state, Provenance: "demo_synthetic"}
	switch state {
	case "online", "typing":
		projection.ExpiresAtMS = now.Add(20 * time.Second).UnixMilli()
	case "offline":
		projection.LastSeenAtMS = now.Add(-5 * time.Minute).UnixMilli()
	default:
		projection.State = "unavailable"
	}
	return projection
}

func demoConversationDetails(chat Chat) (string, any, bool) {
	now := time.Now()
	if chat.ID == "demo:alex" && !chat.IsGroup {
		return "contact_details", map[string]any{
			"display_name": "Alex Morgan",
			"phone":        "+1-202-555-0147",
			"about":        "Synthetic demo contact for local UI tests.",
			"presence":     syntheticDemoPresence("online", now),
		}, true
	}
	if chat.ID == "demo:studio" && chat.IsGroup {
		participants := []map[string]any{
			{"display_name": "Alex Morgan", "role": "Admin", "presence": syntheticDemoPresence("online", now)},
			{"display_name": "Maya Chen", "role": "Member", "presence": syntheticDemoPresence("typing", now)},
			{"display_name": "Sam Rivera", "role": "Member", "presence": syntheticDemoPresence("offline", now)},
		}
		return "group_details", map[string]any{
			"description":       "Synthetic group for local UI tests.",
			"participant_count": len(participants),
			"participants":      participants,
		}, true
	}
	return "", nil, false
}

func (a *App) chatInfo(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		ChatID string `json:"chat_id"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	if !validHubChat(p.ChatID) {
		return nil, fail("invalid_params", "Select an exact chat ID")
	}
	if err := a.chatExists(ctx, p.ChatID); err != nil {
		return nil, err
	}
	var c Chat
	if err := a.db.QueryRowContext(ctx, `SELECT id,title,is_group FROM chats WHERE id=?`, p.ChatID).Scan(&c.ID, &c.Title, &c.IsGroup); err != nil {
		return nil, err
	}
	var messages, media int64
	// Counts read row metadata only; deleted and protected placeholders are not
	// counted as retained content. They are not full WhatsApp history totals.
	if err := a.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN `+cachedMediaMessage+` THEN 1 ELSE 0 END),0) FROM messages m WHERE m.chat_id=? AND `+cachedVisibleMessage, p.ChatID).Scan(&messages, &media); err != nil {
		return nil, err
	}
	chats := []Chat{c}
	if err := a.fillChatOrganization(ctx, chats); err != nil {
		return nil, err
	}
	if err := a.fillChatCommunity(ctx, chats); err != nil {
		return nil, err
	}
	c = chats[0]
	if c.ListIDs == nil {
		c.ListIDs = []string{}
	}
	fields := []string{"id", "title", "is_group", "message_count", "media_count", "archived", "pinned", "muted", "marked_unread", "favorite", "list_ids"}
	result := map[string]any{"id": c.ID, "title": c.Title, "is_group": c.IsGroup, "message_count": messages, "media_count": media, "archived": c.Archived, "pinned": c.Pinned, "muted": c.Muted, "marked_unread": c.UnreadMark, "favorite": c.Favorite, "list_ids": c.ListIDs, "provenance": "local_cached", "organization_provenance": "local_only", "count_scope": "local_retained_non_deleted_non_protected"}
	if a.demo {
		if field, details, ok := demoConversationDetails(c); ok {
			result[field] = details
			result["details_provenance"] = "demo_synthetic"
			fields = append(fields, field, "details_provenance")
		} else if details, ok, detailsErr := a.demoGroupDetails(ctx, c.ID); detailsErr != nil {
			return nil, detailsErr
		} else if ok {
			result["group_details"] = details
			result["details_provenance"] = "demo_synthetic"
			fields = append(fields, "group_details", "details_provenance")
		}
	}
	if c.CommunityID != "" {
		result["community_id"] = c.CommunityID
		result["community_title"] = c.CommunityTitle
		fields = append(fields, "community_id", "community_title")
		if c.IsCommunityAnnouncement != nil {
			result["is_community_announcement"] = *c.IsCommunityAnnouncement
			fields = append(fields, "is_community_announcement")
		}
	}
	result["available_fields"] = fields
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if len(encoded) > pageResultBudget {
		return nil, fail("chat_info_too_large", "Cached chat info exceeds the local preview limit")
	}
	return result, nil
}
