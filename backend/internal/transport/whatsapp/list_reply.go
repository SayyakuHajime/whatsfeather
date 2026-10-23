package whatsapp

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func listReplyTextValid(value string, maxRunes int, required bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || (required && strings.TrimSpace(value) == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func buildListResponseMessage(target app.Message, rowID, title, description string) (*waE2E.Message, error) {
	if target.Kind != "list" || target.Deleted || target.Outgoing || target.ID == "" ||
		rowID == "" || len(rowID) > 256 || !listReplyTextValid(rowID, 256, true) ||
		!listReplyTextValid(title, app.ListRowTitleMaxRunes, true) ||
		!listReplyTextValid(description, app.ListRowDescriptionMax, false) {
		return nil, fmt.Errorf("invalid list response selection")
	}
	key, err := messageKey(target.ChatID, target.ID, target.Outgoing, target.SenderID)
	if err != nil {
		return nil, err
	}
	contextInfo := &waE2E.ContextInfo{StanzaID: proto.String(key.GetID())}
	if key.GetParticipant() != "" {
		contextInfo.Participant = proto.String(key.GetParticipant())
	}
	listType := waE2E.ListResponseMessage_SINGLE_SELECT
	response := &waE2E.ListResponseMessage{
		Title:             proto.String(title),
		ListType:          &listType,
		SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String(rowID)},
		ContextInfo:       contextInfo,
		Description:       proto.String(description),
	}
	return &waE2E.Message{ListResponseMessage: response}, nil
}
