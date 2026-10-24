package nodes

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIDCacheExpiresAndForgets(t *testing.T) {
	var c idCache
	t0 := time.Unix(1000, 0)
	c.put("k", "id1", t0)
	if id, ok := c.get("k", t0.Add(idCacheTTL-time.Second)); !ok || id != "id1" {
		t.Fatalf("%q %v", id, ok)
	}
	if _, ok := c.get("k", t0.Add(idCacheTTL)); ok {
		t.Fatal("expired entry served")
	}
	c.put("k", "id1", t0)
	c.forgetOnNotFound("k", &APIError{Status: 500})
	if _, ok := c.get("k", t0); !ok {
		t.Fatal("a 500 must not drop the entry")
	}
	c.forgetOnNotFound("k", errors.New("x"))
	c.forgetOnNotFound("k", &APIError{Status: 404})
	if _, ok := c.get("k", t0); ok {
		t.Fatal("404 must drop the entry")
	}
}

func TestOCIResolvedIDCachedWithinTTLAndDroppedOnNotFound(t *testing.T) {
	ctx := context.Background()
	f := &fakeOCI{instances: []map[string]string{ociInst("i1", "web", "RUNNING")}}
	p := newOCI(t, f, ociEnv(t))
	for i := 0; i < 2; i++ {
		if _, err := p.Status(ctx, ociTarget); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.lists) != 1 {
		t.Fatalf("two polls did %d list calls", len(f.lists))
	}
	f.instances = []map[string]string{ociInst("i2", "web", "RUNNING")} // i1 is gone
	var ae *APIError
	if _, err := p.Status(ctx, ociTarget); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("want the 404 for the stale id, got %v", err)
	}
	if st, err := p.Status(ctx, ociTarget); err != nil || st["status"] != "RUNNING" || len(f.lists) != 2 {
		t.Fatalf("%v %v lists=%d", st, err, len(f.lists))
	}
}

func TestYandexResolvedIDCachedWithinTTLAndDroppedOnNotFound(t *testing.T) {
	ctx := context.Background()
	f := &fakeYandex{instances: []yxInstance{{"i1", "web", "RUNNING"}}}
	p, c := newYandex(t, f, yxEnv(t))
	for i := 0; i < 2; i++ {
		if _, err := p.Status(ctx, yxTarget); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.lists) != 1 {
		t.Fatalf("two polls did %d list calls", len(f.lists))
	}
	c.add(idCacheTTL + time.Second)
	if _, err := p.Status(ctx, yxTarget); err != nil || len(f.lists) != 2 {
		t.Fatalf("expiry: %v lists=%d", err, len(f.lists))
	}
	f.instances = []yxInstance{{"i2", "web", "RUNNING"}}
	var ae *APIError
	if _, err := p.Status(ctx, yxTarget); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("want the 404 for the stale id, got %v", err)
	}
	if st, err := p.Status(ctx, yxTarget); err != nil || st["status"] != "RUNNING" || len(f.lists) != 3 {
		t.Fatalf("%v %v lists=%d", st, err, len(f.lists))
	}
}

func TestSelectelResolvedIDCachedWithinTTLAndDroppedOnNotFound(t *testing.T) {
	ctx := context.Background()
	f := &fakeNova{servers: []map[string]string{{"id": "id1", "name": "node-a"}}, status: map[string]string{"id1": "ACTIVE"}}
	p, c := newSelectel(t, f, selEnv)
	for i := 0; i < 2; i++ {
		if _, err := p.Status(ctx, selTarget); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.queries) != 1 {
		t.Fatalf("two polls did %d list calls", len(f.queries))
	}
	c.add(idCacheTTL + time.Second)
	if _, err := p.Status(ctx, selTarget); err != nil || len(f.queries) != 2 {
		t.Fatalf("expiry: %v lists=%d", err, len(f.queries))
	}
	f.servers = []map[string]string{{"id": "id2", "name": "node-a"}}
	f.status = map[string]string{"id2": "ACTIVE"}
	var ae *APIError
	if _, err := p.Status(ctx, selTarget); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("want the 404 for the stale id, got %v", err)
	}
	if st, err := p.Status(ctx, selTarget); err != nil || st["status"] != "RUNNING" || len(f.queries) != 3 {
		t.Fatalf("%v %v lists=%d", st, err, len(f.queries))
	}
}
