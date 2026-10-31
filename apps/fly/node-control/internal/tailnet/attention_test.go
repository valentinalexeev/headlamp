package tailnet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/core"
)

var _ core.AttentionSource = (*module)(nil)

func attentionOf(t *testing.T, expiry func(context.Context) (time.Time, error)) ([]core.AttentionItem, []core.SystemLine, error) {
	t.Helper()
	var opts []Option
	if expiry != nil {
		opts = append(opts, WithKeyExpiry(expiry))
	}
	m := New(fakeSrc{}, nil, opts...).(*module)
	m.now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	return m.Attention(context.Background(), "alice@")
}

func TestTailnetAttentionAPIKeyExpiry(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		expiry    func(context.Context) (time.Time, error)
		wantItem  bool
		wantTitle string
	}{
		{"under 30 days", func(context.Context) (time.Time, error) { return now.Add(12*24*time.Hour + time.Hour), nil }, true, "12 days"},
		{"one day left", func(context.Context) (time.Time, error) { return now.Add(30 * time.Hour), nil }, true, "1 day"},
		{"already expired", func(context.Context) (time.Time, error) { return now.Add(-time.Hour), nil }, true, "has expired"},
		{"exactly 30 days", func(context.Context) (time.Time, error) { return now.Add(30 * 24 * time.Hour), nil }, true, "30 days"},
		{"over 30 days", func(context.Context) (time.Time, error) { return now.Add(31 * 24 * time.Hour), nil }, false, ""},
		{"never expires", func(context.Context) (time.Time, error) { return time.Time{}, nil }, false, ""},
		{"expiry unreadable", func(context.Context) (time.Time, error) { return time.Time{}, errors.New("headscale is down") }, false, ""},
		{"no key source", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, lines, err := attentionOf(t, tc.expiry)
			if err != nil {
				t.Fatalf("an unreadable expiry is no reason to fail: %v", err)
			}
			if len(lines) != 0 {
				t.Errorf("lines: %+v", lines)
			}
			if !tc.wantItem {
				if len(items) != 0 {
					t.Fatalf("want no item, got %+v", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("want one item, got %+v", items)
			}
			it := items[0]
			if it.Level != core.LevelWaiting || it.Source != "tailnet" || it.ActionPath != "" || it.ActionLabel != "" {
				t.Errorf("%+v", it)
			}
			if !strings.Contains(it.Title, tc.wantTitle) {
				t.Errorf("title %q lacks %q", it.Title, tc.wantTitle)
			}
			if !strings.Contains(it.Detail, "rotate-headscale-api-key.sh") {
				t.Errorf("the detail must carry the rotation hint: %q", it.Detail)
			}
		})
	}
}
