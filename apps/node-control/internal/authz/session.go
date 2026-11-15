package authz

import (
	"slices"
	"sync"
	"time"
)

// Session groups (#366). An admin who signs in with OIDC on the console gets the groups of the ID
// token for a limited time. The sign-in is bound to the tailnet login (the session's login must be
// the login that WhoIs reports for the caller), so the groups are kept by login: every check that
// asks Can(login) also sees the login's groups, without a change at each call site. File grants and
// stored grants by login apply as before. The groups end at the session's expiry or at sign-out.

// maxSessionGroups caps the groups kept for one login.
const maxSessionGroups = 64

type sessionGroups struct {
	groups []string
	until  time.Time
}

var (
	groupsMu  sync.Mutex
	groupsBy  = map[string]sessionGroups{}
	groupsNow = time.Now
)

// SetSessionGroups records the groups of login until until. An empty list or a past time removes
// them. The caller has checked the session and its binding to login.
func SetSessionGroups(login string, groups []string, until time.Time) {
	l := NormalizeLogin(login)
	if l == "" {
		return
	}
	groupsMu.Lock()
	defer groupsMu.Unlock()
	if len(groups) == 0 || !until.After(groupsNow()) {
		delete(groupsBy, l)
		return
	}
	g := slices.Clone(groups)
	if len(g) > maxSessionGroups {
		g = g[:maxSessionGroups]
	}
	groupsBy[l] = sessionGroups{groups: g, until: until}
}

// SessionGroups returns the groups of login, nil when it has no live session.
func SessionGroups(login string) []string {
	l := NormalizeLogin(login)
	groupsMu.Lock()
	defer groupsMu.Unlock()
	s, ok := groupsBy[l]
	if !ok {
		return nil
	}
	if !s.until.After(groupsNow()) {
		delete(groupsBy, l)
		return nil
	}
	return slices.Clone(s.groups)
}

// subjectOf is the subject of login: the login and the groups of its live session.
func subjectOf(login string) Subject { return Subject{Login: login, Groups: SessionGroups(login)} }

// SubjectOf is subjectOf for modules that hold their own Policy (the access module).
func SubjectOf(login string) Subject { return subjectOf(login) }
