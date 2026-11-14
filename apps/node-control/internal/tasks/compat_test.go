package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #300, #322: the runner API a runner reports is recorded; COMPAT_STRICT makes an incompatible runner, or one that
// reports no API version (older than 0.18.0), not healthy, so it gets no task. Without strict mode it stays healthy.
func TestRunnerAPICompat(t *testing.T) {
	for _, c := range []struct {
		body    string
		strict  bool
		wantOK  bool
		wantAPI int
	}{
		{`{"busy":false,"api":1}`, false, true, 1},
		{`{"busy":false,"api":7}`, false, true, 7},
		{`{"busy":false}`, false, true, 0},
		{`{"busy":false,"api":7}`, true, false, 7},
		{`{"busy":false}`, true, false, 0},
		{`{"busy":false,"api":1}`, true, true, 1},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(c.body))
		}))
		tn := &dialTailnet{target: srv.Listener.Addr().String()}
		newClient := NewRunnerClient
		if c.strict {
			newClient = NewStrictRunnerClient
		}
		r := newClient(tn, 8123, "tok", nil)
		ok, err := r.Health(context.Background(), "100.64.0.9")
		srv.Close()
		if err != nil || ok != c.wantOK {
			t.Errorf("%s strict=%v: ok=%v err=%v", c.body, c.strict, ok, err)
		}
		if got := r.(RunnerAPIs).APIs()["100.64.0.9"].API; got != c.wantAPI {
			t.Errorf("%s: api %d", c.body, got)
		}
	}
}
