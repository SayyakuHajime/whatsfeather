package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestIncomingLocationProjectionIsTypedBoundedAndDurable(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	a, err := app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := types.ParseJID("synthetic-location@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(a, t.TempDir())
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "static-location", Timestamp: time.Unix(2, 0)},
		Message: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: proto.Float64(47.6), DegreesLongitude: proto.Float64(-122.3),
			Name: proto.String("Synthetic <b>place</b>"), Address: proto.String("Northwind & West"),
			Comment: proto.String("Meet <here>"), IsLive: proto.Bool(false),
			URL:           proto.String("https://maps.invalid/sentinel-do-not-expose"),
			JPEGThumbnail: []byte("thumbnail-sentinel-do-not-expose"),
		}},
	}, "Synthetic location")
	adapter.record(context.Background(), &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat}, ID: "live-location", Timestamp: time.Unix(3, 0)},
		Message: &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude: proto.Float64(-33.9), DegreesLongitude: proto.Float64(151.2),
			Caption: proto.String("Received snapshot"), JPEGThumbnail: []byte("live-thumbnail-sentinel"),
		}},
	}, "Synthetic location")

	readPage := func() map[string]any {
		t.Helper()
		result, callErr := a.Handle(context.Background(), "message.list", []byte(`{"chat_id":"synthetic-location@s.whatsapp.net","limit":10}`))
		if callErr != nil {
			t.Fatal(callErr)
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), "maps.invalid") || strings.Contains(string(encoded), "thumbnail-sentinel") || strings.Contains(string(encoded), "JPEGThumbnail") {
			t.Fatalf("location URL/thumbnail or raw protocol data escaped the projection: %s", encoded)
		}
		var page map[string]any
		if unmarshalErr := json.Unmarshal(encoded, &page); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return page
	}
	assertPage := func(page map[string]any) {
		t.Helper()
		rows, ok := page["messages"].([]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("message.list did not return both synthetic location snapshots: %#v", page)
		}
		static := rows[0].(map[string]any)
		staticLocation, ok := static["location"].(map[string]any)
		if !ok || static["kind"] != "location" || staticLocation["latitude"] != 47.6 || staticLocation["longitude"] != -122.3 ||
			staticLocation["is_live"] != false || staticLocation["caption"] != "Meet <here>" ||
			staticLocation["name"] != "Synthetic <b>place</b>" || staticLocation["address"] != "Northwind & West" {
			t.Fatalf("static location was not projected as bounded typed fields: %#v", static)
		}
		live := rows[1].(map[string]any)
		liveLocation, ok := live["location"].(map[string]any)
		if !ok || live["kind"] != "location" || liveLocation["latitude"] != -33.9 || liveLocation["longitude"] != 151.2 ||
			liveLocation["is_live"] != true || liveLocation["caption"] != "Received snapshot" {
			t.Fatalf("live location was not represented as a received snapshot: %#v", live)
		}
	}

	assertPage(readPage())
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = app.Open(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	assertPage(readPage())

	db, err := sql.Open("sqlite", filepath.Join(profile, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT name FROM pragma_table_info('message_locations') ORDER BY cid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"chat_id", "message_id", "latitude", "longitude", "is_live", "caption", "name", "address"}
	if len(columns) != len(want) {
		t.Fatalf("normalized location projection columns = %v, want %v", columns, want)
	}
	for i := range want {
		if columns[i] != want[i] {
			t.Fatalf("normalized location projection columns = %v, want %v", columns, want)
		}
	}
}

func TestInvalidLocationCoordinatesFallBackAtomically(t *testing.T) {
	const fallback = "[Unsupported content — open the official app]"
	static := func(latitude, longitude *float64) *waE2E.Message {
		return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: latitude, DegreesLongitude: longitude,
			Comment: proto.String("private-caption"), Name: proto.String("private-name"),
			Address: proto.String("private-address"), URL: proto.String("https://maps.invalid/private"),
			JPEGThumbnail: []byte("private-thumbnail"),
		}}
	}
	live := func(latitude, longitude *float64) *waE2E.Message {
		return &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude: latitude, DegreesLongitude: longitude,
			Caption: proto.String("private-caption"), JPEGThumbnail: []byte("private-thumbnail"),
		}}
	}
	cases := []struct {
		name    string
		message *waE2E.Message
	}{
		{"static-nan-latitude", static(proto.Float64(math.NaN()), proto.Float64(1))},
		{"static-infinite-longitude", static(proto.Float64(1), proto.Float64(math.Inf(1)))},
		{"static-latitude-range", static(proto.Float64(90.0001), proto.Float64(1))},
		{"static-longitude-range", static(proto.Float64(1), proto.Float64(-180.0001))},
		{"static-missing-latitude", static(nil, proto.Float64(1))},
		{"live-nan-latitude", live(proto.Float64(math.NaN()), proto.Float64(1))},
		{"live-infinite-longitude", live(proto.Float64(1), proto.Float64(math.Inf(-1)))},
		{"live-latitude-range", live(proto.Float64(-90.0001), proto.Float64(1))},
		{"live-longitude-range", live(proto.Float64(1), proto.Float64(180.0001))},
		{"live-missing-longitude", live(proto.Float64(1), nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := messageContent(&events.Message{Message: tc.message})
			if got.kind != "unsupported" || got.body != fallback || got.location != nil {
				t.Fatalf("invalid coordinates did not atomically use the generic unsupported fallback: %#v", got)
			}
			if strings.Contains(got.body, "private-") || strings.Contains(got.body, "maps.invalid") {
				t.Fatalf("invalid location leaked protocol content: %#v", got)
			}
		})
	}
}

func TestStaticLocationSendBuildsTypedNonLiveProtocolEnvelope(t *testing.T) {
	probe := &contactEnvelopeProbe{}
	location := app.LocationProjection{Latitude: 47.6, Longitude: -122.3, Caption: "Meet <here>", Name: "Synthetic place", Address: "Northwind & West"}
	stamp, err := sendLocationWithClient(context.Background(), probe, "15551234567@s.whatsapp.net", location, "stable-static-location-id")
	if err != nil || stamp != time.Unix(7, 0).UnixMilli() {
		t.Fatalf("static location send = %d, %v", stamp, err)
	}
	message := probe.message.GetLocationMessage()
	if message == nil || probe.id != "stable-static-location-id" || probe.chat.String() != "15551234567@s.whatsapp.net" ||
		message.DegreesLatitude == nil || message.GetDegreesLatitude() != location.Latitude ||
		message.DegreesLongitude == nil || message.GetDegreesLongitude() != location.Longitude ||
		message.IsLive == nil || message.GetIsLive() || message.GetComment() != location.Caption ||
		message.GetName() != location.Name || message.GetAddress() != location.Address || message.GetURL() != "" || len(message.GetJPEGThumbnail()) != 0 {
		t.Fatalf("static location was not sent as a minimal IsLive=false typed message: probe=%#v protocol=%#v", probe, message)
	}
}

func TestUnsafeLocationTextFallsBackAtomically(t *testing.T) {
	const fallback = "[Unsupported content — open the official app]"
	static := func(caption, name, address string) *waE2E.Message {
		return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2),
			Comment: proto.String(caption), Name: proto.String(name), Address: proto.String(address),
			URL: proto.String("https://maps.invalid/private"), JPEGThumbnail: []byte("private-thumbnail"),
		}}
	}
	live := func(caption string) *waE2E.Message {
		return &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2),
			Caption: proto.String(caption), JPEGThumbnail: []byte("private-thumbnail"),
		}}
	}
	cases := []struct {
		name    string
		message *waE2E.Message
	}{
		{"static-caption-control", static("line one\nline two", "", "")},
		{"static-name-too-long", static("", strings.Repeat("n", 257), "")},
		{"static-address-delete-control", static("", "", "private\x7faddress")},
		{"live-caption-too-long", live(strings.Repeat("c", 257))},
		{"live-caption-control", live("snapshot\tlabel")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := messageContent(&events.Message{Message: tc.message})
			if got.kind != "unsupported" || got.body != fallback || got.location != nil {
				t.Fatalf("unsafe location text did not atomically use the generic unsupported fallback: %#v", got)
			}
		})
	}
}
