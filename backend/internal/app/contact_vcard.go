package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ContactVCardMaxBytes = 64 << 10
	ContactCardMaxCount  = 10
	contactNameMaxRunes  = 256
	contactPhoneMaxRunes = 64
	contactEmailMaxRunes = 254
	contactOrgMaxRunes   = 256
	contactValuesMax     = 5
)

type ContactCard struct {
	Name         string   `json:"name"`
	Phones       []string `json:"phones,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	Organization string   `json:"organization,omitempty"`
}

func contactVCardError() error {
	return fail("invalid_contact", "The vCard is malformed or contains unsupported data")
}

// ParseVCard accepts only the bounded plain-text subset WhatsFeather renders
// and shares. It intentionally discards all properties except FN/N, TEL,
// EMAIL, and ORG, including waid/JID protocol metadata.
func ParseVCard(data []byte) ([]ContactCard, error) {
	if len(data) == 0 || len(data) > ContactVCardMaxBytes || !utf8.Valid(data) {
		return nil, contactVCardError()
	}
	for i, b := range data {
		if (b < 0x20 && b != '\r' && b != '\n' && b != '\t') || b == 0x7f ||
			(b == '\t' && i > 0 && data[i-1] != '\n') {
			return nil, contactVCardError()
		}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.ContainsRune(text, '\r') {
		return nil, contactVCardError()
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '	' {
			return nil, contactVCardError()
		}
	}
	physical := strings.Split(text, "\n")
	lines := make([]string, 0, len(physical))
	for _, line := range physical {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if len(lines) == 0 || line == " " || line == "\t" {
				return nil, contactVCardError()
			}
			lines[len(lines)-1] += line[1:]
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
	}

	cards := make([]ContactCard, 0, 1)
	inside := false
	var fn, nValue, organization string
	phones, emails := []string{}, []string{}
	for _, line := range lines {
		upper := strings.ToUpper(line)
		if upper == "BEGIN:VCARD" {
			if inside || len(cards) >= ContactCardMaxCount {
				return nil, contactVCardError()
			}
			inside = true
			fn, nValue, organization = "", "", ""
			phones, emails = nil, nil
			continue
		}
		if upper == "END:VCARD" {
			if !inside {
				return nil, contactVCardError()
			}
			name := fn
			if strings.TrimSpace(name) == "" {
				name = safeNName(nValue)
			}
			name, ok := safeContactText(name, contactNameMaxRunes, true)
			if !ok || name == "" {
				return nil, contactVCardError()
			}
			cards = append(cards, ContactCard{Name: name, Phones: append([]string(nil), phones...), Emails: append([]string(nil), emails...), Organization: organization})
			inside = false
			continue
		}
		if !inside {
			return nil, contactVCardError()
		}
		colon := firstUnescapedColon(line)
		if colon <= 0 {
			return nil, contactVCardError()
		}
		left, value := line[:colon], line[colon+1:]
		segments := strings.Split(left, ";")
		property := strings.ToUpper(segments[0])
		if dot := strings.LastIndexByte(property, '.'); dot >= 0 {
			property = property[dot+1:]
		}
		switch property {
		case "FN", "N", "TEL", "EMAIL", "ORG":
			for _, parameter := range segments[1:] {
				key, _, hasValue := strings.Cut(parameter, "=")
				if hasValue && (strings.EqualFold(key, "ENCODING") || strings.EqualFold(key, "CHARSET") || strings.EqualFold(key, "VALUE")) {
					return nil, contactVCardError()
				}
			}
			decoded, ok := unescapeVCard(value)
			if !ok {
				return nil, contactVCardError()
			}
			switch property {
			case "FN":
				if fn == "" {
					fn = decoded
				}
			case "N":
				if nValue == "" {
					nValue = value
				}
			case "TEL":
				phone, ok := safeContactText(decoded, contactPhoneMaxRunes, false)
				if !ok || !validPhone(phone) || len(phones) >= contactValuesMax {
					return nil, contactVCardError()
				}
				phones = append(phones, phone)
			case "EMAIL":
				email, ok := safeContactText(decoded, contactEmailMaxRunes, false)
				if !ok || !validEmail(email) || len(emails) >= contactValuesMax {
					return nil, contactVCardError()
				}
				emails = append(emails, email)
			case "ORG":
				if organization == "" {
					parts, ok := splitUnescaped(value, ';')
					if !ok {
						return nil, contactVCardError()
					}
					decodedParts := make([]string, 0, len(parts))
					for _, part := range parts {
						decodedPart, valid := unescapeVCard(part)
						if !valid {
							return nil, contactVCardError()
						}
						if strings.TrimSpace(decodedPart) != "" {
							decodedParts = append(decodedParts, strings.TrimSpace(decodedPart))
						}
					}
					organization, ok = safeContactText(strings.Join(decodedParts, ", "), contactOrgMaxRunes, true)
					if !ok {
						return nil, contactVCardError()
					}
				}
			}
		default:
			// Unknown properties are discarded without decoding or exposing them.
		}
	}
	if inside || len(cards) == 0 {
		return nil, contactVCardError()
	}
	return cards, nil
}

func firstUnescapedColon(line string) int {
	escaped := false
	for i := 0; i < len(line); i++ {
		if escaped {
			escaped = false
			continue
		}
		if line[i] == '\\' {
			escaped = true
			continue
		}
		if line[i] == ':' {
			return i
		}
	}
	return -1
}

func splitUnescaped(value string, delimiter byte) ([]string, bool) {
	parts := []string{}
	start := 0
	escaped := false
	for i := 0; i < len(value); i++ {
		if escaped {
			escaped = false
			continue
		}
		if value[i] == '\\' {
			escaped = true
			continue
		}
		if value[i] == delimiter {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	if escaped {
		return nil, false
	}
	return append(parts, value[start:]), true
}

func unescapeVCard(value string) (string, bool) {
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		i++
		if i >= len(value) {
			return "", false
		}
		switch value[i] {
		case '\\', ',', ';', ':':
			out.WriteByte(value[i])
		case 'n', 'N':
			out.WriteByte('\n')
		default:
			return "", false
		}
	}
	return out.String(), utf8.ValidString(out.String())
}

func safeContactText(value string, maxRunes int, allowEscapedNewline bool) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return "", false
	}
	for _, r := range value {
		if r == '\n' && allowEscapedNewline {
			continue
		}
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return value, true
}

func safeNName(raw string) string {
	if raw == "" {
		return ""
	}
	parts, ok := splitUnescaped(raw, ';')
	if !ok || len(parts) > 5 {
		return ""
	}
	decoded := make([]string, 5)
	for i, part := range parts {
		v, valid := unescapeVCard(part)
		if !valid {
			return ""
		}
		decoded[i] = strings.TrimSpace(v)
	}
	// vCard N order is family;given;additional;prefix;suffix. Present it in
	// ordinary reading order without storing its structured components.
	name := strings.Join(nonEmpty(decoded[3], decoded[1], decoded[2], decoded[0], decoded[4]), " ")
	return name
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func validPhone(value string) bool {
	digit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digit = true
			continue
		}
		if !strings.ContainsRune("+ -().", r) {
			return false
		}
	}
	return digit
}

func validEmail(value string) bool {
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value && !strings.ContainsAny(value, " \t\r\n")
}

func CanonicalVCard(card ContactCard) (string, error) {
	name, ok := safeContactText(card.Name, contactNameMaxRunes, true)
	if !ok {
		return "", fmt.Errorf("invalid contact name")
	}
	lines := []string{"BEGIN:VCARD", "VERSION:3.0", "FN:" + escapeVCard(name), "N:" + escapeVCard(name) + ";;;;"}
	if len(card.Phones) > contactValuesMax || len(card.Emails) > contactValuesMax {
		return "", fmt.Errorf("too many contact fields")
	}
	for _, value := range card.Phones {
		phone, valid := safeContactText(value, contactPhoneMaxRunes, false)
		if !valid || !validPhone(phone) {
			return "", fmt.Errorf("invalid phone")
		}
		lines = append(lines, "TEL;TYPE=CELL:"+escapeVCard(phone))
	}
	for _, value := range card.Emails {
		email, valid := safeContactText(value, contactEmailMaxRunes, false)
		if !valid || !validEmail(email) {
			return "", fmt.Errorf("invalid email")
		}
		lines = append(lines, "EMAIL;TYPE=INTERNET:"+escapeVCard(email))
	}
	if card.Organization != "" {
		organization, valid := safeContactText(card.Organization, contactOrgMaxRunes, true)
		if !valid {
			return "", fmt.Errorf("invalid organization")
		}
		lines = append(lines, "ORG:"+escapeVCard(organization))
	}
	lines = append(lines, "END:VCARD")
	return strings.Join(lines, "\r\n") + "\r\n", nil
}

func normalizeContactCards(cards []ContactCard) ([]ContactCard, error) {
	if len(cards) == 0 || len(cards) > ContactCardMaxCount {
		return nil, contactVCardError()
	}
	normalized := make([]ContactCard, 0, len(cards))
	totalBytes := 0
	for _, card := range cards {
		canonical, err := CanonicalVCard(card)
		if err != nil {
			return nil, contactVCardError()
		}
		totalBytes += len(canonical)
		if totalBytes > ContactVCardMaxBytes {
			return nil, contactVCardError()
		}
		parsed, err := ParseVCard([]byte(canonical))
		if err != nil || len(parsed) != 1 {
			return nil, contactVCardError()
		}
		normalized = append(normalized, parsed[0])
	}
	return normalized, nil
}

func (a *App) fillContactCards(ctx context.Context, chat string, messages []Message) error {
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
	query := "SELECT message_id,projection FROM message_contacts WHERE chat_id=? AND message_id IN (" + strings.Join(placeholders, ",") + ")"
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string][]ContactCard, len(messages))
	for rows.Next() {
		var id, projection string
		if err = rows.Scan(&id, &projection); err != nil {
			return err
		}
		var cards []ContactCard
		if err = json.Unmarshal([]byte(projection), &cards); err != nil {
			return fmt.Errorf("stored contact projection is invalid")
		}
		cards, err = normalizeContactCards(cards)
		if err != nil {
			return fmt.Errorf("stored contact projection is invalid")
		}
		byID[id] = cards
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range messages {
		if cards, ok := byID[messages[i].ID]; ok {
			messages[i].ContactCards = &cards
		}
	}
	return nil
}

func escapeVCard(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString("\\\\")
		case ',':
			out.WriteString("\\,")
		case ';':
			out.WriteString("\\;")
		case ':':
			out.WriteString("\\:")
		case '\n':
			out.WriteString("\\n")
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
