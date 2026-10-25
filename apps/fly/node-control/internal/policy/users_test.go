package policy

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"example.com/node-control/internal/headscale"
)

type listerAPI struct {
	*fakeAPI
	users []string
	err   error
}

func (l *listerAPI) ListUsers(context.Context) ([]headscale.User, error) {
	if l.err != nil {
		return nil, l.err
	}
	var out []headscale.User
	for i, n := range l.users {
		out = append(out, headscale.User{ID: uint64(i + 1), Name: n})
	}
	return out, nil
}

func userWarnings(t *testing.T, api API, after string) []string {
	t.Helper()
	svc, _, _ := newTestService(t, api)
	p, err := svc.Preview(context.Background(), after)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, w := range p.Warnings {
		if len(w) > 5 && w[:5] == "user " {
			out = append(out, w)
		}
	}
	return out
}

func TestPreviewFlagsUnknownUsers(t *testing.T) {
	api := &listerAPI{fakeAPI: newFakeAPI("{}"), users: []string{"alice", "bob"}}
	after := `{
	  "groups": {"group:g": ["carol@", "alice@"]},
	  "tagOwners": {"tag:x": ["dave@", "bob@"]},
	  "acls": [
	    {"action":"accept","src":["ghost@","tag:x","group:g","*","10.0.0.0/8","host1","autogroup:internet"],
	     "dst":["erin@:22,80","alice@:*","autogroup:internet:*","tag:x:443","10.1.0.0/16:*","*:*","ghost@:1"]}
	  ],
	  "autoApprovers": {"exitNode": ["frank@"], "routes": {"10.0.0.0/8": ["bob@", "gina@"]}}
	}`
	want := []string{`user "carol" does not exist`, `user "dave" does not exist`, `user "erin" does not exist`,
		`user "frank" does not exist`, `user "ghost" does not exist`, `user "gina" does not exist`}
	if got := userWarnings(t, api, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestPreviewKnownUsersAndNonUsersNotFlagged(t *testing.T) {
	api := &listerAPI{fakeAPI: newFakeAPI("{}"), users: []string{"alice"}}
	after := `{"acls":[{"action":"accept","src":["alice@","tag:x","group:x","*","192.168.0.0/16","autogroup:member"],"dst":["*:*","autogroup:internet:*","host1:22"]}]}`
	if got := userWarnings(t, api, after); got != nil {
		t.Fatalf("got %q", got)
	}
}

func TestPreviewListUsersErrorAddsNoWarningAndNoFailure(t *testing.T) {
	api := &listerAPI{fakeAPI: newFakeAPI("{}"), err: errors.New("down")}
	svc, _, _ := newTestService(t, api)
	p, err := svc.Preview(context.Background(), `{"acls":[{"src":["ghost@"],"dst":["*:*"]}]}`)
	if err != nil || !p.Valid {
		t.Fatalf("%+v %v", p, err)
	}
	for _, w := range p.Warnings {
		if len(w) > 5 && w[:5] == "user " {
			t.Fatalf("warning %q", w)
		}
	}
}

func TestPreviewWithoutListerAddsNoUserWarning(t *testing.T) {
	if got := userWarnings(t, newFakeAPI("{}"), `{"acls":[{"src":["ghost@"],"dst":["*:*"]}]}`); got != nil {
		t.Fatalf("got %q", got)
	}
}
