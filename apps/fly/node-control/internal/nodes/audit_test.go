package nodes

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

type recLog struct {
	mu      sync.Mutex
	entries []core.AuditEntry
	err     error
}

func (l *recLog) Audit(actor, action, detail string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	l.entries = append(l.entries, core.AuditEntry{Actor: actor, Action: action, Detail: detail})
	return nil
}

func (l *recLog) AuditLog(int) ([]core.AuditEntry, error) { return l.entries, nil }

type whoStub struct {
	core.Tailnet
	login string
	err   error
}

func (w whoStub) WhoIs(context.Context, string) (string, []string, error) { return w.login, nil, w.err }

func auditHandler(t *testing.T, tn core.Tailnet, lg *slog.Logger, audit func() core.AuditLog, provs map[string]Provider) http.Handler {
	t.Helper()
	m := New(testConfig(t), nil, provs, WithAudit(audit))
	r := &core.Routes{Admin: http.NewServeMux(), Tailnet: tn, Log: lg}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	return r.Admin
}

func TestNodeActionsAudited(t *testing.T) {
	lg := &recLog{}
	bad := &fakeProvider{act: func(string, Target) (string, string, error) { return "", "", ErrNotConfigured }}
	boom := &fakeProvider{act: func(string, Target) (string, string, error) {
		return "", "", &APIError{Status: 500, Message: "token=hunter2 rejected"}
	}}
	h := auditHandler(t, whoStub{login: "operator@example.net"}, nil, func() core.AuditLog { return lg },
		map[string]Provider{"yandex": okProvider(), "oci": bad, "fly": boom})
	do(h, "POST", "/nodes/up/start")
	do(h, "POST", "/nodes/up/stop")
	do(h, "POST", "/nodes/bad/start")
	do(h, "POST", "/nodes/down/stop")
	do(h, "POST", "/nodes/nope/start") // unknown node: not an action
	want := []core.AuditEntry{
		{Actor: "operator@example.net", Action: "node.start", Detail: "up: ok"},
		{Actor: "operator@example.net", Action: "node.stop", Detail: "up: ok"},
		{Actor: "operator@example.net", Action: "node.start", Detail: "bad: failed (not_configured)"},
		{Actor: "operator@example.net", Action: "node.stop", Detail: "down: failed (provider_error)"},
	}
	if len(lg.entries) != len(want) {
		t.Fatalf("entries = %+v", lg.entries)
	}
	for i, w := range want {
		if lg.entries[i] != w {
			t.Errorf("entry %d = %+v, want %+v", i, lg.entries[i], w)
		}
	}
	for _, e := range lg.entries {
		if strings.Contains(e.Detail, "hunter2") || strings.Contains(e.Detail, "SECRET") {
			t.Errorf("detail leaks: %q", e.Detail)
		}
	}
}

func TestNodeActionsWhoIsFailureAndLogFailure(t *testing.T) {
	lg := &recLog{}
	h := auditHandler(t, whoStub{err: errors.New("no")}, nil, func() core.AuditLog { return lg }, map[string]Provider{"yandex": okProvider()})
	if rec := do(h, "POST", "/nodes/up/start"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if len(lg.entries) != 1 || lg.entries[0].Actor != "unknown" {
		t.Fatalf("entries = %+v", lg.entries)
	}
	lg.err = errors.New("disk full")
	if rec := do(h, "POST", "/nodes/up/start"); rec.Code != 200 {
		t.Fatalf("a failing log failed the action: %d", rec.Code)
	}
}

func TestNodeActionsWithoutLogUseStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	for name, audit := range map[string]func() core.AuditLog{
		"no function": nil,
		"nil log":     func() core.AuditLog { return nil },
	} {
		buf.Reset()
		h := auditHandler(t, whoStub{login: "operator@example.net"}, lg, audit, map[string]Provider{"yandex": okProvider()})
		if rec := do(h, "POST", "/nodes/up/stop"); rec.Code != 200 {
			t.Fatal(name, rec.Code)
		}
		out := buf.String()
		for _, frag := range []string{"msg=audit", "actor=operator@example.net", "action=node.stop", `detail="up: ok"`} {
			if !strings.Contains(out, frag) {
				t.Errorf("%s: %q lacks %q", name, out, frag)
			}
		}
	}
}
