package whatsapp

import (
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

const maxCallLogDurationSeconds int64 = 24 * 60 * 60

func callLogSummary(message *waE2E.CallLogMessage) string {
	if message == nil || message.CallOutcome == nil {
		return "Call details unavailable"
	}

	callKind := "voice call"
	if message.GetIsVideo() {
		callKind = "video call"
	}
	switch message.GetCallType() {
	case waE2E.CallLogMessage_REGULAR:
	case waE2E.CallLogMessage_SCHEDULED_CALL:
		callKind = "scheduled " + callKind
	case waE2E.CallLogMessage_VOICE_CHAT:
		callKind = "voice chat"
	default:
		return "Call details unavailable"
	}

	var summary string
	switch message.GetCallOutcome() {
	case waE2E.CallLogMessage_CONNECTED:
		summary = strings.ToUpper(callKind[:1]) + callKind[1:]
	case waE2E.CallLogMessage_MISSED:
		summary = "Missed " + callKind
	case waE2E.CallLogMessage_FAILED:
		summary = "Failed " + callKind
	case waE2E.CallLogMessage_REJECTED:
		summary = "Declined " + callKind
	case waE2E.CallLogMessage_ACCEPTED_ELSEWHERE:
		summary = "Answered elsewhere"
	case waE2E.CallLogMessage_ONGOING:
		summary = "Ongoing " + callKind
	case waE2E.CallLogMessage_SILENCED_BY_DND:
		summary = "Silenced by Do Not Disturb"
	case waE2E.CallLogMessage_SILENCED_UNKNOWN_CALLER:
		summary = "Silenced unknown caller"
	default:
		return "Call details unavailable"
	}

	if message.GetCallOutcome() == waE2E.CallLogMessage_CONNECTED {
		if duration := message.GetDurationSecs(); duration > 0 && duration <= maxCallLogDurationSeconds {
			summary += " · " + formatCallDuration(duration)
		}
	}
	return summary
}

func formatCallDuration(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("0:%02d", seconds)
	}
	if seconds < 60*60 {
		return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", seconds/3600, (seconds/60)%60, seconds%60)
}
