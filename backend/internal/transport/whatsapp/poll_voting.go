package whatsapp

import (
	"bytes"
	"context"
	"fmt"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"time"
)

type pollRejected struct{ err error }

func (e pollRejected) Error() string          { return e.err.Error() }
func (e pollRejected) PollVoteRejected() bool { return true }

func (a *Adapter) buildPollVote(ctx context.Context, c *whatsmeow.Client, target app.Message, labels []string) (*waE2E.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	chat, err := types.ParseJID(target.ChatID)
	if err != nil || chat.IsEmpty() {
		return nil, fmt.Errorf("invalid stored poll chat")
	}
	sender, err := types.ParseJID(target.SenderID)
	if err != nil || sender.IsEmpty() {
		return nil, fmt.Errorf("original poll sender unavailable")
	}
	if target.Kind != "poll" || target.Deleted || target.ID == "" {
		return nil, fmt.Errorf("invalid original poll")
	}
	info := types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: target.Outgoing, IsGroup: chat.Server == types.GroupServer}, ID: target.ID, Timestamp: time.UnixMilli(target.Timestamp)}
	return c.BuildPollVote(ctx, &info, labels)
}

func (a *Adapter) SendPollVote(ctx context.Context, target app.Message, labels []string, id string) (int64, error) {
	if err := a.requireScopedMutationTarget(target.ChatID, target.ID); err != nil {
		return 0, pollRejected{err}
	}
	a.mu.Lock()
	c := a.client
	a.mu.Unlock()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return 0, pollRejected{fmt.Errorf("not connected")}
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	message, err := a.buildPollVote(bounded, c, target, labels)
	if err != nil {
		return 0, pollRejected{fmt.Errorf("poll encryption unavailable")}
	}
	return sendProtocolAtBoundary(bounded, a.scope, c, a.scopeSelf(c), target.ChatID, message, id, func() bool { return a.current(c) })
}

func (a *Adapter) receivePollVote(ctx context.Context, c *whatsmeow.Client, e *events.Message) {
	if ctx.Err() != nil || c == nil || e.IsViewOnce || e.IsViewOnceV2 || e.IsViewOnceV2Extension || e.IsEphemeral {
		return
	}
	update := e.Message.GetPollUpdateMessage()
	key := update.GetPollCreationMessageKey()
	if key == nil || key.GetID() == "" || key.GetRemoteJID() != e.Info.Chat.String() {
		return
	}
	target, err := a.app.PollVoteTarget(ctx, e.Info.Chat.String(), key.GetID())
	if err != nil {
		return
	}
	// FromMe describes whether the original poll was sent by this account; it
	// does not identify the sender of this vote update. Validate it against the
	// stored poll and bind group creator identity through Participant instead.
	if key.GetFromMe() != target.Outgoing {
		return
	}
	if e.Info.IsGroup {
		if key.GetParticipant() != target.SenderID {
			return
		}
	} else if key.GetParticipant() != "" {
		return
	}
	vote, err := c.DecryptPollVote(ctx, e)
	if err != nil {
		return
	}
	hashes := whatsmeow.HashPollOptions(target.Poll.Options)
	if len(vote.GetSelectedOptions()) > len(hashes) {
		return
	}
	indices := make([]int, 0, len(vote.GetSelectedOptions()))
	for _, selected := range vote.GetSelectedOptions() {
		found := -1
		for i, hash := range hashes {
			if bytes.Equal(hash, selected) {
				if found != -1 {
					return
				}
				found = i
			}
		}
		if found < 0 {
			return
		}
		indices = append(indices, found)
	}
	_ = a.app.ApplyPollVote(ctx, e.Info.Chat.String(), target.ID, e.Info.Sender.ToNonAD().String(), e.Info.ID, update.GetSenderTimestampMS(), indices)
}
