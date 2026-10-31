package tailnet

import (
	"context"
	"fmt"
	"time"

	"example.com/node-control/internal/core"
)

// keyWarn is how long before the API key expires the module starts to ask for a rotation.
const keyWarn = 30 * 24 * time.Hour

// Attention warns when the headscale API key is about to expire. A key whose expiry cannot be read, or
// that never expires, raises nothing: the warning is advice, and a failed read must not look like a problem
// with the tailnet. The module has no line of its own; its status is part of Nodes.
func (m *module) Attention(ctx context.Context, _ string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.expiry == nil {
		return nil, nil, nil
	}
	at, err := m.keyExpiry(ctx)
	if err != nil || at.IsZero() {
		return nil, nil, nil
	}
	left := at.Sub(m.now())
	if left > keyWarn {
		return nil, nil, nil
	}
	title := "The headscale API key has expired"
	if left > 0 {
		days := int(left / (24 * time.Hour))
		title = fmt.Sprintf("The headscale API key expires in %d %s", days, plural(days, "day", "days"))
		if days == 0 {
			title = "The headscale API key expires today"
		}
	}
	return []core.AttentionItem{{
		ID: "tailnet-api-key", Source: "tailnet", Level: core.LevelWaiting, Title: title,
		Detail: "The tailnet status line stops when it does. Rotate it with scripts/rotate-headscale-api-key.sh.",
		Since:  at.Add(-keyWarn),
	}}, nil, nil
}

// expiryTTL is how long a read expiry is reused: it changes once a year, and Overview polls.
const expiryTTL = 10 * time.Minute

func (m *module) keyExpiry(ctx context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expOK && m.now().Sub(m.expAt) < expiryTTL {
		return m.expV, nil
	}
	at, err := m.expiry(ctx)
	if err != nil {
		return time.Time{}, err
	}
	m.expAt, m.expOK, m.expV = m.now(), true, at
	return at, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
