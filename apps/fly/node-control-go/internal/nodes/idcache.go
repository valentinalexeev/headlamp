package nodes

import (
	"sync"
	"time"
)

// idCacheTTL is how long a name->instance-id lookup is reused (spec 4.7: the
// discovery TTL), so the 8 s status poll does not list instances every time.
const idCacheTTL = 15 * time.Second

// idCache remembers resolved instance ids per lookup key. An entry is dropped
// when the instance turns out to be gone.
type idCache struct {
	mu sync.Mutex
	m  map[string]idEntry
}

type idEntry struct {
	id      string
	expires time.Time
}

func (c *idCache) get(key string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !now.Before(e.expires) {
		delete(c.m, key)
		return "", false
	}
	return e.id, true
}

func (c *idCache) put(key, id string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]idEntry{}
	}
	c.m[key] = idEntry{id: id, expires: now.Add(idCacheTTL)}
}

func (c *idCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// forgetOnNotFound drops key when err says the instance no longer exists.
func (c *idCache) forgetOnNotFound(key string, err error) {
	if ae, ok := err.(*APIError); ok && ae.Status == 404 {
		c.forget(key)
	}
}
