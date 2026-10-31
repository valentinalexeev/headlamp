package headscale

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// keyToken reduces an API key, or the prefix the key list shows, to the part that names it:
// "hskey-api-<prefix>-<secret>", "<prefix>.<secret>", "hskey-api-<prefix>-***" and a bare "<prefix>"
// all give "<prefix>". The secret part is dropped without being kept anywhere.
func keyToken(s string) string {
	s = strings.TrimSuffix(strings.TrimRight(s, "*"), "-")
	if rest, ok := strings.CutPrefix(s, "hskey-api-"); ok {
		s = rest
		if i := strings.IndexByte(s, '-'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	return s
}

// APIKeyExpiry calls GET /api/v1/apikey and returns the expiry of the key this client uses, found by its
// prefix. The zero time means the key never expires. A key that is not in the list is an error.
func (c *Client) APIKeyExpiry(ctx context.Context) (time.Time, error) {
	const op = "listing API keys"
	body, err := c.call(ctx, op, http.MethodGet, "/api/v1/apikey", nil, maxBody)
	if err != nil {
		return time.Time{}, err
	}
	var out struct {
		Keys []struct {
			Prefix     string     `json:"prefix"`
			Expiration *time.Time `json:"expiration"`
		} `json:"apiKeys"`
	}
	if err := decode(op, body, &out); err != nil {
		return time.Time{}, err
	}
	want := keyToken(c.APIKey)
	if want == "" {
		return time.Time{}, errors.New(op + ": the configured key has no prefix to look for")
	}
	for _, k := range out.Keys {
		if keyToken(k.Prefix) != want {
			continue
		}
		if k.Expiration == nil {
			return time.Time{}, nil
		}
		return *k.Expiration, nil
	}
	return time.Time{}, fmt.Errorf("%s: the configured key is not in the list", op)
}
