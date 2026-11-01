package tailnet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/headscale"
)

// keyWarn is how long before the API key expires the module starts to ask for a rotation.
const keyWarn = 30 * 24 * time.Hour

// Attention warns when the headscale API key is about to expire. A key that never expires raises nothing. A read
// that fails keeps reporting from the last expiry read successfully, so the item does not vanish when the key
// finally expires (an expired key is what makes the read fail); with no earlier read it raises nothing, as a
// failed read must not look like a problem with the tailnet. Headscale refusing the key (401 or 403) says so, unless
// the last read shows the key has simply run out. The module has no line of its own; its status is part of Nodes.
// With WithKeyViewers, nobody else sees it.
func (m *module) Attention(ctx context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.expiry == nil {
		return nil, nil, nil
	}
	if m.viewers != nil && !m.viewers(login) {
		return nil, nil, core.ErrNotVisible
	}
	at, err := m.keyExpiry(ctx)
	if err != nil {
		var ae *headscale.APIError
		m.mu.Lock()
		known := m.lastKey
		m.mu.Unlock()
		switch {
		case errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403) && (known.IsZero() || known.After(m.now())):
			m.mu.Lock()
			if m.refused.IsZero() {
				m.refused = m.now()
			}
			since := m.refused
			m.mu.Unlock()
			return []core.AttentionItem{{
				ID: "tailnet-api-key", Source: "tailnet", Level: core.LevelWaiting, AckKey: "tailnet-api-key|refused",
				Title:  "The headscale API key is refused",
				Detail: "The tailnet status line stops until it is replaced. Rotate it with scripts/rotate-headscale-api-key.sh.",
				Since:  since,
			}}, nil, nil
		case !known.IsZero():
			at = known
		default:
			return nil, nil, nil
		}
	} else if at.IsZero() {
		return nil, nil, nil
	}
	left := at.Sub(m.now())
	if left > keyWarn {
		return nil, nil, nil
	}
	title, key := "The headscale API key has expired", "tailnet-api-key|expired"
	if left > 0 {
		key = "tailnet-api-key|expiring"
		days := int(left / (24 * time.Hour))
		title = fmt.Sprintf("The headscale API key expires in %d %s", days, plural(days, "day", "days"))
		if days == 0 {
			title = "The headscale API key expires today"
		}
	}
	return []core.AttentionItem{{
		ID: "tailnet-api-key", Source: "tailnet", Level: core.LevelWaiting, Title: title, AckKey: key,
		Detail: "The tailnet status line stops when it does. Rotate it with scripts/rotate-headscale-api-key.sh.",
		Since:  at.Add(-keyWarn),
	}}, nil, nil
}

// expiryTTL is how long a read expiry is reused: it changes once a year, and Overview polls. A failed read is
// reused for failedTTL, so a headscale that is down is not asked on every poll either.
const (
	expiryTTL = 10 * time.Minute
	failedTTL = time.Minute
)

func (m *module) keyExpiry(ctx context.Context) (time.Time, error) {
	m.mu.Lock()
	ttl := expiryTTL
	if m.expErr != nil {
		ttl = failedTTL
	}
	if m.expSet && m.now().Sub(m.expAt) < ttl {
		at, err := m.expV, m.expErr
		m.mu.Unlock()
		return at, err
	}
	m.mu.Unlock()

	// The network call runs without the lock, once for all who wait. A caller that leaves is not a failed read, so
	// its leaving is not cached.
	return m.expRead.Do(ctx, "", func(c context.Context) (time.Time, error) {
		at, err := m.expiry(c)
		m.mu.Lock()
		m.expAt, m.expSet, m.expV, m.expErr = m.now(), true, at, err
		if err == nil {
			m.lastKey = at
			m.refused = time.Time{}
		}
		m.mu.Unlock()
		return at, err
	})
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
