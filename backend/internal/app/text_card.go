package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TextCardProjection holds only inert visible text. It contains no action metadata.
type TextCardProjection struct {
	Title    string   `json:"title,omitempty"`
	Subtitle string   `json:"subtitle,omitempty"`
	Body     string   `json:"body,omitempty"`
	Footer   string   `json:"footer,omitempty"`
	Labels   []string `json:"labels,omitempty"`
}

const TextCardProjectionMaxBytes = 4096

func TextCardProjectionValid(p TextCardProjection) bool {
	if len(p.Labels) > 4 || p.fallback() == "" {
		return false
	}
	for _, s := range append([]string{p.Title, p.Subtitle, p.Body, p.Footer}, p.Labels...) {
		if len(s) > 4096 || !buttonsTextValid(s, 4096, false) {
			return false
		}
	}
	for _, s := range p.Labels {
		if strings.TrimSpace(s) == "" {
			return false
		}
	}
	b, e := json.Marshal(p)
	return e == nil && len(b) <= 4096
}
func (p TextCardProjection) fallback() string {
	for _, s := range append([]string{p.Body, p.Title, p.Subtitle, p.Footer}, p.Labels...) {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
func decodeStoredTextCardProjection(encoded string) (TextCardProjection, error) {
	var card TextCardProjection
	if len(encoded) > TextCardProjectionMaxBytes || !utf8.ValidString(encoded) {
		return card, fmt.Errorf("stored text card projection is invalid")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	invalid := fmt.Errorf("stored text card projection is invalid")
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return card, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return card, invalid
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return card, invalid
		}
		seen[name] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil {
			return card, invalid
		}
		if name == "labels" {
			if len(raw) == 0 || raw[0] != '[' {
				return card, invalid
			}
			var labels []json.RawMessage
			if json.Unmarshal(raw, &labels) != nil || len(labels) > 4 {
				return card, invalid
			}
			for _, label := range labels {
				v, ok := strictCardString(label)
				if !ok {
					return card, invalid
				}
				card.Labels = append(card.Labels, v)
			}
		} else {
			v, ok := strictCardString(raw)
			if !ok {
				return card, invalid
			}
			switch name {
			case "title":
				card.Title = v
			case "subtitle":
				card.Subtitle = v
			case "body":
				card.Body = v
			case "footer":
				card.Footer = v
			default:
				return card, invalid
			}
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return card, invalid
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF || !TextCardProjectionValid(card) {
		return card, invalid
	}

	return card, nil
}

// encoding/json replaces lone UTF-16 surrogates. Reject them before decoding.
func strictCardString(raw []byte) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", false
	}
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw)-1 {
			return "", false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return "", false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return "", false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return "", false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return "", false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return "", false
			}
			i += 6
		}
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func (a *App) fillTextCardProjections(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,kind,CASE WHEN length(CAST(projection AS BLOB)) <= 4096 THEN projection ELSE '' END FROM message_text_cards WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]TextCardProjection, len(messages))
	kinds := make(map[string]string, len(messages))
	for rows.Next() {
		var id, kind, encoded string
		if err = rows.Scan(&id, &kind, &encoded); err != nil {
			return err
		}
		card, decodeErr := decodeStoredTextCardProjection(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		if (kind != "interactive" && kind != "template") || (kind == "interactive" && len(card.Labels) != 0) {
			return fmt.Errorf("invalid stored text card kind or labels")
		}
		byID[id] = card
		kinds[id] = kind
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		card, ok := byID[messages[i].ID]
		if (messages[i].Kind == "interactive" || messages[i].Kind == "template") && !ok {
			return fmt.Errorf("stored text card projection is missing")
		}
		if ok {
			if messages[i].Kind != kinds[messages[i].ID] {
				return fmt.Errorf("stored text card projection does not match message kind")
			}
			if messages[i].Kind == "interactive" {
				messages[i].Interactive = &card
			} else if messages[i].Kind == "template" {
				messages[i].Template = &card
			} else {
				return fmt.Errorf("invalid text card kind")
			}
		}
	}
	return nil
}
