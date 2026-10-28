package statestore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"tailscale.com/ipn"
)

const testKey = "funnel-ingress:tsnet"

func newTestRedis(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	r, err := NewRedis("redis://"+mr.Addr(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, mr
}

func TestRedisReadMissingIsErrStateNotExist(t *testing.T) {
	r, _ := newTestRedis(t)
	if _, err := r.ReadState("k"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("want ErrStateNotExist, got %v", err)
	}
}

func TestRedisWriteReadRoundTrip(t *testing.T) {
	r, mr := newTestRedis(t)
	want := []byte{0, 1, 2, 0xff, '\n'}
	if err := r.WriteState("k", want); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadState("k")
	if err != nil || string(got) != string(want) {
		t.Fatalf("got %q, %v", got, err)
	}
	if mr.HGet(testKey, "k") != string(want) {
		t.Fatal("value not stored in the hash under the state key")
	}
}

func TestRedisNilWriteDeletes(t *testing.T) {
	r, _ := newTestRedis(t)
	if err := r.WriteState("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteState("k", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadState("k"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("want ErrStateNotExist after delete, got %v", err)
	}
}

func TestRedisNeverUsesTTL(t *testing.T) {
	r, mr := newTestRedis(t)
	if err := r.WriteState("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL(testKey); ttl != 0 {
		t.Fatalf("state key has a TTL: %v", ttl)
	}
}

func TestRedisLen(t *testing.T) {
	r, _ := newTestRedis(t)
	if n, err := r.Len(); err != nil || n != 0 {
		t.Fatalf("empty: %d, %v", n, err)
	}
	r.WriteState("a", []byte("1"))
	r.WriteState("b", []byte("2"))
	if n, err := r.Len(); err != nil || n != 2 {
		t.Fatalf("two: %d, %v", n, err)
	}
}

func TestRedisUnreachableFailsFast(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	start := time.Now()
	_, err := NewRedis("redis://"+addr, testKey)
	if err == nil {
		t.Fatal("unreachable Redis accepted")
	}
	if d := time.Since(start); d > 9*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestRedisErrorsDoNotContainURL(t *testing.T) {
	const pw = "fake-pw-4f9a7c"
	url := "redis://default:" + pw + "@redis.example.com:6379"

	t.Run("parse error", func(t *testing.T) {
		for _, bad := range []string{
			"redis://default:" + pw + "@redis.example.com:notaport",
			"http://default:" + pw + "@redis.example.com:6379",
			"default:" + pw + "@redis.example.com",
		} {
			_, err := NewRedis(bad, testKey)
			if err == nil {
				t.Fatalf("%q accepted", bad)
			}
			if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "redis.example.com") {
				t.Fatalf("parse error leaks the URL: %v", err)
			}
		}
	})

	t.Run("operation error", func(t *testing.T) {
		r := &Redis{secrets: secretsFor(url, pw)}
		err := r.wrap("read", errors.New("dial failed for "+url+" with "+pw))
		if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), url) {
			t.Fatalf("operation error leaks the URL: %v", err)
		}
	})

	t.Run("connect error", func(t *testing.T) {
		mr := miniredis.RunT(t)
		addr := mr.Addr()
		mr.Close()
		_, err := NewRedis("redis://default:"+pw+"@"+addr, testKey)
		if err == nil || strings.Contains(err.Error(), pw) {
			t.Fatalf("connect error: %v", err)
		}
	})
}

func TestRedisCommandSet(t *testing.T) {
	const pw = "fake-pw-4f9a7c"
	mr := miniredis.RunT(t)
	mr.RequireUserAuth("default", pw)
	var mu sync.Mutex
	seen := map[string]bool{}
	var helloArgs []string
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		mu.Lock()
		seen[strings.ToUpper(cmd)] = true
		if strings.EqualFold(cmd, "HELLO") && helloArgs == nil {
			helloArgs = args
		}
		mu.Unlock()
		return false
	})
	r, err := NewRedis("redis://default:"+pw+"@"+mr.Addr(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.WriteState("a", []byte("1"))
	r.ReadState("a")
	r.Len()
	r.Begin([]string{"a"})
	r.Pending()
	r.Len() // with a marker present
	r.End()
	r.Begin([]string{"a"})
	r.Abort([]string{"a"})
	r.WriteState("a", nil)
	r.Fields()

	// go-redis always opens with HELLO; with Protocol 2 it is "HELLO 2 AUTH <user> <pass>", which authenticates
	// and keeps the connection on RESP2 (no RESP3, no CLIENT, no SELECT).
	want := []string{"HDEL", "HELLO", "HEXISTS", "HGET", "HKEYS", "HLEN", "HSET", "PING"}
	var got []string
	mu.Lock()
	for c := range seen {
		got = append(got, c)
	}
	mu.Unlock()
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("commands sent: %v, want %v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(helloArgs) < 4 || helloArgs[0] != "2" || !strings.EqualFold(helloArgs[1], "AUTH") {
		t.Fatalf("handshake is not HELLO 2 AUTH: %v", helloArgs)
	}
}

func TestRedisLogsAreScrubbedAndGoThroughSlog(t *testing.T) {
	const pw = "fake-pw-4f9a7c"
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	if _, err := NewRedis("redis://default:"+pw+"@"+addr, testKey); err == nil {
		t.Fatal("unreachable Redis accepted")
	}
	out := buf.String()
	if !strings.Contains(out, "failed to dial") {
		t.Fatalf("go-redis log did not reach slog: %q", out)
	}
	if strings.Contains(out, pw) {
		t.Fatalf("password in log: %q", out)
	}

	buf.Reset()
	url := "redis://default:" + pw + "@redis.example.com:6379"
	installLogger(secretsFor(url, pw))
	scrubLogger{}.Printf(context.Background(), "redis: dial %s: %v", url, errors.New("bad "+pw))
	if strings.Contains(buf.String(), pw) || strings.Contains(buf.String(), "redis.example.com") {
		t.Fatalf("secret in adapter output: %q", buf.String())
	}
}

func TestRedisMarkerIsReserved(t *testing.T) {
	r, _ := newTestRedis(t)
	if err := r.WriteState(MarkerField, []byte("x")); err == nil {
		t.Fatal("marker field writable as state")
	}
	r.Begin([]string{"a"})
	if _, err := r.ReadState(MarkerField); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("marker visible as state: %v", err)
	}
	if n, _ := r.Len(); n != 0 {
		t.Fatalf("marker counted: %d", n)
	}
}
