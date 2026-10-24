package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"example.com/node-control/internal/headscale"
)

// rereadTimeout bounds the read that follows an ambiguous write failure.
const rereadTimeout = 10 * time.Second

// writeTimeout bounds a policy write. The write runs detached from the caller's cancellation, so a
// closed tab or a client timeout cannot abandon a half-sent change.
const writeTimeout = 30 * time.Second

// API is the part of the headscale client the service uses.
type API interface {
	GetPolicy(ctx context.Context) (text, updatedAt string, err error)
	SetPolicy(ctx context.Context, text string) error
	CheckPolicy(ctx context.Context, text string) error
}

// Service applies policy changes with a confirm-or-revert deadline. One mutex serialises apply,
// confirm and revert, and only one change can be pending at a time.
type Service struct {
	api      API
	store    *Store
	revert   time.Duration
	maxBytes int
	clock    func() time.Time
	mu       sync.Mutex
}

// NewService builds a service. A nil clock means time.Now.
func NewService(api API, st *Store, revert time.Duration, maxBytes int, clock func() time.Time) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{api: api, store: st, revert: revert, maxBytes: maxBytes, clock: clock}
}

// Current returns the live policy and its update stamp.
func (s *Service) Current(ctx context.Context) (text, updatedAt string, err error) {
	return s.api.GetPolicy(ctx)
}

// Outcome returns the state of a change.
func (s *Service) Outcome(id int64) Outcome { return s.store.Outcome(id) }

// ready checks that nothing is pending and reads the live policy. The caller holds s.mu.
func (s *Service) ready(ctx context.Context) (cur, updatedAt string, err error) {
	if _, pending, err := s.store.Pending(); err != nil {
		return "", "", err
	} else if pending {
		return "", "", ErrBusy
	}
	cur, updatedAt, err = s.api.GetPolicy(ctx)
	if err == nil && updatedAt == "" {
		return "", "", ErrNoVersion
	}
	return cur, updatedAt, err
}

// start stores the change and sends it to headscale; a rejected change leaves no trace. The
// caller holds s.mu.
func (s *Service) start(ctx context.Context, actor, reason, before, after string) (int64, time.Time, error) {
	if len(after) > s.maxBytes {
		return 0, time.Time{}, ErrTooLarge
	}
	deadline := s.clock().Add(s.revert)
	id, err := s.store.Begin(actor, reason, before, after, deadline)
	if err != nil {
		return 0, time.Time{}, err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := s.api.SetPolicy(wctx, after); err != nil {
		if err := s.settleWriteError(id, after, err); err != nil {
			if errors.Is(err, ErrUncertain) {
				return id, deadline, err
			}
			return 0, time.Time{}, err
		}
	}
	slog.Info("policy change applied", "actor", actor, "id", id, "reason", reasonClass(reason),
		"before_bytes", len(before), "after_bytes", len(after), "deadline", deadline.Unix())
	return id, deadline, nil
}

// settleWriteError decides what a failed SetPolicy means. Only a headscale rejection is definite:
// the change is dropped. Any other failure is ambiguous (headscale may have applied it, now or
// later), so the live policy is re-read: equal to after means success (nil); in every other case,
// including a failed re-read, the change stays pending with its revert armed (reverting a change
// that never landed is harmless) and ErrUncertain is returned.
func (s *Service) settleWriteError(id int64, after string, err error) error {
	var ae *headscale.APIError
	if errors.As(err, &ae) {
		s.abort(id)
		slog.Warn("policy change rejected", "id", id, "bytes", len(after), "cause", errCause(err))
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rereadTimeout)
	defer cancel()
	live, _, rerr := s.api.GetPolicy(ctx)
	if rerr == nil && live == after {
		slog.Warn("policy write reported an error but was applied", "id", id, "cause", errCause(err))
		return nil
	}
	slog.Error("policy write outcome unknown; revert armed", "id", id, "cause", errCause(err))
	return fmt.Errorf("%w: %w", ErrUncertain, err)
}

func (s *Service) abort(id int64) {
	if err := s.store.Abort(id); err != nil {
		slog.Error("policy change abort failed", "id", id, "err", err)
	}
}

// errCause describes an error for logs without the text a headscale message may quote.
func errCause(err error) string {
	var ae *headscale.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("headscale status %d", ae.Status)
	}
	return err.Error()
}

// reasonClass keeps free-form reasons out of the logs: only the fixed revert marker is named.
func reasonClass(reason string) string {
	if strings.HasPrefix(reason, "revert of") {
		return "revert"
	}
	return "edit"
}

// Apply replaces the policy with text, provided the live policy is still at baseUpdatedAt.
func (s *Service) Apply(ctx context.Context, actor, text, baseUpdatedAt, reason string) (int64, time.Time, error) {
	if len(text) > s.maxBytes {
		return 0, time.Time{}, ErrTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, upd, err := s.ready(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	if upd != baseUpdatedAt {
		return 0, time.Time{}, ErrConflict
	}
	return s.start(ctx, actor, reason, cur, text)
}

// Change applies transform(current policy) as one pending change. A transform error, or a result
// equal to the current text (ErrNoChange), applies nothing.
func (s *Service) Change(ctx context.Context, actor string, transform func(current string) (string, error), reason string) (int64, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, _, err := s.ready(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	next, err := transform(cur)
	if err != nil {
		return 0, time.Time{}, err
	}
	if next == cur {
		return 0, time.Time{}, ErrNoChange
	}
	return s.start(ctx, actor, reason, cur, next)
}

// Confirm accepts the pending change id so the watcher no longer reverts it, provided headscale
// still serves that change's policy. The deadline is deliberately not checked: a confirmation that
// arrives after the deadline but before the watcher has reverted is still accepted.
func (s *Service) Confirm(ctx context.Context, actor string, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok, err := s.store.Pending()
	if err != nil {
		return err
	}
	if !ok {
		return ErrNothingPending
	}
	if p.ID != id {
		return ErrNotPending
	}
	live, _, err := s.api.GetPolicy(ctx)
	if err != nil {
		return err
	}
	if live != p.After {
		return ErrNotLive
	}
	if err := s.store.Confirm(p.ID); err != nil {
		return err
	}
	slog.Info("policy change confirmed", "actor", actor, "id", p.ID)
	return nil
}

// RevertTo re-applies the policy that was live before history entry historyID, as a new change.
func (s *Service) RevertTo(ctx context.Context, actor string, historyID int64) (int64, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, err := s.store.TextOf(historyID, "before")
	if err != nil {
		return 0, time.Time{}, err
	}
	if len(text) > s.maxBytes {
		return 0, time.Time{}, ErrTooLarge
	}
	cur, _, err := s.ready(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	return s.start(ctx, actor, fmt.Sprintf("revert of #%d", historyID), cur, text)
}

// RunWatcher reverts a pending change past its deadline. It checks once synchronously first, so
// a deadline that passed while the process was down is handled at startup, then every interval
// until ctx is done.
func (s *Service) RunWatcher(ctx context.Context, interval time.Duration) {
	s.checkRevert(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkRevert(ctx)
		}
	}
}

func (s *Service) checkRevert(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok, err := s.store.Pending()
	if err != nil {
		slog.Error("policy pending state unreadable", "err", err)
		return
	}
	if !ok || s.clock().Before(p.Deadline) {
		return
	}
	if err := s.api.SetPolicy(ctx, p.Before); err != nil {
		if merr := s.store.MarkReverted(p.ID, false); merr != nil {
			slog.Error("policy revert bookkeeping failed", "id", p.ID, "err", merr)
		}
		slog.Error("policy revert failed", "id", p.ID, "attempts", p.Attempts+1, "cause", errCause(err))
		return
	}
	if err := s.store.MarkReverted(p.ID, true); err != nil {
		slog.Error("policy revert bookkeeping failed", "id", p.ID, "err", err)
		return
	}
	slog.Warn("policy change reverted at deadline", "id", p.ID, "actor", p.Actor, "bytes", len(p.Before))
}
