package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

type locationShareProbe struct {
	calls      int
	chat       string
	id         string
	projection LocationProjection
	stamp      int64
	err        error
}

func (p *locationShareProbe) Connect(context.Context) error { return nil }
func (p *locationShareProbe) Disconnect()                   {}
func (p *locationShareProbe) Send(context.Context, string, string, string) (int64, error) {
	return 0, nil
}
func (p *locationShareProbe) SendLocation(_ context.Context, chat string, location LocationProjection, id string) (int64, error) {
	p.calls++
	p.chat, p.id, p.projection = chat, id, location
	return p.stamp, p.err
}

func TestLocationSendUsesReviewedTypedStableMessage(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const chat = "synthetic-location-send-chat"
	if err = a.Record(context.Background(), Message{ID: "location-seed", ChatID: chat, Body: "seed", Timestamp: 1, Status: "received", Kind: "text"}, "Synthetic location"); err != nil {
		t.Fatal(err)
	}
	transport := &locationShareProbe{stamp: 4321}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	params := map[string]any{
		"chat_id": chat, "latitude": 47.6, "longitude": -122.3,
		"caption": "Meet <here>", "name": "Synthetic <b>place</b>", "address": "Northwind & West",
		"reviewed": true, "client_id": "location-send-stable",
	}
	call := func() Message {
		t.Helper()
		value, callErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
		if callErr != nil {
			t.Fatal(callErr)
		}
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var response struct {
			Message Message `json:"message"`
		}
		if unmarshalErr := json.Unmarshal(encoded, &response); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return response.Message
	}
	first := call()
	want := LocationProjection{Latitude: 47.6, Longitude: -122.3, Caption: "Meet <here>", Name: "Synthetic <b>place</b>", Address: "Northwind & West"}
	if first.ID != "location-send-stable" || first.ChatID != chat || first.Kind != "location" || first.Status != "accepted" || first.Location == nil ||
		!reflect.DeepEqual(*first.Location, want) || transport.calls != 1 || transport.chat != chat || transport.id != first.ID || !reflect.DeepEqual(transport.projection, want) {
		t.Fatalf("location was not sent through one reviewed typed fake-transport call: message=%#v transport=%#v", first, transport)
	}
	second := call()
	if second.ID != first.ID || second.Status != "accepted" || transport.calls != 1 {
		t.Fatalf("same reviewed client ID was replayed: first=%#v second=%#v calls=%d", first, second, transport.calls)
	}
	listed := invoke(t, a, "message.list", map[string]any{"chat_id": chat, "limit": 10})["messages"].([]any)
	found := false
	for _, value := range listed {
		message := value.(map[string]any)
		if message["id"] == first.ID {
			found = true
			projection := message["location"].(map[string]any)
			if projection["latitude"] != 47.6 || projection["longitude"] != -122.3 || projection["is_live"] != false ||
				projection["caption"] != want.Caption || projection["name"] != want.Name || projection["address"] != want.Address {
				t.Fatalf("message.list lost the typed location projection: %#v", message)
			}
		}
	}
	if !found {
		t.Fatal("accepted typed location was not present in message.list")
	}
}

func TestLocationSendRejectsCoordinatesBeforeExplicitReview(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const chat = "synthetic-location-review-chat"
	if err = a.Record(context.Background(), Message{ID: "review-seed", ChatID: chat, Body: "seed", Timestamp: 1, Status: "received", Kind: "text"}, "Synthetic location"); err != nil {
		t.Fatal(err)
	}
	transport := &locationShareProbe{stamp: 1234}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	params := map[string]any{"chat_id": chat, "latitude": 1.0, "longitude": 2.0, "client_id": "location-unreviewed"}
	_, callErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
	if errorCode(callErr) != "review_required" || transport.calls != 0 {
		t.Fatalf("unreviewed coordinate send error=%v transport calls=%d, want review_required and no dispatch", callErr, transport.calls)
	}
	var rows int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", "location-unreviewed").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("unreviewed location created %d durable send intents", rows)
	}
}

func TestLocationSendRejectsOutOfRangeCoordinatesBeforeReservation(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const chat = "synthetic-location-range-chat"
	if err = a.Record(context.Background(), Message{ID: "range-seed", ChatID: chat, Body: "seed", Timestamp: 1, Status: "received", Kind: "text"}, "Synthetic location"); err != nil {
		t.Fatal(err)
	}
	transport := &locationShareProbe{stamp: 1234}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	for _, params := range []map[string]any{
		{"chat_id": chat, "latitude": 90.0001, "longitude": 1.0, "reviewed": true, "client_id": "location-bad-lat"},
		{"chat_id": chat, "latitude": -1.0, "longitude": -180.0001, "reviewed": true, "client_id": "location-bad-lon"},
	} {
		_, callErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
		if errorCode(callErr) != "invalid_location" {
			t.Fatalf("out-of-range coordinates error=%v, want invalid_location", callErr)
		}
	}
	if transport.calls != 0 {
		t.Fatalf("out-of-range coordinates reached transport %d times", transport.calls)
	}
	var messages, actions int
	if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id LIKE 'location-bad-%'").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id LIKE 'location-bad-%'").Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || actions != 0 {
		t.Fatalf("invalid coordinates reserved durable intent: messages=%d actions=%d", messages, actions)
	}
}

func TestLocationSendPersistsUnknownAndNeverReplaysAfterReopen(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	const chat = "synthetic-location-unknown-chat"
	if err = a.Record(context.Background(), Message{ID: "location-unknown-seed", ChatID: chat, Body: "seed", Timestamp: 1, Status: "received", Kind: "text"}, "Synthetic location"); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"chat_id": chat, "latitude": -33.9, "longitude": 151.2, "caption": "snapshot", "reviewed": true, "client_id": "location-unknown-stable"}
	transport := &locationShareProbe{err: errors.New("ambiguous synthetic transport failure")}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")
	_, sendErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
	if errorCode(sendErr) != "unknown_outcome" || transport.calls != 1 {
		t.Fatalf("ambiguous location send error=%v calls=%d, want one call and durable unknown", sendErr, transport.calls)
	}
	var messageStatus, actionStatus string
	if err = a.db.QueryRow("SELECT status FROM messages WHERE id=? AND chat_id=?", "location-unknown-stable", chat).Scan(&messageStatus); err != nil {
		t.Fatal(err)
	}
	if err = a.db.QueryRow("SELECT status FROM message_actions WHERE client_id=?", "location-unknown-stable").Scan(&actionStatus); err != nil {
		t.Fatal(err)
	}
	if messageStatus != "unknown" || actionStatus != "unknown" {
		t.Fatalf("ambiguous location outcome was not durably unknown: message=%q action=%q", messageStatus, actionStatus)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	replayProbe := &locationShareProbe{}
	a.SetTransport(replayProbe)
	a.SetStatus("connected", "synthetic retry transport", "")
	_, replayErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
	if errorCode(replayErr) != "unknown_outcome" || replayProbe.calls != 0 {
		t.Fatalf("reopened unknown location was automatically replayed: error=%v calls=%d", replayErr, replayProbe.calls)
	}
	listed := invoke(t, a, "message.list", map[string]any{"chat_id": chat, "limit": 10})["messages"].([]any)
	for _, value := range listed {
		message := value.(map[string]any)
		if message["id"] == "location-unknown-stable" {
			if message["status"] != "unknown" || message["location"].(map[string]any)["latitude"] != -33.9 {
				t.Fatalf("unknown outcome lost its durable location projection: %#v", message)
			}
			return
		}
	}
	t.Fatal("unknown location intent disappeared after profile reopen")
}

func TestLocationSendRequiresBothCoordinatesButAcceptsExplicitZero(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "profile"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const chat = "synthetic-location-presence-chat"
	if err = a.Record(context.Background(), Message{ID: "presence-seed", ChatID: chat, Body: "seed", Timestamp: 1, Status: "received", Kind: "text"}, "Synthetic location"); err != nil {
		t.Fatal(err)
	}
	transport := &locationShareProbe{stamp: 1234}
	a.SetTransport(transport)
	a.SetStatus("connected", "synthetic transport", "")

	cases := []struct {
		name     string
		clientID string
		field    string
		omit     bool
	}{
		{name: "omitted latitude", clientID: "location-omitted-latitude", field: "latitude", omit: true},
		{name: "omitted longitude", clientID: "location-omitted-longitude", field: "longitude", omit: true},
		{name: "null latitude", clientID: "location-null-latitude", field: "latitude"},
		{name: "null longitude", clientID: "location-null-longitude", field: "longitude"},
	}
	for _, tc := range cases {
		params := map[string]any{
			"chat_id": chat, "latitude": 12.5, "longitude": -70.25,
			"reviewed": true, "client_id": tc.clientID,
		}
		if tc.omit {
			delete(params, tc.field)
		} else {
			params[tc.field] = nil
		}
		_, callErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, params))
		if code := errorCode(callErr); code != "invalid_location" {
			t.Errorf("%s: error=%v (code %q), want invalid_location; fake transport calls=%d", tc.name, callErr, code, transport.calls)
		}
		if transport.calls != 0 {
			t.Errorf("%s reached fake transport %d times, want 0", tc.name, transport.calls)
		}
		var messages, actions int
		if err = a.db.QueryRow("SELECT count(*) FROM messages WHERE client_id=?", tc.clientID).Scan(&messages); err != nil {
			t.Fatal(err)
		}
		if err = a.db.QueryRow("SELECT count(*) FROM message_actions WHERE client_id=?", tc.clientID).Scan(&actions); err != nil {
			t.Fatal(err)
		}
		if messages != 0 || actions != 0 {
			t.Errorf("%s created durable records: messages=%d message_actions=%d, want 0", tc.name, messages, actions)
		}
	}

	zeroParams := map[string]any{
		"chat_id": chat, "latitude": 0.0, "longitude": 0.0,
		"caption": "Explicit zero", "reviewed": true, "client_id": "location-explicit-zero",
	}
	_, zeroErr := a.Handle(context.Background(), "message.location.send", mustJSON(t, zeroParams))
	if zeroErr != nil {
		t.Fatalf("explicit numeric (0,0) coordinate pair was rejected: %v", zeroErr)
	}
	want := LocationProjection{Latitude: 0, Longitude: 0, Caption: "Explicit zero"}
	if transport.calls != 1 || transport.chat != chat || transport.id != "location-explicit-zero" || !reflect.DeepEqual(transport.projection, want) {
		t.Fatalf("explicit numeric (0,0) was not dispatched exactly once: transport=%#v", transport)
	}
}
