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

func buttonsResponseTextValid(value string, maxRunes int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func buildButtonsResponseMessage(target app.Message, buttonID, displayText string) (*waE2E.Message, error) {
	if target.Kind != "buttons" || target.Deleted || target.Outgoing || target.ID == "" ||
		!buttonsResponseTextValid(buttonID, 256) ||
		!buttonsResponseTextValid(displayText, app.ButtonsLabelMaxRunes) {
		return nil, fmt.Errorf("invalid ButtonsMessage response selection")
	}
	key, err := messageKey(target.ChatID, target.ID, target.Outgoing, target.SenderID)
	if err != nil {
		return nil, err
	}
	contextInfo := &waE2E.ContextInfo{StanzaID: proto.String(key.GetID())}
	if key.GetParticipant() != "" {
		contextInfo.Participant = proto.String(key.GetParticipant())
	}
	responseType := waE2E.ButtonsResponseMessage_DISPLAY_TEXT
	response := &waE2E.ButtonsResponseMessage{
		Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: displayText},
		SelectedButtonID: proto.String(buttonID),
		ContextInfo:      contextInfo,
		Type:             &responseType,
	}
	return &waE2E.Message{ButtonsResponseMessage: response}, nil
}
