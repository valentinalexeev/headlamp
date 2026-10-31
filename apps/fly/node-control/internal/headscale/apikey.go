package headscale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
	// An exact match on the prefix first; then a looser one, for a listing that shows a longer or shorter
	// prefix than the key's own (a listed prefix of at least six characters that begins the key, or the reverse).
	found := func(loose bool) (time.Time, bool) {
		for _, k := range out.Keys {
			p := keyToken(k.Prefix)
			match := p == want
			if loose && len(p) >= minLoosePrefix && len(want) >= minLoosePrefix {
				match = strings.HasPrefix(c.APIKey, k.Prefix) || strings.HasPrefix(want, p) || strings.HasPrefix(p, want)
			}
			if match {
				if k.Expiration == nil {
					return time.Time{}, true
				}
				return *k.Expiration, true
			}
		}
		return time.Time{}, false
	}
	if at, ok := found(false); ok {
		return at, nil
	}
	if at, ok := found(true); ok {
		return at, nil
	}
	if notListedWarned.CompareAndSwap(false, true) {
		// No key material: the log says only that the key is missing from the list.
		slog.Warn("headscale: the configured API key matches none of the listed keys, so its expiry cannot be warned about",
			"listed_keys", len(out.Keys))
	}
	return time.Time{}, fmt.Errorf("%s: the configured key is not in the list", op)
}

// minLoosePrefix is the shortest prefix the looser match accepts, so a short listed prefix cannot match by chance.
const minLoosePrefix = 6

// notListedWarned makes the missing-key warning appear once per process, not on every poll.
var notListedWarned atomic.Bool
