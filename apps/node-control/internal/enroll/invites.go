package enroll

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Invitations (bound self-enrolment): an administrator invites one email address. The link
// /i/<token> takes the person through the identity provider. When the verified email of the
// sign-in matches, the invitation is spent and one relay device is made for that person, who then
// picks the app on the self-service page. Only the hash of a token is stored. Invitations live in
// the OIDC store (OIDC_DB), beside the people they are for.

const inviteSchema = `
CREATE TABLE IF NOT EXISTS oidc_invites (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash  TEXT NOT NULL UNIQUE,
	email       TEXT NOT NULL,
	device_name TEXT NOT NULL,
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	used_at     INTEGER NOT NULL DEFAULT 0,
	used_by     TEXT NOT NULL DEFAULT '',
	device_id   INTEGER NOT NULL DEFAULT 0,
	revoked     INTEGER NOT NULL DEFAULT 0
);
`

// Invite states.
const (
	InvitePending = "pending"
	InviteUsed    = "used"
	InviteExpired = "expired"
	InviteRevoked = "revoked"
)

// inviteTokenBytes is the random part of an invitation link (64 hex characters).
const inviteTokenBytes = 32

// Invite is one invitation; never its token.
type Invite struct {
	ID         int64  `json:"id"`
	Email      string `json:"email"`
	DeviceName string `json:"device_name"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	UsedAt     int64  `json:"used_at"`
	UsedBy     string `json:"used_by"` // the owner of the device made with it: "oidc:<subject>"
	DeviceID   int64  `json:"device_id"`
	Revoked    bool   `json:"revoked"`
	State      string `json:"state"`
}

// Refusals of a redemption that the person may read.
var (
	ErrInviteInvalid = errors.New("this invitation is not valid: it was used, " +
		"withdrawn or has expired; ask an administrator for a new one")
	ErrInviteOtherEmail = errors.New("this invitation is for another account; " +
		"sign in with the email address it was sent to")
	ErrEmailUnverified = errors.New(
		"the identity provider does not confirm the email address of your account")
)

// normEmail is the comparable form of an email address.
func normEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// NewInvite stores an invitation and returns it with its token (shown once).
func (s *SelfStore) NewInvite(
	ctx context.Context, email, deviceName, by string, ttl time.Duration,
) (Invite, string, error) {
	tok := randomToken(inviteTokenBytes)
	now := s.now()
	inv := Invite{Email: normEmail(email), DeviceName: deviceName, CreatedBy: by, CreatedAt: now,
		ExpiresAt: s.clock().Add(ttl).Unix(), State: InvitePending}
	res, err := s.db.ExecContext(ctx, `INSERT INTO oidc_invites(token_hash, email, device_name,
		created_by, created_at, expires_at) VALUES(?,?,?,?,?,?)`,
		hashID(tok), inv.Email, deviceName, by, now, inv.ExpiresAt)
	if err != nil {
		return Invite{}, "", err
	}
	inv.ID, err = res.LastInsertId()
	return inv, tok, err
}

const inviteCols = "id, email, device_name, created_by, created_at, expires_at, used_at, " +
	"used_by, device_id, revoked"

func (s *SelfStore) scanInvite(sc interface{ Scan(...any) error }) (Invite, error) {
	var in Invite
	var revoked int
	if err := sc.Scan(&in.ID, &in.Email, &in.DeviceName, &in.CreatedBy, &in.CreatedAt,
		&in.ExpiresAt, &in.UsedAt, &in.UsedBy, &in.DeviceID, &revoked); err != nil {
		return Invite{}, err
	}
	in.Revoked = revoked != 0
	switch {
	case in.UsedAt != 0:
		in.State = InviteUsed
	case in.Revoked:
		in.State = InviteRevoked
	case in.ExpiresAt <= s.now():
		in.State = InviteExpired
	default:
		in.State = InvitePending
	}
	return in, nil
}

// Invites lists the invitations, newest first.
func (s *SelfStore) Invites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+inviteCols+" FROM oidc_invites ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		in, err := s.scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// InviteByToken returns the invitation of a token, ErrNotFound when there is none.
func (s *SelfStore) InviteByToken(ctx context.Context, token string) (Invite, error) {
	in, err := s.scanInvite(s.db.QueryRowContext(ctx,
		"SELECT "+inviteCols+" FROM oidc_invites WHERE token_hash=?", hashID(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	return in, err
}

// RevokeInvite withdraws an unused invitation. A used one stays as it is: its device is revoked
// on the Devices page.
func (s *SelfStore) RevokeInvite(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE oidc_invites SET revoked=1 WHERE id=? AND used_at=0", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		err := s.db.QueryRowContext(ctx, "SELECT 1 FROM oidc_invites WHERE id=?", id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// ClaimInvite spends an invitation for a signed-in person whose verified email is email. The spend
// is atomic: of concurrent claims of one token, one succeeds. ReleaseInvite gives it back when the
// device cannot be made.
func (s *SelfStore) ClaimInvite(ctx context.Context, token, email, owner string) (Invite, error) {
	in, err := s.InviteByToken(ctx, token)
	if errors.Is(err, ErrNotFound) {
		return Invite{}, ErrInviteInvalid
	}
	if err != nil {
		return Invite{}, err
	}
	if in.State != InvitePending {
		return Invite{}, ErrInviteInvalid
	}
	if normEmail(email) == "" || normEmail(email) != in.Email {
		return Invite{}, ErrInviteOtherEmail
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `UPDATE oidc_invites SET used_at=?, used_by=?
		WHERE id=? AND used_at=0 AND revoked=0 AND expires_at>?`, now, owner, in.ID, now)
	if err != nil {
		return Invite{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Invite{}, ErrInviteInvalid
	}
	in.UsedAt, in.UsedBy, in.State = now, owner, InviteUsed
	return in, nil
}

// ReleaseInvite makes a claimed invitation usable again (the device could not be made).
func (s *SelfStore) ReleaseInvite(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE oidc_invites SET used_at=0, used_by='' WHERE id=? AND device_id=0", id)
	return err
}

// InviteDevice records the device an invitation made.
func (s *SelfStore) InviteDevice(ctx context.Context, id, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE oidc_invites SET device_id=? WHERE id=?", deviceID, id)
	return err
}
