// Package scrub keeps secrets and unbounded upstream text out of error details.
package scrub

import (
	"strings"
	"unicode/utf8"
)

// MaxLen is the longest text Scrub returns before the ellipsis.
const MaxLen = 512

// Scrub replaces every non-empty secret in text with "***" and cuts the result
// to MaxLen bytes at a rune boundary, appending "..." when it cut.
func Scrub(text string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			text = strings.ReplaceAll(text, sec, "***")
		}
	}
	if len(text) > MaxLen {
		cut := MaxLen
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "..."
	}
	return text
}
