package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestJoinTailnetFailsOnUpError(t *testing.T) {
	err := joinTailnet(context.Background(), func(context.Context) error { return errors.New("bad auth key") }, time.Second)
	if err == nil || !strings.Contains(err.Error(), "bad auth key") {
		t.Fatalf("%v", err)
	}
}

func TestJoinTailnetBoundedByTimeout(t *testing.T) {
	start := time.Now()
	err := joinTailnet(context.Background(), func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, 50*time.Millisecond)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestJoinTailnetOK(t *testing.T) {
	if err := joinTailnet(context.Background(), func(context.Context) error { return nil }, time.Second); err != nil {
		t.Fatal(err)
	}
}
