package sidecar

import (
	"context"
	"io"
	"net"
	"sync"
)

// Forward accepts connections on ln and copies bytes both ways to target (dialed with dial) until ctx is
// done or the listener fails. When one side of a connection closes, the other is closed too. It closes ln and
// every open connection and returns only after all its goroutines have finished.
func Forward(ctx context.Context, ln net.Listener, target string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
	)
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		conns[c] = struct{}{}
		return true
	}
	untrack := func(c net.Conn) {
		mu.Lock()
		delete(conns, c)
		mu.Unlock()
	}
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		close(stopped)
	}()

	for {
		in, err := ln.Accept()
		if err != nil {
			break // closed by the context, or a fatal listener error
		}
		if !track(in) {
			in.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer untrack(in)
			defer in.Close()
			out, err := dial(ctx, "tcp", target)
			if err != nil {
				return
			}
			if !track(out) {
				out.Close()
				return
			}
			defer untrack(out)
			defer out.Close()
			done := make(chan struct{}, 2)
			cp := func(dst, src net.Conn) {
				io.Copy(dst, src)
				// One direction ended: close both so the other direction unblocks.
				dst.Close()
				src.Close()
				done <- struct{}{}
			}
			go cp(out, in)
			go cp(in, out)
			<-done
			<-done
		}()
	}
	cancel()
	<-stopped
	wg.Wait()
}
