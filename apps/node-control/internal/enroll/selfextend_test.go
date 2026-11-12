package enroll

import (
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

// extendFx is a self-enrolled person with one device near its expiry.
type extendFx struct {
	*selfFx
	c    *http.Client
	csrf string
	dev  Device
}

func newExtendFx(t *testing.T, cfg SelfConfig) *extendFx {
	t.Helper()
	f := newSelfFx(t, cfg)
	c := f.browser()
	csrf := f.signIn(c)
	if code, page := f.enrol(c, csrf, "Phone"); code != 200 {
		t.Fatalf("enrol: %d %s", code, page)
	}
	devs, _ := f.store.ListDevices()
	if len(devs) != 1 {
		t.Fatalf("%+v", devs)
	}
	return &extendFx{selfFx: f, c: c, csrf: csrf, dev: devs[0]}
}

// at moves the clocks of the device store and the person store to t. The browser session of the
// enrolment has ended by then, so the person signs in again, as a real person would.
func (f *extendFx) at(t time.Time) {
	f.store.clock = func() time.Time { return t }
	f.self.users.clock = func() time.Time { return t }
	f.c = f.browser()
	f.csrf = f.signIn(f.c)
}

func (f *extendFx) extend(id int64) (int, string) {
	f.t.Helper()
	resp, err := f.c.PostForm(f.srv.URL+"/me/devices/"+strconv.FormatInt(id, 10)+"/extend",
		url.Values{"csrf": {f.csrf}})
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, readAll(resp)
}

func (f *extendFx) device() Device {
	f.t.Helper()
	d, err := f.store.GetDevice(f.dev.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func (f *extendFx) page() string {
	resp, err := f.c.Get(f.srv.URL + "/me")
	if err != nil {
		f.t.Fatal(err)
	}
	return readAll(resp)
}

var extendCfg = SelfConfig{AllowedGroups: []string{"staff"}, DeviceTTL: 90 * day, ExtendWindow: 14 * day,
	ExtendGrace: 30 * day}

// Acceptance (#301): a device near expiry, Extend on /me, the expiry moves, and the subscription URL
// and the UUIDs stay. The bridges get the new expiry.
func TestSelfExtendKeepsTheDevice(t *testing.T) {
	f := newExtendFx(t, extendCfg)
	secret, _ := f.store.SubscriptionSecret(f.dev.ID)
	exp := time.Unix(f.dev.ExpiresAt, 0)

	// Outside the window: no button.
	if strings.Contains(f.page(), "/extend") {
		t.Fatal("Extend shows 90 days before expiry")
	}
	now := exp.Add(-10 * day)
	f.at(now)
	if !strings.Contains(f.page(), `action="/me/devices/`+strconv.FormatInt(f.dev.ID, 10)+`/extend"`) {
		t.Fatal("no Extend button 10 days before expiry")
	}
	pushes := f.pushes
	code, page := f.extend(f.dev.ID)
	if code != 200 || !strings.Contains(page, "Signed in as") {
		t.Fatalf("extend: %d %s", code, page)
	}
	d := f.device()
	if want := now.Add(90 * day).Unix(); d.ExpiresAt != want {
		t.Fatalf("expires %d, want now + lifetime %d", d.ExpiresAt, want)
	}
	if !maps.Equal(d.UUIDs, f.dev.UUIDs) || d.ID != f.dev.ID || d.Revoked {
		t.Fatalf("device changed: %+v -> %+v", f.dev, d)
	}
	if s, _ := f.store.SubscriptionSecret(d.ID); s != secret {
		t.Fatal("the subscription URL changed")
	}
	if f.pushes != pushes+1 {
		t.Fatal("no push to the bridges")
	}
	for _, c := range f.agent.last().creds {
		if c.NotAfter != d.ExpiresAt {
			t.Fatalf("the bridge got not_after %d, want %d", c.NotAfter, d.ExpiresAt)
		}
	}
	if !strings.Contains(auditText(t, f.store), "device.self_extend") {
		t.Fatal("no audit entry")
	}
	// The new expiry is out of the window again.
	if strings.Contains(f.page(), "/extend") {
		t.Fatal("Extend still shows after the extend")
	}
}

// Acceptance (#301): outside the window the button is absent and the request gets 409.
func TestSelfExtendOutsideTheWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"too early":       -15 * day,
		"after the grace": 31 * day,
	}
	for name, off := range cases {
		t.Run(name, func(t *testing.T) {
			f := newExtendFx(t, extendCfg)
			f.at(time.Unix(f.dev.ExpiresAt, 0).Add(off))
			if strings.Contains(f.page(), "/extend") {
				t.Fatal("Extend shows outside the window")
			}
			if code, _ := f.extend(f.dev.ID); code != http.StatusConflict {
				t.Fatalf("got %d, want 409", code)
			}
			if f.device().ExpiresAt != f.dev.ExpiresAt {
				t.Fatal("the expiry moved")
			}
		})
	}
}

// In the grace time after the expiry, an ended device can still be extended.
func TestSelfExtendInTheGrace(t *testing.T) {
	f := newExtendFx(t, extendCfg)
	now := time.Unix(f.dev.ExpiresAt, 0).Add(20 * day)
	f.at(now)
	if !strings.Contains(f.page(), "/extend") {
		t.Fatal("no Extend button in the grace time")
	}
	if code, _ := f.extend(f.dev.ID); code != 200 {
		t.Fatalf("extend: %d", code)
	}
	if f.device().ExpiresAt != now.Add(90*day).Unix() {
		t.Fatal("the expiry did not move")
	}
}

// Acceptance (#301): a person no longer in an allowed group gets 403 and is locked, as the
// re-check does. The device does not change.
func TestSelfExtendNeedsTheGroupNow(t *testing.T) {
	f := newExtendFx(t, extendCfg)
	f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
	f.is.SetUser(oidctest.User{Subject: "sub-1", Groups: []string{"other"}})
	if code, _ := f.extend(f.dev.ID); code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", code)
	}
	if u, _ := f.self.users.User("sub-1"); !u.Locked {
		t.Fatal("the person is not locked")
	}
	// The lock revokes the device; the expiry stays as it was.
	if d := f.device(); d.ExpiresAt != f.dev.ExpiresAt || !d.Revoked {
		t.Fatalf("want the old expiry and revoked: %+v", d)
	}
}

// An issuer that cannot be reached refuses the extend, and locks nobody.
func TestSelfExtendIssuerDown(t *testing.T) {
	f := newExtendFx(t, extendCfg)
	f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
	f.is.Close()
	if code, _ := f.extend(f.dev.ID); code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", code)
	}
	if u, _ := f.self.users.User("sub-1"); u.Locked {
		t.Fatal("locked on an unreachable issuer")
	}
	if f.device().ExpiresAt != f.dev.ExpiresAt {
		t.Fatal("the expiry moved")
	}
}

// Another person's device, a revoked device, and self-extend off.
func TestSelfExtendRefusals(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		cfg := extendCfg
		cfg.ExtendWindow = 0
		f := newExtendFx(t, cfg)
		f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
		if strings.Contains(f.page(), "/extend") {
			t.Fatal("Extend shows while off")
		}
		if code, _ := f.extend(f.dev.ID); code != http.StatusConflict {
			t.Fatalf("got %d, want 409", code)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		f := newExtendFx(t, extendCfg)
		f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
		if err := f.store.Revoke(f.dev.ID); err != nil {
			t.Fatal(err)
		}
		if code, _ := f.extend(f.dev.ID); code != http.StatusConflict {
			t.Fatalf("got %d, want 409", code)
		}
	})
	t.Run("another person's device", func(t *testing.T) {
		f := newExtendFx(t, extendCfg)
		other, _, err := f.store.CreateDevice("Theirs", "oidc:sub-2", "sing-box", f.dev.ExpiresAt)
		if err != nil {
			t.Fatal(err)
		}
		f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
		// The same answer as an unknown id: back to /me, nothing changes.
		if code, _ := f.extend(other.ID); code != 200 {
			t.Fatalf("got %d", code)
		}
		if d, _ := f.store.GetDevice(other.ID); d.ExpiresAt != f.dev.ExpiresAt {
			t.Fatal("another person's device was extended")
		}
	})
	t.Run("no CSRF token", func(t *testing.T) {
		f := newExtendFx(t, extendCfg)
		f.at(time.Unix(f.dev.ExpiresAt, 0).Add(-day))
		resp, err := f.c.PostForm(f.srv.URL+"/me/devices/"+strconv.FormatInt(f.dev.ID, 10)+"/extend", nil)
		if err != nil {
			t.Fatal(err)
		}
		readAll(resp)
		if resp.StatusCode != http.StatusForbidden || f.device().ExpiresAt != f.dev.ExpiresAt {
			t.Fatalf("got %d", resp.StatusCode)
		}
	})
}

func TestSelfExtendConfigFromEnv(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "c", "OIDC_DB": "/x",
		"OIDC_REDIRECT_URL": "https://links.example/oidc/callback"}
	with := func(kv map[string]string) core.Env {
		m := maps.Clone(base)
		maps.Copy(m, kv)
		return core.MapEnv(m)
	}
	for _, tc := range []struct{ key, val string }{{"OIDC_EXTEND_WINDOW_DAYS", "400"}, {"OIDC_EXTEND_GRACE_DAYS", "-1"}} {
		_, _, _, err := selfConfig(with(map[string]string{tc.key: tc.val}))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%s: %v", tc.key, tc.val, err)
		}
	}
	_, sc, _, err := selfConfig(with(map[string]string{"OIDC_EXTEND_WINDOW_DAYS": "14", "OIDC_EXTEND_GRACE_DAYS": "30"}))
	if err != nil || sc.ExtendWindow != 14*day || sc.ExtendGrace != 30*day {
		t.Fatalf("%v %+v", err, sc)
	}
	// Unset: off.
	if _, sc, _, _ := selfConfig(with(nil)); sc.ExtendWindow != 0 {
		t.Fatal("self-extend is on by default")
	}
}

func auditText(t *testing.T, s *Store) string {
	t.Helper()
	es, err := s.AuditLog(50)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range es {
		b.WriteString(e.Action + " " + e.Detail + "\n")
	}
	return b.String()
}
