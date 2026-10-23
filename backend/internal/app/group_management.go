package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxDemoGroupTitleRunes       = 128
	maxDemoGroupDescriptionBytes = 2048
	maxDemoGroupMembers          = 3
)

type demoGroupParticipant struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Self        bool   `json:"is_self,omitempty"`
}

type demoGroupDetails struct {
	Description      string                 `json:"description"`
	ParticipantCount int                    `json:"participant_count"`
	Participants     []demoGroupParticipant `json:"participants"`
	MembershipState  string                 `json:"membership_state"`
	Managed          bool                   `json:"managed"`
	CanManage        bool                   `json:"can_manage"`
}

func demoParticipantName(id string) (string, bool) {
	switch id {
	case "demo:participant:alex":
		return "Alex Morgan", true
	case "demo:participant:maya":
		return "Maya Chen", true
	case "demo:participant:sam":
		return "Sam Rivera", true
	default:
		return "", false
	}
}

func validDemoGroupText(value string, maxRunes int, maxBytes int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || len(value) > maxBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func demoGroupID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "demo:group:" + hex.EncodeToString(id[:]), nil
}

// demoGroupDetails exposes only locally-created synthetic groups. No live
// group metadata is inferred from the community cache or a service response.
func (a *App) demoGroupDetails(ctx context.Context, groupID string) (demoGroupDetails, bool, error) {
	var details demoGroupDetails
	if err := a.db.QueryRowContext(ctx, `SELECT g.description,g.membership_state
		FROM demo_groups g JOIN chats c ON c.id=g.group_id WHERE g.group_id=?`, groupID).
		Scan(&details.Description, &details.MembershipState); err != nil {
		if err == sql.ErrNoRows {
			return demoGroupDetails{}, false, nil
		}
		return demoGroupDetails{}, false, err
	}
	rows, err := a.db.QueryContext(ctx, `SELECT participant_id,display_name,role
		FROM demo_group_participants WHERE group_id=?
		ORDER BY CASE role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,display_name,participant_id`, groupID)
	if err != nil {
		return demoGroupDetails{}, false, err
	}
	details.Participants = []demoGroupParticipant{}
	for rows.Next() {
		var participant demoGroupParticipant
		var role string
		if err = rows.Scan(&participant.ID, &participant.DisplayName, &role); err != nil {
			rows.Close()
			return demoGroupDetails{}, false, err
		}
		switch role {
		case "owner":
			participant.Role = "Owner"
			participant.Self = true
		case "admin":
			participant.Role = "Admin"
		default:
			participant.Role = "Member"
		}
		details.Participants = append(details.Participants, participant)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return demoGroupDetails{}, false, err
	}
	if err = rows.Close(); err != nil {
		return demoGroupDetails{}, false, err
	}
	details.ParticipantCount = len(details.Participants)
	details.Managed = true
	details.CanManage = details.MembershipState == "joined"
	return details, true, nil
}

func (a *App) createDemoGroup(ctx context.Context, raw json.RawMessage) (any, error) {
	if !a.demo {
		return nil, fail("unsupported", "Group management is unavailable outside the synthetic demo")
	}
	if a.closing || a.closed {
		return nil, fail("app_closing", "App is closing")
	}
	var p struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		MemberIDs   []string `json:"member_ids"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	p.Title = strings.TrimSpace(p.Title)
	p.Description = strings.TrimSpace(p.Description)
	if p.Title == "" || !validDemoGroupText(p.Title, maxDemoGroupTitleRunes, 512) || !validDemoGroupText(p.Description, 1024, maxDemoGroupDescriptionBytes) {
		return nil, fail("invalid_params", "Group title or description is invalid")
	}
	if len(p.MemberIDs) < 1 || len(p.MemberIDs) > maxDemoGroupMembers {
		return nil, fail("invalid_params", "Choose one to three synthetic participants")
	}
	seen := map[string]bool{}
	participants := make([]demoGroupParticipant, 0, len(p.MemberIDs)+1)
	participants = append(participants, demoGroupParticipant{ID: "demo:account", DisplayName: "Demo Account", Role: "Owner", Self: true})
	for _, id := range p.MemberIDs {
		name, ok := demoParticipantName(id)
		if !ok || seen[id] {
			return nil, fail("invalid_params", "Only distinct synthetic participants can be added")
		}
		seen[id] = true
		participants = append(participants, demoGroupParticipant{ID: id, DisplayName: name, Role: "Member"})
	}
	groupID, err := demoGroupID()
	if err != nil {
		return nil, fail("group_id_unavailable", "A local synthetic group ID could not be generated")
	}
	initials := initialsForCommunity(p.Title)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO chats(id,title,initials,color,is_group) VALUES(?,?,?,'#54656f',1)`, groupID, p.Title, initials); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO demo_groups(group_id,description,membership_state) VALUES(?,?,'joined')`, groupID, p.Description); err != nil {
		return nil, err
	}
	for _, participant := range participants {
		role := "member"
		if participant.Self {
			role = "owner"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO demo_group_participants(group_id,participant_id,display_name,role) VALUES(?,?,?,?)`, groupID, participant.ID, participant.DisplayName, role); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	a.revision++
	return map[string]any{"chat": Chat{ID: groupID, Title: p.Title, Initials: initials, Color: "#54656f", IsGroup: true}, "group_id": groupID, "membership_state": "joined", "provenance": "demo_synthetic"}, nil
}

func (a *App) demoGroupMutationUnavailable(method string) error {
	return fail("unsupported", fmt.Sprintf("%s is not available for live groups", method))
}

type demoGroupQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (a *App) requireDemoGroupManager(ctx context.Context, q demoGroupQueryer, groupID, method string) error {
	if !a.demo {
		return a.demoGroupMutationUnavailable(method)
	}
	var state string
	if err := q.QueryRowContext(ctx, "SELECT membership_state FROM demo_groups WHERE group_id=?", groupID).Scan(&state); err != nil {
		if err == sql.ErrNoRows {
			return fail("not_found", "Only a locally-created synthetic group can be managed")
		}
		return err
	}
	if state != "joined" {
		return fail("not_member", "Join the synthetic group before managing it")
	}
	var role string
	if err := q.QueryRowContext(ctx, "SELECT role FROM demo_group_participants WHERE group_id=? AND participant_id='demo:account'", groupID).Scan(&role); err != nil {
		if err == sql.ErrNoRows {
			return fail("not_member", "The synthetic demo identity is not a group participant")
		}
		return err
	}
	if role != "owner" && role != "admin" {
		return fail("forbidden", "The synthetic demo identity cannot manage this group")
	}
	return nil
}

func (a *App) demoGroupActionResult(ctx context.Context, groupID string, includeChat bool) (map[string]any, error) {
	details, ok, err := a.demoGroupDetails(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fail("not_found", "Only a locally-created synthetic group can be managed")
	}
	result := map[string]any{"group_id": groupID, "group_details": details, "membership_state": details.MembershipState, "provenance": "demo_synthetic"}
	if includeChat {
		var chat Chat
		if err = a.db.QueryRowContext(ctx, `SELECT id,title,initials,color,is_group FROM chats WHERE id=?`, groupID).Scan(&chat.ID, &chat.Title, &chat.Initials, &chat.Color, &chat.IsGroup); err != nil {
			return nil, err
		}
		result["chat"] = chat
	}
	return result, nil
}

func (a *App) updateDemoGroup(ctx context.Context, raw json.RawMessage) (any, error) {
	if !a.demo {
		return nil, a.demoGroupMutationUnavailable("group.update")
	}
	var p struct {
		GroupID     string  `json:"group_id"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	if !validHubChat(p.GroupID) || (p.Title == nil && p.Description == nil) {
		return nil, fail("invalid_params", "Select a synthetic group and at least one field to update")
	}
	if p.Title != nil {
		*p.Title = strings.TrimSpace(*p.Title)
		if *p.Title == "" || !validDemoGroupText(*p.Title, maxDemoGroupTitleRunes, 512) {
			return nil, fail("invalid_params", "Group title is invalid")
		}
	}
	if p.Description != nil {
		*p.Description = strings.TrimSpace(*p.Description)
		if !validDemoGroupText(*p.Description, 1024, maxDemoGroupDescriptionBytes) {
			return nil, fail("invalid_params", "Group description is invalid")
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = a.requireDemoGroupManager(ctx, tx, p.GroupID, "group.update"); err != nil {
		return nil, err
	}
	if p.Title != nil {
		if _, err = tx.ExecContext(ctx, "UPDATE chats SET title=?,initials=? WHERE id=?", *p.Title, initialsForCommunity(*p.Title), p.GroupID); err != nil {
			return nil, err
		}
	}
	if p.Description != nil {
		if _, err = tx.ExecContext(ctx, "UPDATE demo_groups SET description=? WHERE group_id=?", *p.Description, p.GroupID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	a.revision++
	return a.demoGroupActionResult(ctx, p.GroupID, true)
}

func (a *App) manageDemoGroupParticipants(ctx context.Context, raw json.RawMessage) (any, error) {
	if !a.demo {
		return nil, a.demoGroupMutationUnavailable("group.participants")
	}
	var p struct {
		GroupID       string `json:"group_id"`
		Action        string `json:"action"`
		ParticipantID string `json:"participant_id"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	if !validHubChat(p.GroupID) {
		return nil, fail("invalid_params", "Select an exact synthetic group ID")
	}
	if _, ok := demoParticipantName(p.ParticipantID); !ok {
		return nil, fail("invalid_params", "Only a synthetic participant can be changed")
	}
	if p.Action != "add" && p.Action != "remove" && p.Action != "promote" && p.Action != "demote" {
		return nil, fail("invalid_params", "Unsupported participant action")
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = a.requireDemoGroupManager(ctx, tx, p.GroupID, "group.participants"); err != nil {
		return nil, err
	}
	var role string
	lookupErr := tx.QueryRowContext(ctx, "SELECT role FROM demo_group_participants WHERE group_id=? AND participant_id=?", p.GroupID, p.ParticipantID).Scan(&role)
	switch p.Action {
	case "add":
		if lookupErr == nil {
			return nil, fail("conflict", "Participant is already in this synthetic group")
		}
		if lookupErr != sql.ErrNoRows {
			return nil, lookupErr
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM demo_group_participants WHERE group_id=? AND role<>'owner'", p.GroupID).Scan(&count); err != nil {
			return nil, err
		}
		if count >= maxDemoGroupMembers {
			return nil, fail("limit_reached", "Synthetic groups are limited to three added participants")
		}
		name, _ := demoParticipantName(p.ParticipantID)
		if _, err = tx.ExecContext(ctx, "INSERT INTO demo_group_participants(group_id,participant_id,display_name,role) VALUES(?,?,?,'member')", p.GroupID, p.ParticipantID, name); err != nil {
			return nil, err
		}
	case "remove":
		if lookupErr == sql.ErrNoRows {
			return nil, fail("not_found", "Participant is not in this synthetic group")
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		if role == "owner" {
			return nil, fail("forbidden", "The synthetic group owner cannot be removed")
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM demo_group_participants WHERE group_id=? AND participant_id=?", p.GroupID, p.ParticipantID); err != nil {
			return nil, err
		}
	case "promote", "demote":
		if lookupErr == sql.ErrNoRows {
			return nil, fail("not_found", "Participant is not in this synthetic group")
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		if role == "owner" {
			return nil, fail("forbidden", "The synthetic group owner role cannot be changed")
		}
		from, to := "member", "admin"
		if p.Action == "demote" {
			from, to = "admin", "member"
		}
		if role != from {
			return nil, fail("conflict", "Participant already has a different group role")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE demo_group_participants SET role=? WHERE group_id=? AND participant_id=?", to, p.GroupID, p.ParticipantID); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	a.revision++
	return a.demoGroupActionResult(ctx, p.GroupID, false)
}

func (a *App) changeDemoGroupMembership(ctx context.Context, raw json.RawMessage, target, method string) (any, error) {
	if !a.demo {
		return nil, a.demoGroupMutationUnavailable(method)
	}
	var p struct {
		GroupID string `json:"group_id"`
	}
	if err := decodeHubParams(raw, &p); err != nil {
		return nil, err
	}
	if !validHubChat(p.GroupID) {
		return nil, fail("invalid_params", "Select an exact synthetic group ID")
	}
	var current string
	if err := a.db.QueryRowContext(ctx, "SELECT membership_state FROM demo_groups WHERE group_id=?", p.GroupID).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return nil, fail("not_found", "Only a locally-created synthetic group can change membership")
		}
		return nil, err
	}
	if target != "joined" && target != "left" {
		return nil, fail("invalid_action", "Unsupported synthetic membership state")
	}
	changed := current != target
	if changed {
		if _, err := a.db.ExecContext(ctx, "UPDATE demo_groups SET membership_state=? WHERE group_id=?", target, p.GroupID); err != nil {
			return nil, err
		}
		a.revision++
	}
	result, err := a.demoGroupActionResult(ctx, p.GroupID, false)
	if err != nil {
		return nil, err
	}
	result["changed"] = changed
	return result, nil
}
