package core

import (
	"context"
	"sync"
)

// SharedRead lets the callers of one slow read, by key, share a single run of it. The read belongs to none of them:
// it runs on a context that keeps the first caller's values but not its cancellation, bounded like a source's answer
// (attentionTimeout), so a caller that leaves cannot turn the read into a failure for the others or for a cache the
// read fills. A caller that leaves stops waiting and gets its context's error. A read that panics or outlasts the
// bound gives an error, and the next caller starts a new one. The zero value is ready to use.
type SharedRead[T any] struct {
	mu    sync.Mutex
	calls map[string]*sharedCall[T]
}

type sharedCall[T any] struct {
	done chan struct{}
	v    T
	err  error
}

// Do joins the read of key that is running, or starts read. A cache belongs inside read, so that it is filled even
// when every caller has left.
func (s *SharedRead[T]) Do(ctx context.Context, key string, read func(context.Context) (T, error)) (T, error) {
	s.mu.Lock()
	c := s.calls[key]
	if c == nil {
		c = &sharedCall[T]{done: make(chan struct{})}
		if s.calls == nil {
			s.calls = map[string]*sharedCall[T]{}
		}
		s.calls[key] = c
		go func() {
			c.v, c.err = callSource(context.WithoutCancel(ctx), read)
			s.mu.Lock()
			delete(s.calls, key)
			s.mu.Unlock()
			close(c.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-c.done:
		return c.v, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}
