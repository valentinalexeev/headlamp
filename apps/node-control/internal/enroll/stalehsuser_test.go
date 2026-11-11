package enroll

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

// #288: the recorded headscale user of a person was deleted (for example a duplicate removed by hand, the #253
// workaround). The next device must find the right user again, not fail for good.
func TestSelfEnrolRecoversFromADeletedHeadscaleUser(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	// headscale's own OIDC user of the person, which the lookup must find
	f.hs.users = []headscale.User{{ID: 7, Name: "anna@example.test", Email: "anna@example.test", ProviderID: f.is.URL + "/sub-1"}}
	c := f.browser()
	csrf := f.signIn(c)
	// a stale record: user 99 is not in headscale
	if err := f.self.users.SetHSUser("sub-1", 99); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if code, page := f.enrol(c, csrf, "Phone"); code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	if len(f.hs.keys) != 1 || f.hs.keys[0].UserID != 7 {
		t.Fatalf("the key must be for the found user 7: %+v", f.hs.keys)
	}
	if u, _ := f.self.users.User("sub-1"); u.HSUserID != 7 {
		t.Errorf("the record must now be 7, got %d", u.HSUserID)
	}
	if !strings.Contains(logs.String(), "cause=user_not_found") || !strings.Contains(logs.String(), "step=headscale_user") {
		t.Errorf("the log must name the step and the cause: %s", logs.String())
	}
	// the next device uses the record again, without a second lookup change
	if code, page := f.enrol(c, csrf, "Laptop"); code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	if len(f.hs.users) != 1 || f.hs.keys[1].UserID != 7 {
		t.Fatalf("users %+v keys %+v", f.hs.users, f.hs.keys)
	}
}

// A failed step names itself in the log, and never with text from headscale.
func TestSelfEnrolFailureLogNamesTheStep(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	c := f.browser()
	csrf := f.signIn(c)
	f.hs.listErr = errFake("secret-token-from-headscale")
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if code, _ := f.enrol(c, csrf, "Phone"); code == 200 {
		t.Fatal("want a failure")
	}
	out := logs.String()
	if !strings.Contains(out, "step=headscale_user") || !strings.Contains(out, "cause=list_failed") {
		t.Errorf("no step in the log: %s", out)
	}
	if strings.Contains(out, "secret-token") {
		t.Errorf("the log holds text from headscale: %s", out)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
