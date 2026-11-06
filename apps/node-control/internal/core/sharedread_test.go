package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedReadRunsOnceForConcurrentCallers(t *testing.T) {
	var s SharedRead[int]
	var calls atomic.Int32
	release := make(chan struct{})
	read := func(context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 7, nil
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := s.Do(context.Background(), "k", read); v != 7 || err != nil {
				t.Errorf("%d %v", v, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	// Keys are separate, and a finished read is not remembered.
	if v, err := s.Do(context.Background(), "other", func(context.Context) (int, error) { return 8, nil }); v != 8 || err != nil {
		t.Fatalf("%d %v", v, err)
	}
	if v, err := s.Do(context.Background(), "k", func(context.Context) (int, error) { return 9, nil }); v != 9 || err != nil {
		t.Fatalf("%d %v", v, err)
	}
}

func TestSharedReadOutlivesTheCallerThatStartedIt(t *testing.T) {
	var s SharedRead[string]
	started, release := make(chan struct{}), make(chan struct{})
	read := func(ctx context.Context) (string, error) {
		close(started)
		select {
		case <-release:
			return "value", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	first, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.Do(first, "k", read)
		errc <- err
	}()
	<-started
	res := make(chan string, 1)
	go func() {
		v, _ := s.Do(context.Background(), "k", read) // read would panic on a second start: started is closed
		res <- v
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left got %v", err)
	}
	close(release)
	if v := <-res; v != "value" {
		t.Fatalf("the caller that stayed got %q", v)
	}
}

func TestSharedReadPanicAndStuckReadGiveErrorsAndClear(t *testing.T) {
	old := attentionTimeout
	attentionTimeout = 30 * time.Millisecond
	defer func() { attentionTimeout = old }()
	var s SharedRead[int]
	if _, err := s.Do(context.Background(), "k", func(context.Context) (int, error) { panic("boom") }); err == nil {
		t.Fatal("a panic gave no error")
	}
	stuck := make(chan struct{})
	defer close(stuck)
	if _, err := s.Do(context.Background(), "k", func(context.Context) (int, error) { <-stuck; return 0, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stuck read gave %v", err)
	}
	if v, err := s.Do(context.Background(), "k", func(context.Context) (int, error) { return 1, nil }); v != 1 || err != nil {
		t.Fatalf("no new read after a stuck one: %d %v", v, err)
	}
}
