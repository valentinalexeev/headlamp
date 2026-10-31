package headscale

import (
	"context"
	"strings"
	"testing"
	"time"
)

const apiKeyList = `{"apiKeys":[
 {"id":"1","prefix":"oldprefix","expiration":"2026-01-01T00:00:00Z"},
 {"id":"2","prefix":"abcd1234","expiration":"2027-03-04T05:06:07Z"},
 {"id":"3","prefix":"hskey-api-wxyz5678","expiration":"2028-01-02T03:04:05Z"},
 {"id":"4","prefix":"forever"}]}`

func TestAPIKeyExpiryMatchesThePrefixOfTheConfiguredKey(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		want      time.Time
	}{
		{"new format, bare prefix listed", "hskey-api-abcd1234-SEC", time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)},
		{"new format, prefix with the scheme listed", "hskey-api-wxyz5678-SEC", time.Date(2028, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"old format", "oldprefix.SEC", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"no expiry", "forever.SEC", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, got := recorder(t, 200, apiKeyList)
			c.APIKey = tc.key
			at, err := c.APIKeyExpiry(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !at.Equal(tc.want) {
				t.Fatalf("expiry %v, want %v", at, tc.want)
			}
			if got.method != "GET" || got.path != "/api/v1/apikey" || got.auth != "Bearer "+tc.key {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestAPIKeyExpiryErrors(t *testing.T) {
	c, _ := recorder(t, 200, apiKeyList)
	c.APIKey = "nomatch.SEC"
	if _, err := c.APIKeyExpiry(context.Background()); err == nil {
		t.Fatal("a key that is not in the list must be an error")
	}
	c, _ = recorder(t, 500, `boom abcd1234.SEC`)
	c.APIKey = "abcd1234.SEC"
	_, err := c.APIKeyExpiry(context.Background())
	if err == nil || strings.Contains(err.Error(), "SEC") {
		t.Fatalf("want a scrubbed error, got %v", err)
	}
	c, _ = recorder(t, 200, `{"apiKeys":[{"id":"1","prefix":"abcd1234","expiration":"soon"}]}`)
	c.APIKey = "abcd1234.SEC"
	if _, err := c.APIKeyExpiry(context.Background()); err == nil {
		t.Fatal("an unreadable expiry must be an error")
	}
	if _, err := (&Client{}).APIKeyExpiry(context.Background()); err != ErrNotConfigured {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
