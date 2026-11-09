package enroll

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

// #253: a person who already has a headscale user from headscale's own OIDC login must not get a second one.

func TestSelfEnrolReusesHeadscaleOIDCUser(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	// headscale's OIDC login made this user: name and email are the email, provider id is <issuer>/<sub>
	f.hs.users = []headscale.User{{ID: 7, Name: "anna@example.test", Email: "anna@example.test", ProviderID: f.is.URL + "/sub-1"}}
	c := f.browser()
	csrf := f.signIn(c)
	if code, page := f.enrol(c, csrf, "Phone"); code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	if len(f.hs.users) != 1 || f.hs.keys[0].UserID != 7 {
		t.Fatalf("a second headscale user was made, or the key is for another user: %+v %+v", f.hs.users, f.hs.keys)
	}
}

func TestSelfEnrolReusesTheOneUserWithTheVerifiedEmail(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	f.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = true }
	// for example a user that an operator made, or headscale OIDC with another issuer URL form
	f.hs.users = []headscale.User{{ID: 8, Name: "anna", Email: "Anna@Example.test"}, {ID: 9, Name: "bob", Email: "bob@example.test"}}
	c := f.browser()
	csrf := f.signIn(c)
	if code, page := f.enrol(c, csrf, "Phone"); code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	if len(f.hs.users) != 2 || f.hs.keys[0].UserID != 8 {
		t.Fatalf("users %+v keys %+v", f.hs.users, f.hs.keys)
	}
}

func TestSelfEnrolDoesNotMatchAnUnverifiedEmail(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	// no email_verified claim: the email may be chosen by the person, so it must not select a user
	f.hs.users = []headscale.User{{ID: 8, Name: "anna", Email: "anna@example.test"}}
	c := f.browser()
	csrf := f.signIn(c)
	if code, page := f.enrol(c, csrf, "Phone"); code != 200 {
		t.Fatalf("%d %s", code, page)
	}
	if len(f.hs.users) != 2 || f.hs.keys[0].UserID == 8 || !strings.HasPrefix(f.hs.users[1].Name, "oidc-") {
		t.Fatalf("users %+v keys %+v", f.hs.users, f.hs.keys)
	}
}

func TestSelfEnrolRefusesAnAmbiguousEmail(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	f.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = true }
	f.hs.users = []headscale.User{{ID: 8, Name: "anna", Email: "anna@example.test"}, {ID: 9, Name: "anna2", Email: "anna@example.test"}}
	c := f.browser()
	csrf := f.signIn(c)
	code, page := f.enrol(c, csrf, "Phone")
	if code != 409 || !strings.Contains(page, "more than one tailnet user") || len(f.hs.users) != 2 || len(f.hs.keys) != 0 {
		t.Fatalf("code %d users %+v keys %+v", code, f.hs.users, f.hs.keys)
	}
	if devs, _ := f.store.ListDevices(); len(devs) != 0 {
		t.Fatalf("a device was made: %+v", devs)
	}
}

func TestHSProviderID(t *testing.T) {
	for _, c := range [][3]string{
		{"https://idp.example.test", "abc", "https://idp.example.test/abc"},
		{"https://idp.example.test/", "abc", "https://idp.example.test/abc"},
		{"https://idp.example.test/realms/x", "/abc", "https://idp.example.test/realms/x/abc"},
		{"", "abc", ""},
	} {
		if got := hsProviderID(c[0], c[1]); got != c[2] {
			t.Errorf("hsProviderID(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// A database of 0.15.x has no email_verified column: the store adds it and keeps the people.
func TestSelfStoreAddsEmailVerifiedColumn(t *testing.T) {
	path := t.TempDir() + "/oidc.db"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(selfSchema, ",\n\temail_verified INTEGER NOT NULL DEFAULT 0", "", 1)
	if old == selfSchema {
		t.Fatal("the test no longer removes the column")
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO oidc_users(subject, email, hs_user_id) VALUES('sub-1','anna@example.test',7)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenSelfStore(path, testKey, func() time.Time { return time.Unix(adminNow, 0) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.User("sub-1")
	if err != nil || u.HSUserID != 7 || u.EmailVerified {
		t.Fatalf("%+v %v", u, err)
	}
	if err := s.RecordLogin("sub-1", "anna@example.test", true, ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.User("sub-1"); !u.EmailVerified {
		t.Fatalf("%+v", u)
	}
}
