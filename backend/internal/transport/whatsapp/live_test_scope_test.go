package whatsapp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	whatsmeowstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	scopeTestChat  = "15550123456@s.whatsapp.net"
	scopeTestOther = "15550999999@s.whatsapp.net"
)

func writeScopeConfig(t *testing.T, data string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live-test-scope.json")
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func scopeTestNow() time.Time { return time.Date(2026, 10, 23, 9, 25, 0, 0, time.UTC) }

func validScopeJSON(since string) string {
	return `{"since":"` + since + `","chats":["` + scopeTestChat + `"],"include_self":true}`
}

func TestLiveTestScopeStrictValidationAndPrivateFile(t *testing.T) {
	now := scopeTestNow()
	validPath := writeScopeConfig(t, validScopeJSON("2026-10-20T09:25:00Z"), 0600)
	if _, err := loadLiveTestScopeAt(validPath, now); err != nil {
		t.Fatalf("valid synthetic scope rejected: %v", err)
	}

	valid := validScopeJSON("2026-10-20T09:25:00Z")
	cases := map[string]string{
		"duplicate field":  `{"since":"2026-10-20T09:25:00Z","since":"2026-10-20T09:25:00Z","chats":["` + scopeTestChat + `"],"include_self":false}`,
		"unknown field":    valid[:len(valid)-1] + `,"extra":true}`,
		"trailing JSON":    valid + ` {}`,
		"missing bool":     `{"since":"2026-10-20T09:25:00Z","chats":["` + scopeTestChat + `"]}`,
		"empty chats":      `{"since":"2026-10-20T09:25:00Z","chats":[],"include_self":false}`,
		"duplicate chat":   `{"since":"2026-10-20T09:25:00Z","chats":["` + scopeTestChat + `","` + scopeTestChat + `"],"include_self":false}`,
		"invalid jid":      `{"since":"2026-10-20T09:25:00Z","chats":["not-a-jid"],"include_self":false}`,
		"noncanonical jid": `{"since":"2026-10-20T09:25:00Z","chats":["` + strings.ToUpper(scopeTestChat) + `"],"include_self":false}`,
		"broadcast jid":    `{"since":"2026-10-20T09:25:00Z","chats":["status@broadcast"],"include_self":false}`,
		"unknown server":   `{"since":"2026-10-20T09:25:00Z","chats":["synthetic@invalid.example"],"include_self":false}`,
		"too many chats":   `{"since":"2026-10-20T09:25:00Z","chats":["15550123456@s.whatsapp.net","15550123457@s.whatsapp.net","15550123458@s.whatsapp.net","15550123459@s.whatsapp.net","15550123460@s.whatsapp.net","15550123461@s.whatsapp.net"],"include_self":false}`,
		"future since":     validScopeJSON("2026-10-23T09:25:01Z"),
		"stale since":      validScopeJSON("2026-10-16T09:24:59Z"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeScopeConfig(t, data, 0600)
			if _, err := loadLiveTestScopeAt(path, now); err == nil || err.Error() != errInvalidLiveTestScope.Error() {
				t.Fatalf("invalid scope was not rejected generically: %v", err)
			}
		})
	}

	wide := writeScopeConfig(t, valid, 0640)
	if _, err := loadLiveTestScopeAt(wide, now); err == nil {
		t.Fatal("group-readable scope file was accepted")
	}
	executable := writeScopeConfig(t, valid, 0700)
	if _, err := loadLiveTestScopeAt(executable, now); err == nil {
		t.Fatal("executable scope file was accepted")
	}
	link := filepath.Join(t.TempDir(), "scope-link.json")
	if err := os.Symlink(validPath, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLiveTestScopeAt(link, now); err == nil {
		t.Fatal("scope symlink was accepted")
	}
	overlong := writeScopeConfig(t, strings.Repeat(" ", liveTestScopeMaxBytes+1), 0600)
	if _, err := loadLiveTestScopeAt(overlong, now); err == nil {
		t.Fatal("oversize scope file was accepted")
	}
}

func TestLiveTestScopeHistoryAndReceiptFiltersBeforePersistence(t *testing.T) {
	ctx := context.Background()
	profile := filepath.Join(t.TempDir(), "profile")
	store, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-48 * time.Hour)
	scope := &LiveTestScope{since: since, chats: map[string]struct{}{scopeTestChat: {}}, includeSelf: true, acceptedMessages: make(map[liveTestMessageKey]struct{})}
	adapter := NewWithLiveTestScope(store, t.TempDir(), scope)
	self, err := types.ParseJID(scopeTestOther)
	if err != nil {
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(&whatsmeowstore.Device{ID: &self}, nil)
	adapter.client = client
	chat, err := types.ParseJID(scopeTestChat)
	if err != nil {
		t.Fatal(err)
	}
	other, err := types.ParseJID(scopeTestOther)
	if err != nil {
		t.Fatal(err)
	}

	// The normal-message guards reject the other chat and out-of-window timestamps.
	for _, item := range []struct {
		chat types.JID
		id   string
		ts   time.Time
	}{
		{other, "synthetic-outside-chat", now},
		{chat, "synthetic-too-old", since.Add(-time.Second)},
		{chat, "synthetic-future", now.Add(time.Second)},
	} {
		adapter.event(ctx, nil, &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: item.chat}, ID: item.id, Timestamp: item.ts},
			Message: &waE2E.Message{Conversation: proto.String("must not be persisted")},
		})
	}

	// A disallowed conversation and allowed conversations with old/future
	// timestamp metadata must be discarded before ParseWebMessage (c is nil).
	webMessage := func(chatID, id string, stamp time.Time) *waWeb.WebMessageInfo {
		return &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(chatID), ID: proto.String(id), FromMe: proto.Bool(false)},
			Message:          &waE2E.Message{Conversation: proto.String("history body must not be parsed")},
			MessageTimestamp: proto.Uint64(uint64(stamp.Unix())),
		}
	}
	adapter.event(ctx, nil, &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{
		{ID: proto.String(scopeTestOther), Messages: []*waHistorySync.HistorySyncMsg{{Message: webMessage(scopeTestOther, "synthetic-history-other", now)}}},
		{ID: proto.String(scopeTestChat), Messages: []*waHistorySync.HistorySyncMsg{{Message: webMessage(scopeTestChat, "synthetic-history-old", since.Add(-time.Second))}}},
		{ID: proto.String(scopeTestChat), Messages: []*waHistorySync.HistorySyncMsg{{Message: webMessage(scopeTestChat, "synthetic-history-future", now.Add(time.Second))}}},
	}}})
	adapter.event(ctx, client, &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{
		{ID: proto.String(scopeTestChat), Messages: []*waHistorySync.HistorySyncMsg{{Message: webMessage(scopeTestChat, "synthetic-history-in-window", now.Add(-time.Hour))}}},
	}}})

	// Only a message accepted from this run can be changed by a receipt.
	if err := store.Record(ctx, app.Message{ID: "synthetic-before-run", ChatID: scopeTestChat, Body: "synthetic prior", Timestamp: now.UnixMilli(), Outgoing: true, Status: "accepted", Kind: "text"}, "Synthetic"); err != nil {
		t.Fatal(err)
	}
	adapter.event(ctx, client, &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, IsFromMe: true}, ID: "synthetic-this-run", Timestamp: now.Add(-time.Minute)},
		Message: &waE2E.Message{Conversation: proto.String("synthetic outgoing")},
	})
	adapter.event(ctx, client, &events.Receipt{MessageSource: types.MessageSource{Chat: chat}, MessageIDs: []string{"synthetic-this-run", "synthetic-before-run"}, Type: types.ReceiptTypeDelivered})
	adapter.event(ctx, client, &events.Receipt{MessageSource: types.MessageSource{Chat: other}, MessageIDs: []string{"synthetic-this-run"}, Type: types.ReceiptTypeRead})

	result, err := store.Handle(ctx, "message.list", []byte(`{"chat_id":"`+scopeTestChat+`","limit":20}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages []app.Message `json:"messages"`
	}
	if err = json.Unmarshal(encoded, &page); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, message := range page.Messages {
		statuses[message.ID] = message.Status
	}
	if len(statuses) != 3 || statuses["synthetic-this-run"] != "delivered" || statuses["synthetic-before-run"] != "accepted" || statuses["synthetic-history-in-window"] != "received" {
		t.Fatalf("scope failed to filter persistence/receipts: %#v", statuses)
	}

	// An explicitly requested history window must be both allowed and in-range.
	outsideErr := adapter.RequestHistory(ctx, scopeTestOther, app.HistoryAnchor{Timestamp: now.UnixMilli()}, 50)
	if outsideErr == nil || !strings.Contains(outsideErr.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped history request was not blocked before dispatch: %v", outsideErr)
	}
	oldErr := adapter.RequestHistory(ctx, scopeTestChat, app.HistoryAnchor{Timestamp: since.Add(-time.Second).UnixMilli()}, 50)
	if oldErr == nil || !strings.Contains(oldErr.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped history request was not blocked before dispatch: %v", oldErr)
	}
}

func TestLiveTestScopeIncludeSelfUsesOnlyConnectedIdentity(t *testing.T) {
	scope := &LiveTestScope{chats: map[string]struct{}{}, includeSelf: true}
	if scope.allowsChat(scopeTestChat, "") || !scope.allowsChat(scopeTestChat, scopeTestChat) {
		t.Fatal("include_self was not bound exclusively to the supplied connected identity")
	}
	jid, err := types.ParseJID(scopeTestChat)
	if err != nil {
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(&whatsmeowstore.Device{ID: &jid}, nil)
	adapter := NewWithLiveTestScope(nil, t.TempDir(), scope)
	if adapter.scopeSelf(client) != scopeTestChat {
		t.Fatal("scope self was not derived from the stored device identity before Connected")
	}
}

func TestLiveTestScopeRejectsDuplicateNestedJSONKeys(t *testing.T) {
	if err := rejectDuplicateJSONKeys([]byte(`{"outer":{"x":1,"x":2}}`)); err == nil {
		t.Fatal("nested duplicate JSON key was accepted")
	}
	if err := rejectDuplicateJSONKeys([]byte(`{"x":1} {}`)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestLiveTestScopeWindowBoundaries(t *testing.T) {
	now := scopeTestNow()
	path := writeScopeConfig(t, validScopeJSON(now.Add(-liveTestScopeWindowDuration).Format(time.RFC3339)), 0600)
	scope, err := loadLiveTestScopeAt(path, now)
	if err != nil {
		t.Fatalf("exact seven-day boundary was rejected: %v", err)
	}
	if !scope.allowsTimestamp(scope.since, now) || scope.allowsTimestamp(now.Add(time.Nanosecond), now) || scope.allowsTimestamp(scope.since.Add(-time.Nanosecond), now) {
		t.Fatal("scope timestamp boundary is incorrect")
	}
}

func TestLiveTestScopeBoundsManualMediaToFiveItems(t *testing.T) {
	scope := &LiveTestScope{}
	for i := 0; i < liveTestScopeMaxMediaItems; i++ {
		if !scope.reserveManualMediaItem() {
			t.Fatalf("manual media item %d was rejected early", i+1)
		}
	}
	if scope.reserveManualMediaItem() {
		t.Fatal("scope allowed more than five manual media items in one run")
	}
	if scope.mediaByteLimit() != 20<<20 {
		t.Fatalf("manual media byte limit=%d, want 20 MiB", scope.mediaByteLimit())
	}
}

func TestLiveTestScopeRejectsFIFOWithoutBlocking(t *testing.T) {
	if os.Getenv("WF_SCOPE_FIFO_CHILD") == "1" {
		_, err := loadLiveTestScopeAt(os.Getenv("WF_SCOPE_FIFO_PATH"), scopeTestNow())
		if err == nil || err.Error() != errInvalidLiveTestScope.Error() {
			os.Exit(3)
		}
		os.Exit(0)
	}
	fifo := filepath.Join(t.TempDir(), "scope.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveTestScopeRejectsFIFOWithoutBlocking$")
	cmd.Env = append(os.Environ(), "WF_SCOPE_FIFO_CHILD=1", "WF_SCOPE_FIFO_PATH="+fifo)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FIFO scope probe exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("FIFO scope read blocked instead of rejecting promptly")
	}
}

func TestLiveTestScopeExplicitSelfExclusionAndFourChatLimit(t *testing.T) {
	scope := &LiveTestScope{chats: map[string]struct{}{scopeTestChat: {}}, includeSelf: false}
	if scope.allowsChat(scopeTestChat, scopeTestChat) {
		t.Fatal("explicit self chat overrode include_self=false")
	}
	scope.includeSelf = true
	if !scope.allowsChat(scopeTestOther, scopeTestOther) {
		t.Fatal("dynamic self was not admitted when include_self=true")
	}
	chats := []string{"15550123456@s.whatsapp.net", "15550123457@s.whatsapp.net", "15550123458@s.whatsapp.net", "15550123459@s.whatsapp.net", "15550123460@s.whatsapp.net"}
	path := writeScopeConfig(t, `{"since":"2026-10-20T09:25:00Z","chats":["`+strings.Join(chats, `","`)+`"],"include_self":true}`, 0600)
	if _, err := loadLiveTestScopeAt(path, scopeTestNow()); err == nil {
		t.Fatal("five explicit chat IDs were accepted; only four fit beside dynamic self")
	}
}

func TestScopedHistoryRequestIsDisabledBeforeSDKDispatch(t *testing.T) {
	scope := &LiveTestScope{since: scopeTestNow().Add(-24 * time.Hour), chats: map[string]struct{}{scopeTestChat: {}}, includeSelf: false}
	adapter := NewWithLiveTestScope(nil, t.TempDir(), scope)
	err := adapter.RequestHistory(context.Background(), scopeTestChat, app.HistoryAnchor{Timestamp: scopeTestNow().UnixMilli()}, 50)
	if err == nil || !strings.Contains(err.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped history request was not rejected before SDK dispatch: %v", err)
	}
}

func TestScopedDirectoryLookupsFailBeforeSDK(t *testing.T) {
	adapter := NewWithLiveTestScope(nil, t.TempDir(), &LiveTestScope{})
	if _, _, err := adapter.ResolveContact(context.Background(), "15550123456"); err == nil || !strings.Contains(err.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped contact resolution was not rejected before SDK: %v", err)
	}
	if _, err := adapter.ListContacts(context.Background()); err == nil || !strings.Contains(err.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped contact roster was not rejected before SDK: %v", err)
	}
	if _, err := adapter.GetJoinedGroups(context.Background()); err == nil || !strings.Contains(err.Error(), "disabled for scoped live-test mode") {
		t.Fatalf("scoped community refresh was not rejected before SDK: %v", err)
	}
}

func TestScopedMutationLedgerOnlyTracksSuccessfulCurrentRunSends(t *testing.T) {
	scope := &LiveTestScope{acceptedMessages: make(map[liveTestMessageKey]struct{})}
	if scope.allowsMutationTarget(scopeTestChat, "inbound") {
		t.Fatal("inbound/history acceptance authorized remote mutation")
	}
	scope.rememberSuccessfulSend(scopeTestChat, "bad id", true)
	if scope.allowsMutationTarget(scopeTestChat, "bad id") {
		t.Fatal("invalid outbound ID was authorized for mutation")
	}
	scope.rememberSuccessfulSend(scopeTestChat, "OUTBOUND-123", true)
	if !scope.allowsMutationTarget(scopeTestChat, "OUTBOUND-123") {
		t.Fatal("successful current-run send was not authorized for mutation")
	}
	if scope.allowsMutationTarget(scopeTestOther, "OUTBOUND-123") {
		t.Fatal("mutation ledger was not bound to composite (chat,id)")
	}
	scope.rememberSuccessfulSend(scopeTestChat, "SECOND-123", false)
	if scope.allowsMutationTarget(scopeTestChat, "SECOND-123") {
		t.Fatal("failed/replaced send was authorized for mutation")
	}
}

func TestScopedMediaRequiresProfileLookupAndInWindowAllowedChat(t *testing.T) {
	scope := &LiveTestScope{since: scopeTestNow().Add(-24 * time.Hour), chats: map[string]struct{}{scopeTestChat: {}}, includeSelf: false}
	if scope.allowsManualMedia(scopeTestOther, "synthetic-media", scopeTestNow()) {
		t.Fatal("unallowed chat media passed scoped lookup boundary")
	}
	if scope.allowsManualMedia(scopeTestChat, "synthetic-old", scopeTestNow().Add(-48*time.Hour)) {
		t.Fatal("out-of-window media passed scoped lookup boundary")
	}
	if scope.allowsManualMedia(scopeTestChat, "", scopeTestNow()) {
		t.Fatal("missing message ID passed scoped lookup boundary")
	}
	if !scope.allowsManualMedia(scopeTestChat, "synthetic-current", scopeTestNow()) {
		t.Fatal("allowed current chat/message was rejected")
	}
}

type scopeBoundarySender struct {
	connected bool
	loggedIn  bool
	calls     int
	requestID types.MessageID
	response  whatsmeow.SendResponse
	err       error
}

func (s *scopeBoundarySender) IsConnected() bool { return s.connected }
func (s *scopeBoundarySender) IsLoggedIn() bool  { return s.loggedIn }
func (s *scopeBoundarySender) SendMessage(_ context.Context, _ types.JID, _ *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	s.calls++
	response := s.response
	if len(extra) == 1 {
		s.requestID = extra[0].ID
		response.ID = extra[0].ID
	}
	return response, s.err
}

func TestScopedOutboundBoundaryDispatchesOnlySelfAndTracksCurrentSuccess(t *testing.T) {
	scope := &LiveTestScope{chats: map[string]struct{}{scopeTestOther: {}}, includeSelf: true}
	probe := &scopeBoundarySender{connected: true, loggedIn: true, response: whatsmeow.SendResponse{Timestamp: scopeTestNow()}}
	if _, err := sendProtocolAtBoundary(context.Background(), scope, probe, scopeTestChat, scopeTestOther, &waE2E.Message{}, "client-id", nil); err == nil {
		t.Fatal("explicit non-self chat reached scoped outbound dispatch")
	}
	if probe.calls != 0 {
		t.Fatalf("rejected target dispatched %d messages, want zero", probe.calls)
	}
	if _, err := sendProtocolAtBoundary(context.Background(), scope, probe, scopeTestChat, scopeTestChat, &waE2E.Message{}, "client-id", func() bool { return true }); err != nil {
		t.Fatalf("connected self send was rejected: %v", err)
	}
	if probe.calls != 1 || probe.requestID != "client-id" || !scope.allowsMutationTarget(scopeTestChat, string(probe.requestID)) {
		t.Fatalf("successful current send did not track the SDK response ID: dispatches=%d request_id=%q", probe.calls, probe.requestID)
	}
	if _, err := sendProtocolAtBoundary(context.Background(), scope, probe, scopeTestChat, scopeTestChat, &waE2E.Message{}, "client-id-2", func() bool { return false }); err == nil {
		t.Fatal("send from replaced client was accepted")
	}
	if scope.allowsMutationTarget(scopeTestChat, "client-id-2") {
		t.Fatal("replaced-client send was tracked")
	}
}

func TestScopedOutboundBoundaryDoesNotTrackAmbiguousSendError(t *testing.T) {
	scope := &LiveTestScope{chats: map[string]struct{}{scopeTestOther: {}}, includeSelf: true}
	probe := &scopeBoundarySender{
		connected: true,
		loggedIn:  true,
		response:  whatsmeow.SendResponse{Timestamp: scopeTestNow()},
		err:       context.DeadlineExceeded,
	}
	_, err := sendProtocolAtBoundary(context.Background(), scope, probe, scopeTestChat, scopeTestChat, &waE2E.Message{}, "ambiguous-client-id", func() bool { return true })
	if err != context.DeadlineExceeded {
		t.Fatalf("ambiguous send error was changed: %v", err)
	}
	if probe.calls != 1 {
		t.Fatalf("ambiguous send dispatched %d times, want one attempt", probe.calls)
	}
	if probe.requestID != "ambiguous-client-id" {
		t.Fatalf("ambiguous send did not preserve the SDK request/response ID: %q", probe.requestID)
	}
	if scope.allowsMutationTarget(scopeTestChat, string(probe.requestID)) {
		t.Fatal("ambiguous SendMessage response ID was authorized for mutation")
	}
}

func TestScopedForwardTextIsDisabledBeforeClientAccess(t *testing.T) {
	adapter := NewWithLiveTestScope(nil, t.TempDir(), &LiveTestScope{})
	_, err := adapter.ForwardText(context.Background(), scopeTestChat, "synthetic pre-run transcript", "scoped-forward-synthetic")
	if err == nil || !strings.Contains(err.Error(), "historical forwarding is disabled for scoped live-test mode") {
		t.Fatalf("scoped forward did not fail closed before client access: %v", err)
	}
}

func TestScopedRemoteMutationRoutesRequireCurrentRunTarget(t *testing.T) {
	adapter := NewWithLiveTestScope(nil, t.TempDir(), &LiveTestScope{})
	checks := []struct {
		name string
		call func() error
	}{
		{"reply", func() error {
			_, err := adapter.SendReply(context.Background(), scopeTestChat, "inbound", false, "", "quote", "reply", "send-id")
			return err
		}},
		{"list", func() error {
			_, err := adapter.SendListReply(context.Background(), app.Message{ChatID: scopeTestChat, ID: "inbound"}, "row", "title", "description", "send-id")
			return err
		}},
		{"button", func() error {
			_, err := adapter.SendButtonReply(context.Background(), app.Message{ChatID: scopeTestChat, ID: "inbound"}, "button", "text", "send-id")
			return err
		}},
		{"reaction", func() error {
			_, err := adapter.SendReaction(context.Background(), scopeTestChat, "inbound", false, "", "👍", "send-id")
			return err
		}},
		{"edit", func() error {
			_, err := adapter.EditMessage(context.Background(), scopeTestChat, "inbound", "edited", "send-id")
			return err
		}},
		{"revoke", func() error {
			_, err := adapter.RevokeMessage(context.Background(), scopeTestChat, "inbound", "send-id")
			return err
		}},
		{"poll vote", func() error {
			_, err := adapter.SendPollVote(context.Background(), app.Message{ChatID: scopeTestChat, ID: "inbound"}, []string{"choice"}, "send-id")
			return err
		}},
	}
	for _, test := range checks {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(err.Error(), "scoped remote mutation target") {
				t.Fatalf("inbound/history target was not blocked: %v", err)
			}
		})
	}
}
