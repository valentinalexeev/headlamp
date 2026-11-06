package scrub

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestScrubReplacesAndBounds(t *testing.T) {
	if got := Scrub("a sek b sek", []string{"sek", ""}); got != "a *** b ***" {
		t.Fatal(got)
	}
	got := Scrub("x"+strings.Repeat("é", 1000), nil)
	if len(got) > MaxLen+3 || !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("%d %v", len(got), utf8.ValidString(got))
	}
}
