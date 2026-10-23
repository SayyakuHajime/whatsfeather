package app

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseVCardRejectsUnsafeInputsAtomically(t *testing.T) {
	valid := "BEGIN:VCARD\nFN:Valid\nEND:VCARD\n"
	many := strings.Repeat(valid, ContactCardMaxCount+1)
	cases := map[string][]byte{
		"oversized":                              []byte(strings.Repeat("x", ContactVCardMaxBytes+1)),
		"too many cards":                         []byte(many),
		"malformed second":                       []byte(valid + "BEGIN:VCARD\nFN:Broken\n"),
		"encoded field":                          []byte("BEGIN:VCARD\nFN:Valid\nEMAIL;ENCODING=QUOTED-PRINTABLE:ada=40test\nEND:VCARD\n"),
		"binary":                                 append([]byte(valid), 0),
		"control in ignored property":            []byte("BEGIN:VCARD\nFN:Valid\nNOTE:unexpected\tcontrol\nEND:VCARD\n"),
		"unicode C1 control":                     []byte("BEGIN:VCARD\nFN:Bad\u0085Name\nEND:VCARD\n"),
		"unicode C1 control in ignored property": []byte("BEGIN:VCARD\nFN:Valid\nX-PRIVATE:bad\u0085value\nEND:VCARD\n"),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseVCard(input)
			if err == nil || got != nil {
				t.Fatalf("ParseVCard() = %#v, %v; want atomic rejection", got, err)
			}
		})
	}
}

func TestParseVCardUsesSafeStructuredNameFallback(t *testing.T) {
	got, err := ParseVCard([]byte("BEGIN:VCARD\nN:Second;Grace;;;\nEND:VCARD\n"))
	if err != nil {
		t.Fatalf("ParseVCard() error = %v", err)
	}
	want := []ContactCard{{Name: "Grace Second"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseVCard() = %#v, want %#v", got, want)
	}
}
