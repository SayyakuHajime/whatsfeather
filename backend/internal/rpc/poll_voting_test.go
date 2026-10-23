package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
)

type pollVoteTransport struct {
	mu      sync.Mutex
	calls   int
	labels  [][]string
	clients []string
	failure error
}

func (p *pollVoteTransport) Connect(context.Context) error { return nil }
func (p *pollVoteTransport) Disconnect()                   {}
func (p *pollVoteTransport) Send(context.Context, string, string, string) (int64, error) {
	return 0, errors.New("unexpected text send")
}
func (p *pollVoteTransport) SendPollVote(_ context.Context, _ app.Message, labels []string, id string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.labels = append(p.labels, append([]string(nil), labels...))
	p.clients = append(p.clients, id)
	if p.failure != nil {
		return 0, p.failure
	}
	return 1234, nil
}
func (p *pollVoteTransport) sendCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func startPollRPC(t *testing.T, a *app.App, socket string) (context.CancelFunc, <-chan error) {
	t.Helper()
	server, err := Listen(socket, a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	return cancel, done
}

func stopPollRPC(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("private RPC server did not stop")
	}
}

type pollRPCResponse struct {
	Version int             `json:"v"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *app.Error      `json:"error"`
}

func callPollRPC(t *testing.T, socket, id, method string, params any) pollRPCResponse {
	t.Helper()
	request, err := json.Marshal(struct {
		V      int    `json:"v"`
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{V: 1, ID: id, Method: method, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response pollRPCResponse
	if err = json.Unmarshal(line, &response); err != nil {
		t.Fatalf("invalid private RPC response %q: %v", line, err)
	}
	if response.Version != 1 || response.ID != id {
		t.Fatalf("wrong private RPC envelope: %+v", response)
	}
	return response
}

func openPollRPCApp(t *testing.T, profile string, transport *pollVoteTransport) *app.App {
	t.Helper()
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	if transport != nil {
		a.SetTransport(transport)
		a.SetStatus("connected", "Synthetic transport", "")
	}
	return a
}

func TestPrivateRPCPollVoteEnvelopeAndUnknownRestartGuard(t *testing.T) {
	root, err := os.MkdirTemp("", "poll-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove synthetic poll fixture: %v", err)
		}
	})
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile")
	chat := "15550000001@s.whatsapp.net"
	transport := &pollVoteTransport{}
	a := openPollRPCApp(t, profile, transport)
	defer a.Close()
	max := uint32(1)
	if err := a.Record(context.Background(), app.Message{
		ID: "synthetic-poll", ChatID: chat, Body: "Pick one?", Timestamp: 1000,
		SenderID: "15550000002@s.whatsapp.net", Status: "received", Kind: "poll",
		Poll: &app.PollProjection{Question: "Pick one?", Options: []string{"Alpha", "Beta"}, SelectableOptionsCount: &max},
	}, "Synthetic poll owner"); err != nil {
		t.Fatal(err)
	}

	socket := filepath.Join(root, "first.sock")
	cancel, done := startPollRPC(t, a, socket)
	var stopFirst sync.Once
	defer stopFirst.Do(func() { stopPollRPC(t, cancel, done) })
	accepted := callPollRPC(t, socket, "rpc-accepted", "poll.vote", map[string]any{
		"chat_id": chat, "target_id": "synthetic-poll", "option_indices": []int{1}, "client_id": "rpc-accepted-vote",
	})
	if accepted.Error != nil || len(accepted.Result) == 0 {
		t.Fatalf("accepted vote failed: %+v", accepted)
	}
	var acceptedEnvelope struct {
		Message app.Message `json:"message"`
	}
	if err := json.Unmarshal(accepted.Result, &acceptedEnvelope); err != nil {
		t.Fatal(err)
	}
	message := acceptedEnvelope.Message
	if message.ID != "synthetic-poll" || message.ChatID != chat || message.Kind != "poll" || message.Poll == nil || message.PollResults == nil || message.PollSelfVote == nil {
		t.Fatalf("poll.vote did not return the authoritative message envelope: %s", accepted.Result)
	}
	if message.PollSelfVote.State != "accepted" || len(message.PollSelfVote.OptionIndices) != 1 || message.PollSelfVote.OptionIndices[0] != 1 || message.PollVoteState != "" {
		t.Fatalf("wrong authoritative vote state: self=%+v blocked=%q", message.PollSelfVote, message.PollVoteState)
	}
	if message.PollResults.Options[0].VoteCount != 0 || message.PollResults.Options[1].VoteCount != 0 {
		t.Fatalf("transport acceptance was fabricated as a remote poll tally: %+v", message.PollResults)
	}
	listedAccepted := callPollRPC(t, socket, "rpc-list-accepted", "message.list", map[string]any{"chat_id": chat, "limit": 10})
	if listedAccepted.Error != nil {
		t.Fatal(listedAccepted.Error)
	}
	var acceptedPage struct {
		Messages []app.Message `json:"messages"`
	}
	if err := json.Unmarshal(listedAccepted.Result, &acceptedPage); err != nil {
		t.Fatal(err)
	}
	if len(acceptedPage.Messages) != 1 || !reflect.DeepEqual(acceptedPage.Messages[0], message) {
		t.Fatalf("poll.vote message differs from hydrated message.list target: result=%+v page=%+v", message, acceptedPage.Messages)
	}
	if transport.sendCount() != 1 {
		t.Fatalf("accepted vote send count=%d", transport.sendCount())
	}

	transport.mu.Lock()
	transport.failure = errors.New("synthetic ambiguous send result")
	transport.mu.Unlock()
	unknown := callPollRPC(t, socket, "rpc-unknown", "poll.vote", map[string]any{
		"chat_id": chat, "target_id": "synthetic-poll", "option_indices": []int{0}, "client_id": "rpc-unknown-vote",
	})
	if unknown.Error == nil || unknown.Error.Code != "unknown_outcome" || len(unknown.Result) != 0 {
		t.Fatalf("ambiguous vote response=%+v; want unknown_outcome", unknown)
	}
	if transport.sendCount() != 2 {
		t.Fatalf("unknown vote send count=%d", transport.sendCount())
	}

	listed := callPollRPC(t, socket, "rpc-list-before-restart", "message.list", map[string]any{"chat_id": chat, "limit": 10})
	if listed.Error != nil {
		t.Fatal(listed.Error)
	}
	assertUnknownPollProjection(t, listed.Result)
	stopFirst.Do(func() { stopPollRPC(t, cancel, done) })
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the same synthetic profile but give the adapter a new client ID
	// namespace. The durable target guard, not a repeated client_id, must stop
	// the send before it reaches the transport.
	restartedTransport := &pollVoteTransport{failure: errors.New("must not be called")}
	a = openPollRPCApp(t, profile, restartedTransport)
	defer a.Close()
	restartedSocket := filepath.Join(root, "restarted.sock")
	restartCancel, restartDone := startPollRPC(t, a, restartedSocket)
	defer stopPollRPC(t, restartCancel, restartDone)
	fresh := callPollRPC(t, restartedSocket, "rpc-new-client-id", "poll.vote", map[string]any{
		"chat_id": chat, "target_id": "synthetic-poll", "option_indices": []int{1}, "client_id": "new-random-client-id-after-restart",
	})
	if fresh.Error == nil || fresh.Error.Code != "unknown_outcome" || len(fresh.Result) != 0 {
		t.Fatalf("new client ID bypassed durable unknown poll guard: %+v", fresh)
	}
	if restartedTransport.sendCount() != 0 {
		t.Fatalf("unknown vote replayed after restart: calls=%d", restartedTransport.sendCount())
	}
	listed = callPollRPC(t, restartedSocket, "rpc-list-after-restart", "message.list", map[string]any{"chat_id": chat, "limit": 10})
	if listed.Error != nil {
		t.Fatal(listed.Error)
	}
	assertUnknownPollProjection(t, listed.Result)
}

func assertUnknownPollProjection(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var page struct {
		Messages []struct {
			ID            string                      `json:"id"`
			PollSelfVote  *app.PollSelfVoteProjection `json:"poll_self_vote"`
			PollVoteState string                      `json:"poll_vote_state"`
			PollResults   *app.PollResultsProjection  `json:"poll_results"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 {
		t.Fatalf("expected one synthetic poll in message.list: %s", raw)
	}
	message := page.Messages[0]
	if message.ID != "synthetic-poll" || message.PollSelfVote == nil || message.PollSelfVote.State != "unknown" || len(message.PollSelfVote.OptionIndices) != 1 || message.PollSelfVote.OptionIndices[0] != 0 || message.PollVoteState != "unknown" {
		t.Fatalf("cross-restart unknown state missing from message.list: %s", raw)
	}
	if message.PollResults == nil || message.PollResults.Options[0].VoteCount != 0 || message.PollResults.Options[1].VoteCount != 0 {
		t.Fatalf("local action state changed aggregate without an incoming update: %s", raw)
	}
}
