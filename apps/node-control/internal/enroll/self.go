package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

// Headscale is what self-enrolment needs from headscale (#102).
type SelfHeadscale interface {
	CreateUser(ctx context.Context, name, displayName, email string) (headscale.User, error)
	ListUsers(ctx context.Context) ([]headscale.User, error)
	CreatePreAuthKey(ctx context.Context, userID uint64, tags []string, ttl time.Duration, ephemeral, reusable bool) (headscale.PreAuthKey, error)
	ListPreAuthKeys(ctx context.Context) ([]headscale.PreAuthKey, error)
	ExpirePreAuthKey(ctx context.Context, id uint64) error
	ListNodes(ctx context.Context) ([]headscale.Node, error)
	ExpireNode(ctx context.Context, id uint64) error
}

// Refresher re-checks a person at the issuer (oidc.Provider.Refresh).
type Refresher interface {
	Refresh(ctx context.Context, refreshToken, subject, groupsClaim string) (oidc.Identity, error)
}

// SelfConfig is the OIDC self-enrolment configuration.
type SelfConfig struct {
	AllowedGroups []string      // empty: any authenticated person
	GroupsClaim   string        // default "groups"
	MaxDevices    int           // active devices per person; default 3
	DeviceTTL     time.Duration // relay device lifetime; default 90 days
	KeyTTL        time.Duration // tailnet pre-auth key lifetime; default 1 hour
	Recheck       time.Duration // how often a person is re-checked at the issuer; default 1 hour
	SessionTTL    time.Duration // browser session; default 8 hours
	// InviteOnly: a person can enrol only with an invitation (OIDC_INVITE_ONLY). Default false: the
	// "Add a device" form of /me stays, as before.
	InviteOnly bool
	// InviteAnyEmail accepts an invitation also when the ID token has no email_verified claim
	// (OIDC_INVITE_REQUIRE_VERIFIED=false). The default needs the claim, and true: some issuers
	// let a person set the email of their account and do not send the claim.
	InviteAnyEmail bool
}

func (c *SelfConfig) defaults() {
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.MaxDevices == 0 {
		c.MaxDevices = 3
	}
	if c.DeviceTTL == 0 {
		c.DeviceTTL = 90 * 24 * time.Hour
	}
	if c.KeyTTL == 0 {
		c.KeyTTL = time.Hour
	}
	if c.Recheck == 0 {
		c.Recheck = time.Hour
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 8 * time.Hour
	}
}

// Self is the self-enrolment service: who may enrol, what one enrolment creates, and the locks.
type Self struct {
	cfg   SelfConfig
	users *SelfStore
	store Backend
	hs    SelfHeadscale
	idp   Refresher
	// issuer is OIDC_ISSUER, as in the iss claim. It finds the headscale user that
	// headscale's own OIDC login made.
	issuer string
	// push sends the credential set to every bridge at once (Admin.syncNow without a request).
	push func(ctx context.Context)
}

// OwnerPrefix marks the devices of self-enrolled people: the owner is "oidc:<subject>".
const OwnerPrefix = "oidc:"

func ownerOf(sub string) string { return OwnerPrefix + sub }

// Errors shown to the person.
var (
	ErrLocked      = errors.New("your access is locked; ask an administrator")
	ErrNotInGroup  = errors.New("you are not in a group that may enrol devices")
	ErrDeviceLimit = errors.New("you have reached the limit of active devices; revoke one first")
)

// Allowed says whether a person with groups may enrol.
func (s *Self) Allowed(groups []string) bool {
	if len(s.cfg.AllowedGroups) == 0 {
		return true
	}
	for _, g := range groups {
		if slices.Contains(s.cfg.AllowedGroups, g) {
			return true
		}
	}
	return false
}

// Login records a verified login. A locked person, or one outside the allowed groups, is refused.
func (s *Self) Login(id oidc.Identity) error {
	if u, err := s.users.User(id.Subject); err == nil && u.Locked {
		return ErrLocked
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if !s.Allowed(id.Groups) {
		return ErrNotInGroup
	}
	return s.users.RecordLogin(id.Subject, id.Email, id.EmailVerified, id.RefreshToken)
}

// Devices are the person's devices, newest first.
func (s *Self) Devices(sub string) ([]Device, error) {
	all, err := s.store.ListDevices()
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, d := range all {
		if d.Owner == ownerOf(sub) {
			out = append(out, d)
		}
	}
	slices.Reverse(out)
	return out, nil
}

func (s *Self) active(d Device) bool {
	return !d.Revoked && float64(d.ExpiresAt) > nowSec(s.store)
}

// Enrolment is what one self-enrolment gives the person, once.
type Enrolment struct {
	DeviceID     int64
	Subscription string // relay subscription URL
	Link         string // the client's import link
	TailnetKey   string // one-time pre-auth key for a regular tailnet client
	KeyExpires   time.Time
}

// Enrol creates a device for the person: a relay device (subscription) and a single-use pre-auth key of the person's
// own headscale user. The person must not be locked and must be under the device limit.
func (s *Self) Enrol(ctx context.Context, sub, name, client string, link IngressConfig) (Enrolment, error) {
	u, err := s.users.User(sub)
	if err != nil {
		return Enrolment{}, err
	}
	if u.Locked {
		return Enrolment{}, ErrLocked
	}
	devs, err := s.Devices(sub)
	if err != nil {
		return Enrolment{}, err
	}
	n := 0
	for _, d := range devs {
		if s.active(d) {
			n++
		}
	}
	if n >= s.cfg.MaxDevices {
		return Enrolment{}, ErrDeviceLimit
	}
	hsID, err := s.hsUser(ctx, u)
	if err != nil {
		return Enrolment{}, fmt.Errorf("headscale user: %w", err)
	}
	key, err := s.hs.CreatePreAuthKey(ctx, hsID, nil, s.cfg.KeyTTL, false, false)
	if err != nil {
		return Enrolment{}, fmt.Errorf("pre-auth key: %w", err)
	}
	exp := s.store.Clock()().Add(s.cfg.DeviceTTL).Unix()
	d, secret, err := s.store.CreateDevice(name, ownerOf(sub), client, exp)
	if err != nil {
		_ = s.hs.ExpirePreAuthKey(ctx, key.ID)
		return Enrolment{}, err
	}
	_ = s.store.Audit(ownerOf(sub), "device.self_enrol", name)
	s.push(ctx)
	subURL := SubURL(link, secret)
	dl, _ := DeepLinkFor(client, subURL, link.ProfileName)
	return Enrolment{DeviceID: d.ID, Subscription: subURL, Link: dl, TailnetKey: key.Key, KeyExpires: key.Expiration}, nil
}

// defaultSelfClient is the stored app of a device made on the self-service pages. The person picks
// the app on the picker page, whose links name the profile format; the stored value only sets the
// format of the plain /sub/ URL.
const defaultSelfClient = "sing-box"

// Redeem spends an invitation for a person who has just signed in and makes the device it grants.
// The email of the sign-in must be the invitation's, and the issuer must not say that it is
// unverified. When the device cannot be made (device limit, lock, a headscale failure), the
// invitation stays usable.
func (s *Self) Redeem(
	ctx context.Context, id oidc.Identity, token string, link IngressConfig,
) (Enrolment, Invite, error) {
	if id.EmailUnverified || (!id.EmailVerified && !s.cfg.InviteAnyEmail) {
		return Enrolment{}, Invite{}, ErrEmailUnverified
	}
	inv, err := s.users.ClaimInvite(ctx, token, id.Email, ownerOf(id.Subject))
	if err != nil {
		return Enrolment{}, Invite{}, err
	}
	e, err := s.Enrol(ctx, id.Subject, inv.DeviceName, defaultSelfClient, link)
	if err != nil {
		if rerr := s.users.ReleaseInvite(ctx, inv.ID); rerr != nil {
			slog.Warn("invitation not released", "error_type", errType(rerr))
		}
		return Enrolment{}, Invite{}, err
	}
	inv.DeviceID = e.DeviceID
	if err := s.users.InviteDevice(ctx, inv.ID, e.DeviceID); err != nil {
		slog.Warn("invitation device not recorded", "error_type", errType(err))
	}
	_ = s.store.Audit(ownerOf(id.Subject), "invite.redeem",
		fmt.Sprintf("invitation %d, device %d", inv.ID, e.DeviceID))
	return e, inv, nil
}

// hsUserName is the headscale user of a person: a stable, safe name from the subject.
func hsUserName(sub string) string {
	return "oidc-" + hashID(sub)[:16]
}

// ErrHSUserAmbiguous: more than one headscale user has the person's verified email, and
// none has the person's OIDC identity. node-control does not guess, and makes no new
// user either (#253).
var ErrHSUserAmbiguous = errors.New(
	"your account matches more than one tailnet user; ask an administrator")

// hsProviderID is the provider identifier that headscale's own OIDC login gives a user:
// "<issuer>/<subject>", with no slash at the end of the issuer or at the start of the
// subject (headscale OIDCClaims.Identifier).
func hsProviderID(issuer, sub string) string {
	if issuer == "" || sub == "" {
		return ""
	}
	return strings.TrimSuffix(issuer, "/") + "/" + strings.TrimPrefix(sub, "/")
}

// hsUser returns the person's headscale user, on the first device. It reuses a user that
// already belongs to the person, so that one person is one headscale user (#253). In this
// order:
//  1. the user that headscale's own OIDC login made for the same issuer and subject;
//  2. the user that an earlier attempt of node-control made (oidc-<hash>), not recorded;
//  3. when the issuer said that the email is verified: the one user with that email
//     (more than one: refuse);
//  4. else a new user oidc-<hash>.
func (s *Self) hsUser(ctx context.Context, u SelfUser) (uint64, error) {
	if u.HSUserID != 0 {
		return u.HSUserID, nil
	}
	name := hsUserName(u.Subject)
	users, err := s.hs.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	var id uint64
	if pid := hsProviderID(s.issuer, u.Subject); pid != "" {
		for _, x := range users {
			if x.ProviderID == pid {
				id = x.ID
			}
		}
	}
	if id == 0 {
		for _, x := range users {
			if x.Name == name {
				id = x.ID
			}
		}
	}
	if id == 0 && u.EmailVerified && u.Email != "" {
		var match []uint64
		for _, x := range users {
			if x.Email != "" && strings.EqualFold(x.Email, u.Email) {
				match = append(match, x.ID)
			}
		}
		switch len(match) {
		case 0:
		case 1:
			id = match[0]
		default:
			return 0, ErrHSUserAmbiguous
		}
	}
	if id == 0 {
		cu, err := s.hs.CreateUser(ctx, name, "", u.Email)
		if err != nil {
			return 0, err
		}
		id = cu.ID
	}
	return id, s.users.SetHSUser(u.Subject, id)
}

// RevokeOwn revokes one of the person's own devices.
func (s *Self) RevokeOwn(ctx context.Context, sub string, id int64) error {
	d, err := s.store.GetDevice(id)
	if err != nil {
		return err
	}
	if d.Owner != ownerOf(sub) {
		return ErrNotFound // never tell a person about another person's device
	}
	if err := s.store.Revoke(id); err != nil {
		return err
	}
	_ = s.store.Audit(ownerOf(sub), "device.self_revoke", d.Name)
	s.push(ctx)
	return nil
}

// Lock locks a person: no login, every relay device revoked and pushed at once, every tailnet node of the person's
// headscale user expired, and the user's unused pre-auth keys expired. It returns the first error but always does as
// much as it can: a lock that half works must not stop at the first failure.
func (s *Self) Lock(ctx context.Context, sub, by, reason string) error {
	if err := s.users.SetLocked(sub, true, by, reason); err != nil {
		return err
	}
	var errs []error
	devs, err := s.Devices(sub)
	errs = append(errs, err)
	revoked := 0
	for _, d := range devs {
		if !d.Revoked {
			errs = append(errs, s.store.Revoke(d.ID))
			revoked++
		}
	}
	if revoked > 0 {
		s.push(ctx)
	}
	errs = append(errs, s.expireTailnet(ctx, sub))
	_ = s.store.Audit(by, "person.lock", ownerOf(sub)+": "+reason)
	return errors.Join(errs...)
}

func (s *Self) expireTailnet(ctx context.Context, sub string) error {
	u, err := s.users.User(sub)
	if err != nil || u.HSUserID == 0 {
		return err
	}
	var errs []error
	nodes, err := s.hs.ListNodes(ctx)
	errs = append(errs, err)
	for _, n := range nodes {
		if n.UserID == u.HSUserID {
			errs = append(errs, s.hs.ExpireNode(ctx, n.ID))
		}
	}
	keys, err := s.hs.ListPreAuthKeys(ctx)
	errs = append(errs, err)
	now := time.Now() // headscale's expiry is wall time, not the enrolment store's clock
	for _, k := range keys {
		if k.UserID == u.HSUserID && !k.Used && k.Expiration.After(now) {
			errs = append(errs, s.hs.ExpirePreAuthKey(ctx, k.ID))
		}
	}
	return errors.Join(errs...)
}

// Unlock lets a person log in again. Revoked devices stay revoked and expired nodes stay expired: the person enrols
// again.
func (s *Self) Unlock(sub, by string) error {
	if err := s.users.SetLocked(sub, false, "", ""); err != nil {
		return err
	}
	_ = s.store.Audit(by, "person.unlock", ownerOf(sub))
	return nil
}

// Recheck asks the issuer about every unlocked person with a refresh token whose last check is older than the
// interval. A refused refresh (the person was disabled, or the grant revoked) or a person no longer in an allowed
// group is locked. An unreachable issuer locks nobody: it is recorded and shown, and the check runs again.
func (s *Self) Recheck(ctx context.Context) {
	users, err := s.users.Users()
	if err != nil {
		slog.Warn("oidc recheck: cannot list people", "error_type", errType(err))
		return
	}
	now := s.store.Clock()().Unix()
	for _, u := range users {
		if u.Locked || now-u.LastCheck < int64(s.cfg.Recheck/time.Second) {
			continue
		}
		rt, err := s.users.RefreshToken(u)
		if err != nil || rt == "" {
			continue // no refresh token: the issuer gave none; the session lifetime is the limit
		}
		id, err := s.idp.Refresh(ctx, rt, u.Subject, s.cfg.GroupsClaim)
		switch {
		case errors.Is(err, oidc.ErrDenied), errors.Is(err, oidc.ErrToken):
			if err := s.Lock(ctx, u.Subject, "oidc", "the identity provider refused the re-check"); err != nil {
				slog.Warn("oidc recheck: lock incomplete", "error_type", errType(err))
			}
		case err != nil:
			_ = s.users.Checked(u.Subject, false, "")
			slog.Warn("oidc recheck: issuer unreachable", "error_type", errType(err))
		case id.GroupsKnown && !s.Allowed(id.Groups):
			if err := s.Lock(ctx, u.Subject, "oidc", "no longer in an allowed group"); err != nil {
				slog.Warn("oidc recheck: lock incomplete", "error_type", errType(err))
			}
		default:
			_ = s.users.Checked(u.Subject, true, id.RefreshToken)
		}
	}
}

// RecheckLoop runs Recheck every minute until ctx ends; each person is due once per interval.
func (s *Self) RecheckLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.Recheck(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// subjectOfOwner returns the subject of an "oidc:" owner, ok=false for other owners.
func subjectOfOwner(owner string) (string, bool) {
	return strings.CutPrefix(owner, OwnerPrefix)
}
