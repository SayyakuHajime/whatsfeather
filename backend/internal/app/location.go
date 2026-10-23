package app

import (
	"context"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LocationProjection is the normalized, privacy-safe subset shown to the user.
// It intentionally omits protocol URLs, thumbnails, and live-update metadata.
type LocationProjection struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	IsLive    bool    `json:"is_live"`
	Caption   string  `json:"caption,omitempty"`
	Name      string  `json:"name,omitempty"`
	Address   string  `json:"address,omitempty"`
}

const LocationTextMaxRunes = 256

func LocationCoordinatesValid(latitude, longitude float64) bool {
	return !math.IsNaN(latitude) && !math.IsInf(latitude, 0) && latitude >= -90 && latitude <= 90 &&
		!math.IsNaN(longitude) && !math.IsInf(longitude, 0) && longitude >= -180 && longitude <= 180
}

func locationTextValid(value string) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > LocationTextMaxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func LocationProjectionValid(location LocationProjection) bool {
	return LocationCoordinatesValid(location.Latitude, location.Longitude) &&
		locationTextValid(location.Caption) && locationTextValid(location.Name) && locationTextValid(location.Address)
}

func (a *App) fillLocationProjections(ctx context.Context, chat string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	placeholders := make([]string, len(messages))
	args := make([]any, 0, len(messages)+1)
	args = append(args, chat)
	for i, message := range messages {
		placeholders[i] = "?"
		args = append(args, message.ID)
	}
	query := "SELECT message_id,latitude,longitude,is_live,caption,name,address FROM message_locations WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]LocationProjection, len(messages))
	for rows.Next() {
		var id string
		var location LocationProjection
		if err = rows.Scan(&id, &location.Latitude, &location.Longitude, &location.IsLive, &location.Caption, &location.Name, &location.Address); err != nil {
			return err
		}
		if !LocationProjectionValid(location) {
			return fail("invalid_location", "Stored location projection is invalid")
		}
		byID[id] = location
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if location, ok := byID[messages[i].ID]; ok {
			messages[i].Location = &location
		}
	}
	return nil
}
