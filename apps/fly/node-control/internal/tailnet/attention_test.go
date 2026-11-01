package tailnet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
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

func TestTailnetAttentionReadsTheExpiryOncePerInterval(t *testing.T) {
	reads := 0
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m := New(fakeSrc{}, nil, WithKeyExpiry(func(context.Context) (time.Time, error) {
		reads++
		return now.Add(24 * time.Hour), nil
	})).(*module)
	m.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if items, _, _ := m.Attention(context.Background(), "alice@"); len(items) != 1 {
			t.Fatalf("%+v", items)
		}
	}
	if reads != 1 {
		t.Fatalf("read %d times", reads)
	}
	now = now.Add(11 * time.Minute)
	m.Attention(context.Background(), "alice@")
	if reads != 2 {
		t.Fatalf("read %d times after the interval", reads)
	}
}

func TestTailnetAttentionCachesAFailedReadBriefly(t *testing.T) {
	reads := 0
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m := New(fakeSrc{}, nil, WithKeyExpiry(func(context.Context) (time.Time, error) {
		reads++
		return time.Time{}, errors.New("headscale is down")
	})).(*module)
	m.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		m.Attention(context.Background(), "alice@")
	}
	if reads != 1 {
		t.Fatalf("a failed read was repeated: %d", reads)
	}
	now = now.Add(61 * time.Second)
	m.Attention(context.Background(), "alice@")
	if reads != 2 {
		t.Fatalf("a failed read must be retried after a minute: %d", reads)
	}
}

func TestTailnetAttentionDoesNotHoldTheLockDuringTheRead(t *testing.T) {
	inRead, release := make(chan struct{}), make(chan struct{})
	m := New(fakeSrc{}, nil, WithKeyExpiry(func(context.Context) (time.Time, error) {
		close(inRead)
		<-release
		return time.Time{}, nil
	})).(*module)
	done := make(chan struct{})
	go func() { m.Attention(context.Background(), "alice@"); close(done) }()
	<-inRead
	locked := make(chan struct{})
	go func() { m.mu.Lock(); m.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("the mutex is held across the network call")
	}
	close(release)
	<-done
}

func TestTailnetAttentionNamesAnExpiredKeyAfterTheReadFails(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	fail := false
	m := New(fakeSrc{}, nil, WithKeyExpiry(func(context.Context) (time.Time, error) {
		if fail {
			return time.Time{}, errors.New("headscale is down")
		}
		return now.Add(2 * time.Hour), nil
	})).(*module)
	m.now = func() time.Time { return now }
	if items, _, _ := m.Attention(context.Background(), "a"); len(items) != 1 || !strings.Contains(items[0].Title, "expires") {
		t.Fatalf("%+v", items)
	}
	fail = true
	now = now.Add(11 * time.Minute) // the good read is stale; the next read fails
	if items, _, _ := m.Attention(context.Background(), "a"); len(items) != 1 {
		t.Fatalf("a failed read before the expiry must keep warning from the last good read, got %+v", items)
	}
	now = now.Add(3 * time.Hour)
	items, _, _ := m.Attention(context.Background(), "a")
	if len(items) != 1 || !strings.Contains(items[0].Title, "has expired") {
		t.Fatalf("want an expired item, got %+v", items)
	}
}

func TestTailnetAttentionNamesARefusedKey(t *testing.T) {
	for _, status := range []int{401, 403} {
		_, _, err := attentionOf(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		items, _, _ := attentionOf(t, func(context.Context) (time.Time, error) {
			return time.Time{}, &headscale.APIError{Status: status, Message: "Unauthorized"}
		})
		if len(items) != 1 || !strings.Contains(items[0].Title, "is refused") || items[0].Level != core.LevelWaiting {
			t.Fatalf("status %d: %+v", status, items)
		}
	}
	items, _, _ := attentionOf(t, func(context.Context) (time.Time, error) {
		return time.Time{}, &headscale.APIError{Status: 500, Message: "boom"}
	})
	if len(items) != 0 {
		t.Fatalf("a 500 is not a refusal: %+v", items)
	}
}
