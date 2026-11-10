package authz

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// An in-process caller (an MCP tool call) is checked against the policy with its groups, and never
// gets the open mode that lets every tailnet user through when no admin is configured.
func TestAuthorizerChecksInProcessCaller(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func(p *Policy, open bool, c core.Caller) int {
		a := &Authorizer{Policy: p, Tailnet: core.NewLocal("127.0.0.1"), Log: log, Open: open}
		h, err := a.Wrap("POST /nodes/{id}/stop", ok)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/nodes/n1/stop", nil)
		req = req.WithContext(core.WithCaller(req.Context(), c))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	empty, _ := NewPolicy(Config{})
	if got := run(empty, true, core.Caller{Login: "alice@"}); got != http.StatusForbidden {
		t.Fatalf("open mode must not apply to an in-process caller: %d", got)
	}
	p, err := NewPolicy(Config{Grants: []Grant{{Subject: "group:ops", Roles: []string{"operator"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(p, false, core.Caller{Login: "bob@example.com", Groups: []string{"ops"}}); got != 200 {
		t.Fatalf("a group grant must count for an in-process caller: %d", got)
	}
	if got := run(p, false, core.Caller{Login: "bob@example.com"}); got != http.StatusForbidden {
		t.Fatalf("no grant: %d", got)
	}
}

func TestMCPIsNotInWildcard(t *testing.T) {
	r := roleOf("*.admin")
	if r.Grants(P(FeatureMCP, LevelAdmin)) || !BuiltinRoles["owner"].Grants(P(FeatureMCP, LevelAdmin)) {
		t.Fatal("*.admin must not include mcp; owner must")
	}
	if BuiltinRoles["operator"].Grants(P(FeatureMCP, LevelView)) {
		t.Fatal("operator must not include mcp")
	}
}
