package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type actionProbe struct {
	replyCalls                   int
	reactionCalls                int
	forwardCalls                 int
	lastChat, lastTarget, lastID string
	fail                         bool
}

type scopedForwardActionProbe struct{ actionProbe }

func (*scopedForwardActionProbe) ScopedLiveTest() bool { return true }

func (p *actionProbe) Connect(context.Context) error                               { return nil }
func (p *actionProbe) Disconnect()                                                 {}
func (p *actionProbe) Send(context.Context, string, string, string) (int64, error) { return 0, nil }
func (p *actionProbe) SendReply(_ context.Context, chat, target string, fromMe bool, participant, quoted, text, id string) (int64, error) {
	p.replyCalls++
	p.lastChat, p.lastTarget, p.lastID = chat, target, id
	if fromMe || participant != "15551234567@s.whatsapp.net" || quoted != "incoming" || text != "answer" {
		return 0, errors.New("reply protocol inputs lost")
	}
	return 1234, nil
}
func (p *actionProbe) SendReaction(_ context.Context, chat, target string, _ bool, _ string, emoji, id string) (int64, error) {
	p.reactionCalls++
	p.lastChat, p.lastTarget, p.lastID = chat, target, id
	if emoji != "🔥" {
		return 0, errors.New("reaction changed")
	}
	if p.fail {
		return 0, errors.New("ambiguous send")
	}
	return 1234, nil
}
func (p *actionProbe) EditMessage(context.Context, string, string, string, string) (int64, error) {
	return 0, nil
}
func (p *actionProbe) RevokeMessage(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (p *actionProbe) ForwardText(context.Context, string, string, string) (int64, error) {
	p.forwardCalls++
	return 0, nil
}
func (p *actionProbe) Account(context.Context) (Account, error) {
	return Account{Available: true, JID: "19995550123@s.whatsapp.net", State: "connected"}, nil
}

type editActionProbe struct {
	actionProbe
	editCalls int
}

func (p *editActionProbe) EditMessage(context.Context, string, string, string, string) (int64, error) {
	p.editCalls++
	return 1234, nil
}

type blockingEditActionProbe struct {
	editActionProbe
	entered chan struct{}
	release chan struct{}
}

type revokeActionProbe struct {
	actionProbe
	revokeCalls int
}

func (p *revokeActionProbe) RevokeMessage(context.Context, string, string, string) (int64, error) {
	p.revokeCalls++
	return 1234, nil
}

func (p *blockingEditActionProbe) EditMessage(ctx context.Context, _, _, _, _ string) (int64, error) {
	p.editCalls++
	close(p.entered)
	select {
	case <-p.release:
		return 1234, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type directoryProbe struct {
	verified bool
	calls    int
}

func (p *directoryProbe) Connect(context.Context) error                               { return nil }
func (p *directoryProbe) Disconnect()                                                 {}
func (p *directoryProbe) Send(context.Context, string, string, string) (int64, error) { return 0, nil }

func (p *directoryProbe) ResolveContact(_ context.Context, phone string) (Contact, bool, error) {
	p.calls++
	return Contact{JID: "15551234567@s.whatsapp.net", Name: "Verified Contact", Phone: phone, Verified: p.verified}, p.verified, nil
}
func (p *directoryProbe) ListContacts(context.Context) ([]Contact, error) {
	return []Contact{{JID: "15551234567@s.whatsapp.net", Name: "Cached", Verified: false}}, nil
}

type selfAliasDirectoryProbe struct {
	directoryProbe
	account    Account
	resolvedID string
}

func (p *selfAliasDirectoryProbe) Account(context.Context) (Account, error) {
	return p.account, nil
}
func (p *selfAliasDirectoryProbe) ResolveContact(_ context.Context, phone string) (Contact, bool, error) {
	p.calls++
	return Contact{JID: p.resolvedID, Name: "Resolved self alias", Phone: phone, Verified: p.verified}, p.verified, nil
}

func TestFakeAcceptedRevokeRedactsLocallyAndReplaysWithoutResending(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &revokeActionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-revoke-chat"
	const messageID = "accepted-revoke-target"
	const clientID = "fake-revoke-action"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "synthetic outgoing message",
		Timestamp: time.Now().Add(-time.Minute).UnixMilli(), Outgoing: true,
		Status: "accepted", Kind: "text", SenderID: "19995550123@s.whatsapp.net",
	}, "Synthetic revoke chat"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-revoke-chat","id":"accepted-revoke-target","scope":"everyone","client_id":"fake-revoke-action"}`)
	result, err := a.Handle(ctx, "message.delete", params)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Message Message `json:"message"`
	}
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if probe.revokeCalls != 1 || response.Message.ID != messageID ||
		response.Message.ChatID != chatID || !response.Message.Deleted ||
		response.Message.Body != "[Message deleted]" ||
		response.Message.MutationProvenance != "accepted" {
		t.Fatalf("fake accepted revoke did not reconcile the target: calls=%d result=%s", probe.revokeCalls, encoded)
	}
	var actionStatus string
	if err = a.db.QueryRowContext(ctx, "SELECT status FROM message_actions WHERE client_id=?", clientID).Scan(&actionStatus); err != nil {
		t.Fatal(err)
	}
	if actionStatus != "accepted" {
		t.Fatalf("fake revoke action status=%q, want accepted", actionStatus)
	}
	if _, err = a.Handle(ctx, "message.delete", params); err != nil {
		t.Fatal(err)
	}
	if probe.revokeCalls != 1 {
		t.Fatalf("accepted revoke replay dispatched again; calls=%d", probe.revokeCalls)
	}
}

func TestLiveRevokeRejectsFutureTimestampBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &revokeActionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-revoke-chat"
	const messageID = "future-revoke-target"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "before",
		Timestamp: time.Now().Add(time.Hour).UnixMilli(), Outgoing: true,
		Status: "accepted", Kind: "text", SenderID: "19995550123@s.whatsapp.net",
	}, "Synthetic revoke chat"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-revoke-chat","id":"future-revoke-target","scope":"everyone","client_id":"future-revoke-action"}`)
	_, err = a.Handle(ctx, "message.delete", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "expired" {
		t.Fatalf("future-dated revoke returned %v, want expired before dispatch", err)
	}
	if probe.revokeCalls != 0 {
		t.Fatalf("transport received %d revoke calls for a future-dated target", probe.revokeCalls)
	}
	var actionRows int
	if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id=?", "future-revoke-action").Scan(&actionRows); err != nil {
		t.Fatal(err)
	}
	if actionRows != 0 {
		t.Fatalf("future-dated preflight reserved %d action rows", actionRows)
	}
	var body string
	if err = a.db.QueryRow("SELECT body FROM messages WHERE chat_id=? AND id=?", chatID, messageID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "before" {
		t.Fatalf("rejected revoke changed local body to %q", body)
	}
}

func TestLiveAcceptedEditReplayPreservesNewerRemoteState(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &editActionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-edit-chat"
	const messageID = "accepted-edit-target"
	const actor = "19995550123@s.whatsapp.net"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "before",
		Timestamp: time.Now().Add(-time.Minute).UnixMilli(), Outgoing: true,
		Status: "accepted", Kind: "text", SenderID: actor,
	}, "Synthetic edit chat"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-edit-chat","id":"accepted-edit-target","text":"requested edit","client_id":"accepted-edit-action"}`)
	if _, err = a.Handle(ctx, "message.edit", params); err != nil {
		t.Fatal(err)
	}
	if err = a.ApplyIncomingEdit(ctx, chatID, messageID, "newer remote edit", actor); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(ctx, "message.edit", params)
	if err != nil {
		t.Fatalf("replaying accepted edit: %v", err)
	}
	message := result.(map[string]any)["message"].(Message)
	if message.Body != "newer remote edit" || message.MutationProvenance != "remote" {
		t.Fatalf("accepted action replay overwrote the newer remote edit: %#v", message)
	}
	if probe.editCalls != 1 {
		t.Fatalf("accepted edit replay dispatched %d transport calls, want exactly one", probe.editCalls)
	}
}

func TestLiveEditDoesNotOverwriteNewerIncomingEditWhileTransportIsPending(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &blockingEditActionProbe{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	const chatID = "synthetic-edit-chat"
	const messageID = "concurrent-edit-target"
	const actor = "19995550123@s.whatsapp.net"
	if err = a.Record(ctx, Message{
		ID: messageID, ChatID: chatID, Body: "before",
		Timestamp: time.Now().Add(-time.Minute).UnixMilli(), Outgoing: true,
		Status: "accepted", Kind: "text", SenderID: actor,
	}, "Synthetic edit chat"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-edit-chat","id":"concurrent-edit-target","text":"requested edit","client_id":"concurrent-edit-action"}`)
	type handleResult struct {
		value any
		err   error
	}
	completed := make(chan handleResult, 1)
	go func() {
		value, handleErr := a.Handle(ctx, "message.edit", params)
		completed <- handleResult{value: value, err: handleErr}
	}()
	select {
	case <-probe.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("edit transport was not entered")
	}
	if err = a.ApplyIncomingEdit(ctx, chatID, messageID, "newer remote edit", actor); err != nil {
		close(probe.release)
		t.Fatalf("apply concurrent remote edit: %v", err)
	}
	close(probe.release)
	select {
	case result := <-completed:
		if result.err != nil {
			t.Fatalf("message.edit returned %v", result.err)
		}
		message := result.value.(map[string]any)["message"].(Message)
		if message.Body != "newer remote edit" || message.MutationProvenance != "remote" {
			t.Fatalf("edit completion overwrote the newer remote event: %#v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message.edit did not finish after transport release")
	}
	if probe.editCalls != 1 {
		t.Fatalf("edit transport call count=%d, want exactly one", probe.editCalls)
	}
}

func TestLiveEditRejectsFutureTimestampBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &editActionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(ctx, Message{
		ID: "future-edit-target", ChatID: "synthetic-edit-chat", Body: "before",
		Timestamp: time.Now().Add(time.Hour).UnixMilli(), Outgoing: true, Status: "accepted",
		Kind: "text", SenderID: "19995550123@s.whatsapp.net",
	}, "Synthetic edit chat"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-edit-chat","id":"future-edit-target","text":"edited","client_id":"future-edit-action"}`)
	_, err = a.Handle(ctx, "message.edit", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "expired" {
		t.Fatalf("future-dated edit returned %v, want expired before dispatch", err)
	}
	if probe.editCalls != 0 {
		t.Fatalf("transport received %d edit calls for a future-dated target", probe.editCalls)
	}
	var body string
	if err = a.db.QueryRow("SELECT body FROM messages WHERE chat_id=? AND id=?", "synthetic-edit-chat", "future-edit-target").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "before" {
		t.Fatalf("rejected edit changed local body to %q", body)
	}
	var actionRows int
	if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id=?", "future-edit-action").Scan(&actionRows); err != nil {
		t.Fatal(err)
	}
	if actionRows != 0 {
		t.Fatalf("future-dated preflight reserved %d action rows", actionRows)
	}
}

func TestLiveReplyUsesImmutableIDAndAcceptsOnlyOnce(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "incoming-id", ChatID: "15551234567@s.whatsapp.net", Body: "incoming", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Contact"); err != nil {
		t.Fatal(err)
	}
	params := `{"chat_id":"15551234567@s.whatsapp.net","target_id":"incoming-id","text":"answer","client_id":"reply-stable"}`
	first, err := a.Handle(context.Background(), "message.reply", json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	var actionStatus string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reply-stable").Scan(&actionStatus); err != nil {
		t.Fatalf("reply has no durable action outcome: %v", err)
	}
	if actionStatus != "accepted" {
		t.Fatalf("reply action state=%q, want transport accepted", actionStatus)
	}
	second, err := a.Handle(context.Background(), "message.reply", json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	if probe.replyCalls != 1 || probe.lastChat != "15551234567@s.whatsapp.net" || probe.lastTarget != "incoming-id" || probe.lastID != "reply-stable" {
		t.Fatalf("reply was duplicated or lost protocol identifiers: %#v", probe)
	}
	for _, result := range []any{first, second} {
		encoded, _ := json.Marshal(result)
		var out struct {
			Message Message `json:"message"`
		}
		if err = json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		if out.Message.ReplyToID != "incoming-id" {
			t.Fatalf("reply target not persisted: %s", encoded)
		}
	}
}

func TestLiveReplyCanRetryWhenDispatchWasProvenNotDispatched(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "reply-target", ChatID: "15551234567@s.whatsapp.net", Body: "incoming", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`CREATE TRIGGER reject_reply_dispatch BEFORE UPDATE ON message_actions WHEN NEW.status='dispatched' BEGIN SELECT RAISE(ABORT, 'synthetic pre-dispatch failure'); END`); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"reply-target","text":"answer","client_id":"reply-safe-retry"}`)
	_, err = a.Handle(context.Background(), "message.reply", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "not_dispatched" {
		t.Fatalf("reply preflight failure returned %v, want not_dispatched", err)
	}
	if probe.replyCalls != 0 {
		t.Fatalf("reply transport was called %d times before safe retry", probe.replyCalls)
	}
	if _, err = a.db.Exec("DROP TRIGGER reject_reply_dispatch"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.reply", params)
	if err != nil {
		t.Fatalf("reply retry after proven not-dispatched failed: %v", err)
	}
	message := result.(map[string]any)["message"].(Message)
	if probe.replyCalls != 1 || message.Status != "accepted" {
		t.Fatalf("safe reply retry did not dispatch once and persist accepted: calls=%d message=%#v", probe.replyCalls, message)
	}
}

func TestLiveForwardCanRetryWhenDispatchWasProvenNotDispatched(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "forward-target", ChatID: "source-chat", Body: "synthetic source", Kind: "text", Timestamp: 1, Status: "received"}, "Synthetic source"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(context.Background(), Message{ID: "destination-seed", ChatID: "destination-chat", Body: "synthetic destination", Kind: "text", Timestamp: 2, Status: "received"}, "Synthetic destination"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`CREATE TRIGGER reject_forward_dispatch BEFORE UPDATE ON message_actions WHEN NEW.status='dispatched' BEGIN SELECT RAISE(ABORT, 'synthetic pre-dispatch failure'); END`); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"source-chat","id":"forward-target","target_chat_id":"destination-chat","client_id":"forward-safe-retry"}`)
	_, err = a.Handle(context.Background(), "message.forward", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "not_dispatched" {
		t.Fatalf("forward preflight failure returned %v, want not_dispatched", err)
	}
	if probe.forwardCalls != 0 {
		t.Fatalf("forward transport was called %d times before safe retry", probe.forwardCalls)
	}
	forwarded, err := a.loadTarget(context.Background(), "destination-chat", "forward-safe-retry")
	if err != nil {
		t.Fatal(err)
	}
	if forwarded.MutationProvenance != "local_only" {
		t.Fatalf("pre-dispatch forward claimed mutation provenance %q, want local_only", forwarded.MutationProvenance)
	}
	if _, err = a.db.Exec("DROP TRIGGER reject_forward_dispatch"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Handle(context.Background(), "message.forward", params)
	if err != nil {
		t.Fatalf("forward retry after proven not-dispatched failed: %v", err)
	}
	message := result.(map[string]any)["message"].(Message)
	if probe.forwardCalls != 1 || message.Status != "accepted" || message.MutationProvenance != "accepted" {
		t.Fatalf("safe forward retry did not dispatch once and persist accepted: calls=%d message=%#v", probe.forwardCalls, message)
	}
}

func TestScopedLiveForwardBlocksHistoricalMessagesBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &scopedForwardActionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic scoped forward probe", "")
	if err = a.Record(ctx, Message{ID: "historic-source", ChatID: "synthetic-source-chat", Body: "synthetic pre-run transcript", Kind: "text", Timestamp: 1, Status: "received"}, "Synthetic source"); err != nil {
		t.Fatal(err)
	}
	if err = a.Record(ctx, Message{ID: "destination-seed", ChatID: "synthetic-destination-chat", Body: "synthetic destination seed", Kind: "text", Timestamp: 2, Status: "received"}, "Synthetic destination"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"synthetic-source-chat","id":"historic-source","target_chat_id":"synthetic-destination-chat","client_id":"scoped-forward-synthetic"}`)
	result, err := a.Handle(ctx, "message.forward", params)
	var appErr *Error
	if result != nil || !errors.As(err, &appErr) || appErr.Code != "unsupported" {
		t.Fatalf("scoped historical forward returned result=%#v error=%v, want unsupported before lookup/dispatch", result, err)
	}
	if probe.forwardCalls != 0 {
		t.Fatalf("scoped historical forward reached transport %d times", probe.forwardCalls)
	}
	var inserted, actions int
	if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE chat_id=? AND client_id=?", "synthetic-destination-chat", "scoped-forward-synthetic").Scan(&inserted); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRowContext(ctx, "SELECT count(*) FROM message_actions WHERE client_id=?", "scoped-forward-synthetic").Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if inserted != 0 || actions != 0 {
		t.Fatalf("scoped forward created local state before rejection: messages=%d actions=%d", inserted, actions)
	}
}

func TestOpenAddsReactionReconciliationColumnsToExistingProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("ALTER TABLE message_actions DROP COLUMN dispatched_at_ms"); err != nil {
		_ = a.Close()
		t.Fatal(err)
	}
	if _, err = a.db.Exec("ALTER TABLE message_actions DROP COLUMN actor_id"); err != nil {
		_ = a.Close()
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, column := range []string{"actor_id", "dispatched_at_ms"} {
		var present int
		if err = a.db.QueryRow("SELECT count(*) FROM pragma_table_info('message_actions') WHERE name=?", column).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present != 1 {
			t.Errorf("existing profile is missing reaction reconciliation column %q", column)
		}
	}
}

func TestLiveReactionEchoDuringTransportCallResolvesOutcome(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	probe := &gatedReactionActionProbe{actionProbe: &actionProbe{fail: true}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() {
		probe.releaseTransport()
		_ = a.Close()
	})
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	chat := "15551234567@s.whatsapp.net"
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: chat, Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: chat}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-event-during-send"}`)
	type response struct {
		value any
		err   error
	}
	result := make(chan response, 1)
	go func() {
		value, requestErr := a.Handle(context.Background(), "message.react", params)
		result <- response{value: value, err: requestErr}
	}()
	select {
	case <-probe.entered:
	case <-time.After(time.Second):
		t.Fatal("reaction transport was not entered")
	}
	if err = a.ApplyIncomingReaction(context.Background(), chat, "target", "19995550123@s.whatsapp.net", "🔥", time.Now().Add(24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	probe.releaseTransport()
	select {
	case out := <-result:
		if out.err != nil {
			t.Fatalf("remote echo during an ambiguous send returned %v", out.err)
		}
		projection := out.value.(map[string]any)
		if projection["provenance"] != "remote" {
			t.Fatalf("reaction echo returned provenance=%v, want remote", projection["provenance"])
		}
	case <-time.After(time.Second):
		t.Fatal("reaction request did not settle after the transport returned")
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("transport calls=%d, want one original dispatch", probe.reactionCalls)
	}
}

func TestLiveReactionUnknownOutcomeReconcilesOnlyOnLaterSelfReactionEvent(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{fail: true}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	chat := "15551234567@s.whatsapp.net"
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: chat, Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: chat}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	first := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-reconcile-first"}`)
	_, err = a.Handle(context.Background(), "message.react", first)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || probe.reactionCalls != 1 {
		t.Fatalf("first reaction err=%v calls=%d, want unknown after one dispatch", err, probe.reactionCalls)
	}

	future := time.Now().Add(24 * time.Hour).UnixMilli()
	if err = a.ApplyIncomingReaction(context.Background(), chat, "target", "15551234567@s.whatsapp.net", "🔥", future); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-reconcile-first").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Fatalf("wrong-actor observation reconciled action to %q, want unknown", status)
	}
	if err = a.ApplyIncomingReaction(context.Background(), chat, "target", "19995550123@s.whatsapp.net", "🔥", time.Now().Add(-24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-reconcile-first").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Fatalf("pre-dispatch observation reconciled action to %q, want unknown", status)
	}
	fresh := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-reconcile-blocked"}`)
	if _, err = a.Handle(context.Background(), "message.react", fresh); !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" {
		t.Fatalf("unreconciled action returned %v, want unknown_outcome", err)
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("unreconciled action caused %d transport calls, want 1", probe.reactionCalls)
	}

	if err = a.ApplyIncomingReaction(context.Background(), chat, "target", "19995550123@s.whatsapp.net", "💚", future); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-reconcile-first").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "reconciled" {
		t.Fatalf("post-dispatch self observation left status %q, want reconciled", status)
	}
	firstReplay := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-reconcile-first"}`)
	if _, err = a.Handle(context.Background(), "message.react", firstReplay); !errors.As(err, &appErr) || appErr.Code != "reconciled" {
		t.Fatalf("same action replay after a differing remote observation returned %v, want reconciled", err)
	}
	probe.fail = false
	afterReconcile := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-reconcile-after"}`)
	if _, err = a.Handle(context.Background(), "message.react", afterReconcile); err != nil {
		t.Fatalf("new reaction after remote observation failed: %v", err)
	}
	if probe.reactionCalls != 2 {
		t.Fatalf("reconciled action transport calls=%d, want exactly one later action", probe.reactionCalls)
	}
}

func TestLiveReactionBlocksNewClientIDWhileTargetOutcomeUnknown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	probe := &actionProbe{fail: true}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	chat := "15551234567@s.whatsapp.net"
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: chat, Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: chat}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	first := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-unknown-first"}`)
	_, err = a.Handle(context.Background(), "message.react", first)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" || probe.reactionCalls != 1 {
		t.Fatalf("first reaction err=%v calls=%d, want durable unknown after one dispatch", err, probe.reactionCalls)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	retryProbe := &actionProbe{}
	a.SetTransport(retryProbe)
	a.SetStatus("connected", "synthetic", "")
	freshID := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-unknown-fresh-id"}`)
	_, err = a.Handle(context.Background(), "message.react", freshID)
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" {
		t.Fatalf("fresh action ID against unresolved target returned %v, want unknown_outcome", err)
	}
	if retryProbe.reactionCalls != 0 {
		t.Fatalf("fresh action ID blindly dispatched %d times after restart", retryProbe.reactionCalls)
	}
}

func TestLiveReactionUnknownOutcomeIsNeverRetriedBlindly(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{fail: true}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "hello", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Contact"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-stable"}`)
	if _, err = a.Handle(context.Background(), "message.react", params); err == nil {
		t.Fatal("ambiguous transport result must not look successful")
	}
	if _, err = a.Handle(context.Background(), "message.react", params); err == nil {
		t.Fatal("unknown action outcome must require reconciliation")
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("unknown outcome was resent %d times", probe.reactionCalls)
	}
}

func TestLiveReactionAcceptedPersistenceFailureBecomesDurableUnknown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`CREATE TRIGGER reject_action_acceptance AFTER UPDATE ON message_actions WHEN NEW.status='accepted' BEGIN SELECT RAISE(FAIL, 'synthetic persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-accept-persist-failure"}`)
	_, err = a.Handle(context.Background(), "message.react", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" {
		t.Fatalf("accepted transport with failed outcome persistence returned %v, want unknown_outcome", err)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-accept-persist-failure").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" {
		t.Fatalf("accepted persistence failure left durable action state %q, want unknown", status)
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("transport calls=%d, want one", probe.reactionCalls)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if _, err = a.Handle(context.Background(), "message.react", params); !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" {
		t.Fatalf("reopened unknown action returned %v, want unknown_outcome", err)
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("replayed an action with unknown persisted outcome; transport calls=%d", probe.reactionCalls)
	}
}

type gatedReactionActionProbe struct {
	*actionProbe
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *gatedReactionActionProbe) SendReaction(ctx context.Context, chat, target string, fromMe bool, participant, emoji, id string) (int64, error) {
	p.entered <- struct{}{}
	<-p.release
	return p.actionProbe.SendReaction(ctx, chat, target, fromMe, participant, emoji, id)
}

func (p *gatedReactionActionProbe) releaseTransport() {
	p.once.Do(func() { close(p.release) })
}

func TestLiveReactionPersistsDispatchBeforeCallingTransport(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &gatedReactionActionProbe{actionProbe: &actionProbe{}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() {
		transport.releaseTransport()
		_ = a.Close()
	})
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`CREATE TABLE action_status_log(status TEXT NOT NULL);
		CREATE TRIGGER log_action_insert AFTER INSERT ON message_actions BEGIN INSERT INTO action_status_log VALUES(NEW.status); END;
		CREATE TRIGGER log_action_update AFTER UPDATE ON message_actions WHEN OLD.status<>NEW.status BEGIN INSERT INTO action_status_log VALUES(NEW.status); END`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, requestErr := a.Handle(context.Background(), "message.react", json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-dispatch-order"}`))
		result <- requestErr
	}()
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("reaction transport was not called")
	}
	rows, err := a.db.Query("SELECT status FROM action_status_log ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for rows.Next() {
		var state string
		if err = rows.Scan(&state); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0] != "reserved" || states[1] != "dispatched" {
		t.Fatalf("action lifecycle at transport entry=%v, want [reserved dispatched]", states)
	}
	transport.releaseTransport()
	select {
	case err = <-result:
		if err != nil {
			t.Fatalf("reaction returned error after accepted fake transport: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reaction did not complete after releasing transport")
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-dispatch-order").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("successful transport state=%q, want accepted", status)
	}
}

func TestHistoricalInFlightActionStatesBecomeUnknownOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	a, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"pending", "dispatched", "reserved"} {
		if _, err = a.db.Exec("INSERT INTO message_actions(client_id,chat_id,target_id,method,payload,status) VALUES(?,?,?,?,?,?)", "legacy-"+status, "chat", "target", "message.react", "🔥", status); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, tc := range []struct {
		status string
		want   string
	}{{"pending", "unknown"}, {"dispatched", "unknown"}, {"reserved", "not_dispatched"}} {
		var got string
		if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "legacy-"+tc.status).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("historical %s action reopened as %q, want %s", tc.status, got, tc.want)
		}
	}
}

func TestLiveReactionRetriesOnlyAfterProvenNotDispatched(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec(`CREATE TRIGGER reject_dispatch BEFORE UPDATE ON message_actions WHEN NEW.status='dispatched' BEGIN SELECT RAISE(ABORT, 'synthetic preflight failure'); END`); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-safe-retry"}`)
	_, err = a.Handle(context.Background(), "message.react", params)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "not_dispatched" {
		t.Fatalf("failed dispatch preflight returned %v, want not_dispatched", err)
	}
	if probe.reactionCalls != 0 {
		t.Fatalf("transport was called %d times after dispatch preflight failed", probe.reactionCalls)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-safe-retry").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "not_dispatched" {
		t.Fatalf("preflight failure state=%q, want not_dispatched", status)
	}
	if _, err = a.db.Exec("DROP TRIGGER reject_dispatch"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Handle(context.Background(), "message.react", params); err != nil {
		t.Fatalf("explicit retry after proven not-dispatched failed: %v", err)
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("safe retry transport calls=%d, want 1", probe.reactionCalls)
	}
}

func TestLiveReactionAcceptanceStaysDistinctFromRemoteProofAndConflicts(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &actionProbe{}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "synthetic", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Synthetic contact"); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"reaction-accepted-only"}`)
	result, err := a.Handle(context.Background(), "message.react", params)
	if err != nil {
		t.Fatal(err)
	}
	projection := result.(map[string]any)
	if projection["provenance"] != "accepted" || projection["message"].(Message).Reactions == nil || (*projection["message"].(Message).Reactions)[0].Provenance != "accepted" {
		t.Fatalf("transport acceptance was not represented explicitly: %#v", projection)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "reaction-accepted-only").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("transport result persisted as %q, want accepted only", status)
	}
	if _, err = a.Handle(context.Background(), "message.react", params); err != nil {
		t.Fatalf("same accepted action was not idempotent: %v", err)
	}
	conflicting := json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"😎","client_id":"reaction-accepted-only"}`)
	_, err = a.Handle(context.Background(), "message.react", conflicting)
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "conflict" {
		t.Fatalf("same action ID with a different payload returned %v, want conflict", err)
	}
	if probe.reactionCalls != 1 {
		t.Fatalf("idempotent retry or payload conflict made %d transport calls, want 1", probe.reactionCalls)
	}
	if err = a.db.QueryRow("SELECT emoji FROM message_reactions WHERE chat_id=? AND message_id=? AND sender_id=?", "15551234567@s.whatsapp.net", "target", "19995550123@s.whatsapp.net").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "🔥" {
		t.Fatalf("payload conflict changed the accepted reaction to %q", status)
	}
}

func TestDispatchedActionIsNeverReclassifiedAsNotDispatched(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.db.Exec("INSERT INTO message_actions(client_id,chat_id,target_id,method,payload,status) VALUES(?,?,?,?,?,'dispatched')", "already-dispatched", "chat", "target", "message.react", "🔥"); err != nil {
		t.Fatal(err)
	}
	transportCalls := 0
	a.mu.Lock()
	_, err = a.runAction(context.Background(), &actionProbe{}, "already-dispatched", func() (int64, error) {
		transportCalls++
		return 0, nil
	})
	a.mu.Unlock()
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "unknown_outcome" {
		t.Fatalf("existing dispatched action returned %v, want unknown_outcome", err)
	}
	var status string
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "already-dispatched").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dispatched" {
		t.Fatalf("existing dispatched action was reclassified as %q", status)
	}
	if transportCalls != 0 {
		t.Fatalf("transport called %d times while reconciling an existing dispatched action", transportCalls)
	}
}

type unavailableAccountActionProbe struct {
	*actionProbe
}

func (p *unavailableAccountActionProbe) Account(context.Context) (Account, error) {
	return Account{Available: false, State: "connected"}, nil
}

func TestLiveReactionVerifiesActorBeforeNetworkAction(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	probe := &unavailableAccountActionProbe{actionProbe: &actionProbe{}}
	a.SetTransport(probe)
	a.SetStatus("connected", "synthetic", "")
	if err = a.Record(context.Background(), Message{ID: "target", ChatID: "15551234567@s.whatsapp.net", Body: "hello", Kind: "text", Timestamp: 1, Status: "received", SenderID: "15551234567@s.whatsapp.net"}, "Contact"); err != nil {
		t.Fatal(err)
	}
	_, err = a.Handle(context.Background(), "message.react", json.RawMessage(`{"chat_id":"15551234567@s.whatsapp.net","target_id":"target","reaction":"🔥","client_id":"react-actor-first"}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "account_unavailable" {
		t.Fatalf("reaction without a verified actor returned %v, want account_unavailable", err)
	}
	if probe.reactionCalls != 0 {
		t.Fatalf("sent %d reaction(s) before verifying the actor", probe.reactionCalls)
	}
	var stored int
	if err = a.db.QueryRow("SELECT count(*) FROM message_reactions WHERE chat_id=? AND message_id=?", "15551234567@s.whatsapp.net", "target").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("persisted %d reaction(s) without a verified actor", stored)
	}
	if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id=?", "react-actor-first").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("reserved an action before verifying the actor: %d row(s)", stored)
	}
}

func TestChatOpenRequiresVerifiedRegisteredPhone(t *testing.T) {
	for _, verified := range []bool{true, false} {
		a, err := Open(filepath.Join(t.TempDir(), "p"), false)
		if err != nil {
			t.Fatal(err)
		}
		account := Account{Available: true, Phone: "19995550123", JID: "19995550123@s.whatsapp.net", State: "connected"}
		directory := &selfAliasDirectoryProbe{
			directoryProbe: directoryProbe{verified: verified},
			account:        account,
			resolvedID:     "15551234567@s.whatsapp.net",
		}
		a.SetTransport(directory)
		a.SetStatus("connected", "synthetic", "")
		result, err := a.Handle(context.Background(), "chat.open", json.RawMessage(`{"recipient":"+15551234567"}`))
		if verified {
			if err != nil {
				t.Fatalf("verified phone lookup failed: %v", err)
			}
			encoded, _ := json.Marshal(result)
			var out map[string]any
			_ = json.Unmarshal(encoded, &out)
			if out["verified"] != true {
				t.Fatalf("chat open did not report verification: %s", encoded)
			}
		} else {
			if err == nil {
				t.Fatal("unregistered number was accepted")
			}
			if len(invoke(t, a, "chat.list", map[string]any{})["chats"].([]any)) != 0 {
				t.Fatal("unverified phone created a chat")
			}
		}
		if directory.calls != 1 {
			t.Fatalf("lookup call count=%d", directory.calls)
		}
		_ = a.Close()
	}
}

func TestChatOpenRequiresVerifiedAccountBeforePhoneLookup(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	directory := &directoryProbe{verified: true}
	a.SetTransport(directory)
	a.SetStatus("connected", "synthetic", "")
	_, err = a.Handle(context.Background(), "chat.open", json.RawMessage(`{"recipient":"+15551234567"}`))
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "account_unavailable" {
		t.Fatalf("chat.open without a verified account returned %v, want account_unavailable", err)
	}
	if directory.calls != 0 {
		t.Fatalf("performed %d phone lookup(s) before verifying the account", directory.calls)
	}
}

func TestChatOpenOwnPhoneUsesCurrentAccountJIDWhenResolverReturnsAlias(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "p"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	account := Account{Available: true, Phone: "15551234567", JID: "15551234567@s.whatsapp.net", State: "connected"}
	directory := &selfAliasDirectoryProbe{
		directoryProbe: directoryProbe{verified: true},
		account:        account,
		resolvedID:     "987654321@lid",
	}
	a.SetTransport(directory)
	a.SetStatus("connected", "synthetic", "")
	result, err := a.Handle(context.Background(), "chat.open", json.RawMessage(`{"recipient":"+15551234567"}`))
	if err != nil {
		t.Fatalf("verified self phone lookup failed: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Chat     Chat    `json:"chat"`
		Contact  Contact `json:"contact"`
		Verified bool    `json:"verified"`
	}
	if err = json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Verified || out.Chat.ID != account.JID || out.Contact.JID != account.JID {
		t.Fatalf("self phone lookup did not preserve the verified account identity: %#v", out)
	}
}
