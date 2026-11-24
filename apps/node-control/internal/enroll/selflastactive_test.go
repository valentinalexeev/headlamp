package enroll

import (
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

// lastActiveRow is the "Last active" cell of the device row with the given name on /me (#450).
func lastActiveRow(t *testing.T, page, name string) string {
	t.Helper()
	i := strings.Index(page, "<td>"+name+"</td>")
	if i < 0 {
		t.Fatalf("no row %q in %s", name, page)
	}
	row := page[i:]
	if j := strings.Index(row, "</tr>"); j >= 0 {
		row = row[:j]
	}
	return row
}

// lastActiveSetup records the traffic, the profile download and the revocation of a case (#450).
func lastActiveSetup(t *testing.T, f *extendFx, traffic, prof int64, revoke bool) {
	t.Helper()
	if traffic > 0 {
		if err := f.store.TouchActive(f.dev.ID, traffic); err != nil {
			t.Fatal(err)
		}
	}
	if prof > 0 {
		if err := f.store.TouchProfile(f.dev.ID, prof); err != nil {
			t.Fatal(err)
		}
	}
	if revoke {
		if err := f.store.Revoke(f.dev.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMeLastActive(t *testing.T) {
	cases := []struct {
		name          string
		traffic, prof int64
		revoke        bool
		want          string
	}{
		{"traffic", adminNow - 3*3600, 0, false, "3 hours ago"},
		{"profile only", 0, adminNow - 2*86400, false, "2 days ago"},
		{"later wins", adminNow - 5*86400, adminNow - 120, false, "2 minutes ago"},
		{"never", 0, 0, false, "Never"},
		{"ended", adminNow - 40*86400, 0, true, "40 days ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExtendFx(t, extendCfg)
			lastActiveSetup(t, f, tc.traffic, tc.prof, tc.revoke)
			row := lastActiveRow(t, f.page(), "Phone")
			if !strings.Contains(row, tc.want) {
				t.Fatalf("want %q in %s", tc.want, row)
			}
			if tc.revoke && !strings.Contains(row, "ended") {
				t.Fatalf("not ended: %s", row)
			}
			if seen := max(tc.traffic, tc.prof); seen > 0 {
				title := time.Unix(seen, 0).UTC().Format("2006-01-02 15:04 UTC")
				if !strings.Contains(row, `title="`+title+`"`) {
					t.Fatalf("no exact UTC %q in %s", title, row)
				}
			}
		})
	}
}

// One person's /me never shows another person's devices or times (#450).
func TestMeLastActiveOwnOnly(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	a := f.browser()
	csrf := f.signIn(a)
	if code, p := f.enrol(a, csrf, "AnnaPhone"); code != 200 {
		t.Fatalf("%d %s", code, p)
	}
	f.is.SetUser(oidctest.User{Subject: "sub-2", Email: "bob@example.test",
		Groups: []string{"staff"}})
	b := f.browser()
	csrf = f.signIn(b)
	if code, p := f.enrol(b, csrf, "BobPhone"); code != 200 {
		t.Fatalf("%d %s", code, p)
	}
	devs, _ := f.store.ListDevices()
	for _, d := range devs {
		if d.Name == "BobPhone" {
			_ = f.store.TouchActive(d.ID, adminNow-7*3600)
		}
	}
	resp, err := a.Get(f.srv.URL + "/me")
	if err != nil {
		t.Fatal(err)
	}
	p := readAll(resp)
	if strings.Contains(p, "BobPhone") || strings.Contains(p, "7 hours ago") {
		t.Fatalf("Anna sees Bob's device: %s", p)
	}
	if !strings.Contains(lastActiveRow(t, p, "AnnaPhone"), "Never") {
		t.Fatalf("Anna's own row: %s", p)
	}
}

func TestAgo(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "1 minute ago", time.Minute: "1 minute ago", 59 * time.Minute: "59 minutes ago",
		25 * time.Hour: "1 day ago", 90 * 24 * time.Hour: "3 months ago",
		800 * 24 * time.Hour: "2 years ago",
	} {
		if got := ago(now, now.Add(-d).Unix()); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}
