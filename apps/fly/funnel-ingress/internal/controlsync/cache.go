package controlsync

import (
	"errors"
)

// Blob stores one opaque value (statekv.NewBlob over the shared state store).
type Blob interface {
	Load() ([]byte, bool, error)
	Save([]byte) error
}

// Cache keeps the last good configuration so a restart can serve before node-control answers.
type Cache struct{ b Blob }

// NewCache wraps a blob.
func NewCache(b Blob) *Cache { return &Cache{b} }

// Load returns the cached configuration. (zero, false, nil) means nothing is cached; a corrupt blob is an
// error and is left untouched (the next successful Save replaces it).
func (c *Cache) Load() (Config, bool, error) {
	raw, ok, err := c.b.Load()
	if err != nil {
		return Config{}, false, errors.New("cannot read the cached configuration")
	}
	if !ok {
		return Config{}, false, nil
	}
	cfg, err := parse(raw)
	if err != nil || cfg.Version == "" {
		return Config{}, false, errors.New("the cached configuration is unreadable")
	}
	return cfg, true, nil
}

// Save stores cfg with one blob write. Callers save only when the version changed.
func (c *Cache) Save(cfg Config) error {
	raw, err := marshal(cfg)
	if err != nil {
		return err
	}
	return c.b.Save(raw)
}
