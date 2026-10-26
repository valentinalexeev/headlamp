package enroll

import (
	"errors"
	"time"
)

// ErrNotRevoked means a device cannot be deleted because it has not been revoked first.
var ErrNotRevoked = errors.New("device is not revoked")

// Backend is what the enrolment code needs from its persistent store. The SQLite Store is one
// implementation; the Redis store (redis.go) is the other. Both must behave identically: the
// contract tests in backend_contract_test.go run against each.
//
// An error is never an empty result: ActiveCredentials and ListDevices return an error when the
// store cannot be read, so a failure cannot be mistaken for "no devices" and pushed to an agent.
type Backend interface {
	// Clock returns the time source the store uses for expiry decisions.
	Clock() func() time.Time

	GetDevice(id int64) (Device, error)
	// CreateDevice adds a device with one fresh UUID per mode and returns it with its subscription
	// secret.
	CreateDevice(name, owner, client string, expiresAt int64) (Device, string, error)
	ListDevices() ([]Device, error)
	Revoke(deviceID int64) error
	// DeleteDevice removes a revoked device and its UUIDs and links for good. It returns ErrNotFound for an unknown
	// id and ErrNotRevoked, changing nothing, for a device that is not revoked. The id is never issued again.
	DeleteDevice(deviceID int64) error
	Extend(deviceID int64, expiresAt int64) error

	// SubscriptionSecret derives the current secret of a device (ErrNotFound if it does not exist).
	SubscriptionSecret(deviceID int64) (string, error)
	// RotateSecret invalidates the device's previous subscription secret.
	RotateSecret(deviceID int64) error
	// VerifySecret returns the active device a subscription secret belongs to, or nil.
	VerifySecret(secret string) (*Device, error)

	// NewLink creates a one-time link token; only its hash is stored.
	NewLink(deviceID int64, ttl time.Duration, maxUses int) (string, error)
	// ConsumeToken spends one use of a token and returns its device, or nil when the token is
	// unknown, used up, expired, or its device is revoked or expired. The spend is atomic: of N
	// concurrent calls on a token with one use left, exactly one succeeds.
	ConsumeToken(token string) (*Device, error)

	// ActiveCredentials returns one entry per mode of every non-revoked, unexpired device, ordered
	// by device id then mode. The result is never nil.
	ActiveCredentials() ([]Credential, error)

	ApprovalRows() (map[string]ApprovalRow, error)
	Approvals() (map[string]string, error)
	Approve(name, fingerprint string) error
	ApproveNode(name, fingerprint string, nodeID uint64, machineKey string) error
	Unapprove(name string) error

	Close() error
}

var _ Backend = (*Store)(nil)

// Clock returns the store's time source.
func (s *Store) Clock() func() time.Time { return s.clock }

// nowSec is the backend's clock as float seconds, matching the Python comparisons.
func nowSec(b Backend) float64 { return float64(b.Clock()().UnixNano()) / 1e9 }
