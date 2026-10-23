package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	LinkPreviewURLMaxBytes         = 2048
	LinkPreviewSiteMaxBytes        = 253
	LinkPreviewTitleMaxRunes       = 256
	LinkPreviewDescriptionMaxRunes = 512
	LinkPreviewProjectionMaxBytes  = 4096
)

// LinkPreviewProjection contains only bounded text metadata already present in
// a message. It deliberately excludes thumbnails, media keys and fetch URLs.
type LinkPreviewProjection struct {
	Site        string `json:"site"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// BuildLinkPreviewProjection validates metadata embedded in a text message.
// It never performs DNS resolution or an HTTP request.
func BuildLinkPreviewProjection(body, matchedURL, title, description string) (*LinkPreviewProjection, bool) {
	if len(matchedURL) == 0 || len(matchedURL) > LinkPreviewURLMaxBytes ||
		!utf8.ValidString(body) || !utf8.ValidString(matchedURL) ||
		strings.TrimSpace(matchedURL) != matchedURL || !strings.Contains(body, matchedURL) {
		return nil, false
	}
	for _, r := range matchedURL {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return nil, false
		}
	}
	parsed, err := url.Parse(matchedURL)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil {
		return nil, false
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return nil, false
	}
	site := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if !validLinkPreviewSite(site) {
		return nil, false
	}
	projection := &LinkPreviewProjection{
		Site:        site,
		Title:       strings.TrimSpace(title),
		Description: strings.TrimSpace(description),
	}
	if !LinkPreviewProjectionValid(*projection) {
		return nil, false
	}
	return projection, true
}

func validLinkPreviewSite(site string) bool {
	if site == "" || len(site) > LinkPreviewSiteMaxBytes || !utf8.ValidString(site) {
		return false
	}
	if net.ParseIP(site) != nil {
		return true
	}
	if strings.ContainsAny(site, "/?#@[]:% \t\r\n") {
		return false
	}
	for _, label := range strings.Split(site, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

func validLinkPreviewText(value string, maxRunes int) bool {
	if len(value) > maxRunes*4 || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func LinkPreviewProjectionValid(p LinkPreviewProjection) bool {
	if !validLinkPreviewSite(p.Site) ||
		!validLinkPreviewText(p.Title, LinkPreviewTitleMaxRunes) ||
		!validLinkPreviewText(p.Description, LinkPreviewDescriptionMaxRunes) ||
		(strings.TrimSpace(p.Title) == "" && strings.TrimSpace(p.Description) == "") {
		return false
	}
	encoded, err := json.Marshal(p)
	return err == nil && len(encoded) <= LinkPreviewProjectionMaxBytes
}

func decodeStoredLinkPreviewProjection(encoded string) (LinkPreviewProjection, error) {
	var projection LinkPreviewProjection
	invalid := fmt.Errorf("stored link preview projection is invalid")
	if len(encoded) == 0 || len(encoded) > LinkPreviewProjectionMaxBytes || !utf8.ValidString(encoded) {
		return projection, invalid
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return projection, invalid
	}
	seen := make(map[string]bool, 3)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return projection, invalid
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return projection, invalid
		}
		seen[name] = true
		var value string
		if decoder.Decode(&value) != nil {
			return projection, invalid
		}
		switch name {
		case "site":
			projection.Site = value
		case "title":
			projection.Title = value
		case "description":
			projection.Description = value
		default:
			return projection, invalid
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return projection, invalid
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF || !LinkPreviewProjectionValid(projection) {
		return projection, invalid
	}
	return projection, nil
}

func (a *App) fillLinkPreviewProjections(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,CASE WHEN length(CAST(projection AS BLOB)) <= 4096 THEN projection ELSE '' END FROM message_link_previews WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]LinkPreviewProjection, len(messages))
	for rows.Next() {
		var id, encoded string
		if err = rows.Scan(&id, &encoded); err != nil {
			return err
		}
		projection, decodeErr := decodeStoredLinkPreviewProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		byID[id] = projection
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		projection, ok := byID[messages[i].ID]
		if !ok {
			continue
		}
		if messages[i].Kind != "text" {
			return fmt.Errorf("stored link preview projection does not match message kind")
		}
		if messages[i].Deleted {
			continue
		}
		copy := projection
		messages[i].LinkPreview = &copy
	}
	return nil
}
