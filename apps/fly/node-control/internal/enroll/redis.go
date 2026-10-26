package enroll

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"example.com/node-control/internal/scrub"
	"example.com/node-control/internal/statestore"
)

// RedisStore keeps the enrolment state in a shared Redis, under one key prefix ("<app>:<name>"), without any
// volume. Layout (all keys DB 0):
//
//	<prefix>:meta        hash; field seq = last device id (never reused, like SQLite AUTOINCREMENT)
//	<prefix>:d:<id>      hash per device, no TTL: meta = sealed JSON {name, owner, client, uuids}; plain numbers
//	                     expires_at, revoked, epoch (so revoke, extend and rotate are single atomic commands) and
//	                     enrolled_at, enrol_count, last_active (activity times; absent in older records, read as 0)
//	<prefix>:t:<hash>    hash per one-time link, with a TTL: device_id, expires, max_uses, uses
//	<prefix>:approvals   hash; field = ingress name, value = sealed JSON
//
// Devices are never expired by Redis. A link's key expires through its TTL, and every spend also checks the stored
// expiry, so an eviction or clock surprise cannot revive a link. Commands sent: EVAL (never EVALSHA), HGET, HGETALL,
// HINCRBY, HSET, HDEL; the scripts also use EXISTS, HSET, HINCRBY and PEXPIRE. Nothing scans, selects a database or
// flushes. Error texts never contain the Redis URL or its password.
type RedisStore struct {
	rdb     *redis.Client
	cipher  *statestore.Cipher // nil: values are stored unsealed (only when the operator allowed plaintext)
	prefix  string
	key     []byte
	clock   func() time.Time
	secrets []string
}

var _ Backend = (*RedisStore)(nil)

// linkTTLMargin is added to a link's lifetime for the Redis TTL, which is only a cleanup: the stored expiry decides.
const linkTTLMargin = time.Minute

// redisOpTimeout bounds every single store operation.
const redisOpTimeout = 10 * time.Second

// NewRedisStore returns a store over an open client (see statestore.OpenClient). secrets are scrubbed from every
// error. prefix must look like "<app>:<name>". secretKey (at least 16 bytes) derives subscription secrets exactly as
// the SQLite store does, so secrets issued before a migration keep working. A nil clock means time.Now.
func NewRedisStore(rdb *redis.Client, secrets []string, c *statestore.Cipher, prefix string, secretKey []byte, clock func() time.Time) (*RedisStore, error) {
	if len(secretKey) < 16 {
		return nil, errors.New("secret key must be at least 16 bytes")
	}
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	return &RedisStore{rdb: rdb, cipher: c, prefix: prefix, key: append([]byte(nil), secretKey...), clock: clock, secrets: secrets}, nil
}

func validatePrefix(p string) error {
	if strings.ContainsAny(p, `*?[\`) {
		return errors.New("the enrolment Redis prefix must not contain glob characters (* ? [ \\)")
	}
	app, rest, ok := strings.Cut(p, ":")
	if !ok || app == "" || rest == "" {
		return errors.New(`the enrolment Redis prefix must look like "<app>:<name>" with a non-empty app prefix`)
	}
	return nil
}

func (s *RedisStore) metaKey() string      { return s.prefix + ":meta" }
func (s *RedisStore) approvalsKey() string { return s.prefix + ":approvals" }
func (s *RedisStore) deviceKey(id int64) string {
	return s.prefix + ":d:" + strconv.FormatInt(id, 10)
}
func (s *RedisStore) tokenKey(hash string) string { return s.prefix + ":t:" + hash }

func (s *RedisStore) Clock() func() time.Time { return s.clock }

// HasData reports whether the store holds enrolment data (the device counter exists). A restore sets the counter
// last, so a half-finished one reads as empty.
func (s *RedisStore) HasData() (bool, error) {
	ctx, cancel := opCtx()
	defer cancel()
	ok, err := s.rdb.HExists(ctx, s.metaKey(), "seq").Result()
	if err != nil {
		return false, s.wrap("checking the enrolment data in redis", err)
	}
	return ok, nil
}

func (s *RedisStore) Close() error { return s.rdb.Close() }

func (s *RedisStore) wrap(op string, err error) error {
	return errors.New(op + ": " + scrub.Scrub(err.Error(), s.secrets))
}

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), redisOpTimeout)
}

func fnum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// deviceMeta is the sealed part of a device record: everything that identifies a person or is a credential.
type deviceMeta struct {
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	Client string            `json:"client"`
	UUIDs  map[string]string `json:"uuids"`
}

// devRec is a device as stored, with the epoch the Device type does not carry.
type devRec struct {
	Device
	Epoch int64
}

func (s *RedisStore) parseDevice(id int64, h map[string]string) (devRec, error) {
	raw, ok := h["meta"]
	if !ok {
		return devRec{}, ErrNotFound
	}
	pt, err := s.cipher.Open(s.prefix, "d:"+strconv.FormatInt(id, 10), []byte(raw))
	if err != nil {
		return devRec{}, err
	}
	var m deviceMeta
	if err := json.Unmarshal(pt, &m); err != nil {
		return devRec{}, errors.New("corrupt device record")
	}
	exp, err1 := strconv.ParseInt(h["expires_at"], 10, 64)
	epoch, err2 := strconv.ParseInt(h["epoch"], 10, 64)
	if err1 != nil || err2 != nil {
		return devRec{}, errors.New("corrupt device record")
	}
	if m.UUIDs == nil {
		m.UUIDs = map[string]string{}
	}
	// The activity numbers are optional: records written before they existed read as 0.
	opt := func(k string) int64 {
		v, _ := strconv.ParseInt(h[k], 10, 64)
		return v
	}
	return devRec{
		Device: Device{ID: id, Name: m.Name, Owner: m.Owner, Client: m.Client, ExpiresAt: exp, Revoked: h["revoked"] == "1", UUIDs: m.UUIDs,
			EnrolledAt: opt("enrolled_at"), Enrolments: opt("enrol_count"), LastActive: opt("last_active")},
		Epoch: epoch,
	}, nil
}

func (s *RedisStore) readDevice(ctx context.Context, id int64) (devRec, error) {
	h, err := s.rdb.HGetAll(ctx, s.deviceKey(id)).Result()
	if err != nil {
		return devRec{}, s.wrap("reading device from redis", err)
	}
	if len(h) == 0 {
		return devRec{}, ErrNotFound
	}
	return s.parseDevice(id, h)
}

func (s *RedisStore) seq(ctx context.Context) (int64, error) {
	v, err := s.rdb.HGet(ctx, s.metaKey(), "seq").Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, s.wrap("reading device counter from redis", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, errors.New("corrupt device counter")
	}
	return n, nil
}

func (s *RedisStore) GetDevice(id int64) (Device, error) {
	ctx, cancel := opCtx()
	defer cancel()
	r, err := s.readDevice(ctx, id)
	if err != nil {
		return Device{}, err
	}
	return r.Device, nil
}

func (s *RedisStore) putDevice(ctx context.Context, d Device, epoch int64) error {
	pt, err := json.Marshal(deviceMeta{Name: d.Name, Owner: d.Owner, Client: d.Client, UUIDs: d.UUIDs})
	if err != nil {
		return err
	}
	sealed, err := s.cipher.Seal(s.prefix, "d:"+strconv.FormatInt(d.ID, 10), pt)
	if err != nil {
		return err
	}
	revoked := "0"
	if d.Revoked {
		revoked = "1"
	}
	// One HSET with all fields: a device record is written whole or not at all.
	if err := s.rdb.HSet(ctx, s.deviceKey(d.ID),
		"meta", sealed, "expires_at", strconv.FormatInt(d.ExpiresAt, 10), "revoked", revoked, "epoch", strconv.FormatInt(epoch, 10),
		"enrolled_at", strconv.FormatInt(d.EnrolledAt, 10), "enrol_count", strconv.FormatInt(d.Enrolments, 10),
		"last_active", strconv.FormatInt(d.LastActive, 10)).Err(); err != nil {
		return s.wrap("writing device to redis", err)
	}
	return nil
}

func (s *RedisStore) CreateDevice(name, owner, client string, expiresAt int64) (Device, string, error) {
	ctx, cancel := opCtx()
	defer cancel()
	id, err := s.rdb.HIncrBy(ctx, s.metaKey(), "seq", 1).Result()
	if err != nil {
		return Device{}, "", s.wrap("allocating device id in redis", err)
	}
	d := Device{ID: id, Name: name, Owner: owner, Client: client, ExpiresAt: expiresAt, UUIDs: map[string]string{}}
	for _, m := range Modes {
		d.UUIDs[m] = newUUID()
	}
	if err := s.putDevice(ctx, d, 0); err != nil {
		return Device{}, "", err
	}
	secret, err := s.SubscriptionSecret(id)
	if err != nil {
		return Device{}, "", err
	}
	return d, secret, nil
}

func (s *RedisStore) mac(deviceID, epoch int64) string {
	m := hmac.New(sha256.New, s.key)
	fmt.Fprintf(m, "sub:%d:%d", deviceID, epoch)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func (s *RedisStore) SubscriptionSecret(deviceID int64) (string, error) {
	ctx, cancel := opCtx()
	defer cancel()
	v, err := s.rdb.HGet(ctx, s.deviceKey(deviceID), "epoch").Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", s.wrap("reading device from redis", err)
	}
	epoch, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return "", errors.New("corrupt device record")
	}
	return strconv.FormatInt(deviceID, 10) + "." + s.mac(deviceID, epoch), nil
}

// updateScript changes one field of an existing device in a single step. It never creates a record: on a missing
// key it returns 0 and writes nothing (a bare HSET or HINCRBY would leave a stray, half-empty device).
var updateScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if ARGV[1] == 'revoke' then redis.call('HSET', KEYS[1], 'revoked', '1')
elseif ARGV[1] == 'extend' then redis.call('HSET', KEYS[1], 'expires_at', ARGV[2])
elseif ARGV[1] == 'rotate' then redis.call('HINCRBY', KEYS[1], 'epoch', 1)
else return redis.error_reply('unknown op') end
return 1`)

func (s *RedisStore) update(op string, deviceID int64, arg string) error {
	ctx, cancel := opCtx()
	defer cancel()
	// Eval, not Run: Run would send EVALSHA, which the shared-Redis rules do not list.
	if err := updateScript.Eval(ctx, s.rdb, []string{s.deviceKey(deviceID)}, op, arg).Err(); err != nil {
		return s.wrap("updating device in redis", err)
	}
	return nil // a missing device is a no-op, as in the SQLite store
}

func (s *RedisStore) RotateSecret(deviceID int64) error { return s.update("rotate", deviceID, "") }
func (s *RedisStore) Revoke(deviceID int64) error       { return s.update("revoke", deviceID, "") }
func (s *RedisStore) Extend(deviceID int64, expiresAt int64) error {
	return s.update("extend", deviceID, strconv.FormatInt(expiresAt, 10))
}

// deleteScript removes a device record only if it exists and is revoked, in one step (an active device is never deleted
// by a racing request). It returns 1 when deleted, 0 when the device is not revoked and -1 when it does not exist.
var deleteScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return -1 end
if redis.call('HGET', KEYS[1], 'revoked') ~= '1' then return 0 end
redis.call('DEL', KEYS[1])
return 1`)

// DeleteDevice removes a revoked device. Its links cannot be listed without a scan, so they stay until their TTL
// ends; a spend of one fails at once, because the device no longer exists. The counter is untouched, so the id is
// never issued again.
func (s *RedisStore) DeleteDevice(deviceID int64) error {
	ctx, cancel := opCtx()
	defer cancel()
	res, err := deleteScript.Eval(ctx, s.rdb, []string{s.deviceKey(deviceID)}).Int()
	if err != nil {
		return s.wrap("deleting device in redis", err)
	}
	switch res {
	case 1:
		return nil
	case 0:
		return ErrNotRevoked
	}
	return ErrNotFound
}

func (s *RedisStore) listRecs(ctx context.Context) ([]devRec, error) {
	n, err := s.seq(ctx)
	if err != nil {
		return nil, err
	}
	// Devices are never deleted, so ids 1..seq are the whole set (gaps from a crash between the id and the record
	// are skipped). A plain pipeline: no scan.
	cmds := make([]*redis.MapStringStringCmd, n)
	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := int64(1); i <= n; i++ {
			cmds[i-1] = p.HGetAll(ctx, s.deviceKey(i))
		}
		return nil
	})
	if err != nil {
		return nil, s.wrap("listing devices in redis", err)
	}
	var out []devRec
	for i, c := range cmds {
		h, err := c.Result()
		if err != nil {
			return nil, s.wrap("listing devices in redis", err)
		}
		if len(h) == 0 {
			continue
		}
		r, err := s.parseDevice(int64(i+1), h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *RedisStore) ListDevices() ([]Device, error) {
	ctx, cancel := opCtx()
	defer cancel()
	recs, err := s.listRecs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Device, len(recs))
	for i, r := range recs {
		out[i] = r.Device
	}
	return out, nil
}

func (s *RedisStore) ActiveCredentials() ([]Credential, error) {
	ctx, cancel := opCtx()
	defer cancel()
	recs, err := s.listRecs(ctx)
	if err != nil {
		return nil, err
	}
	now := nowSec(s)
	out := []Credential{}
	for _, r := range recs {
		if r.Revoked || float64(r.ExpiresAt) <= now {
			continue
		}
		modes := make([]string, 0, len(r.UUIDs))
		for m := range r.UUIDs {
			modes = append(modes, m)
		}
		sort.Strings(modes)
		for _, m := range modes {
			out = append(out, Credential{UUID: r.UUIDs[m], Mode: m, NotAfter: r.ExpiresAt})
		}
	}
	return out, nil
}

func (s *RedisStore) VerifySecret(secret string) (*Device, error) {
	idStr, mac, ok := strings.Cut(secret, ".")
	if !ok || idStr == "" || len(idStr) > 12 {
		return nil, nil
	}
	for i := 0; i < len(idStr); i++ {
		if idStr[i] < '0' || idStr[i] > '9' {
			return nil, nil
		}
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	r, err := s.readDevice(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Revoked || float64(r.ExpiresAt) <= nowSec(s) {
		return nil, nil
	}
	if !hmac.Equal([]byte(mac), []byte(s.mac(id, r.Epoch))) {
		return nil, nil
	}
	d := r.Device
	return &d, nil
}

// newLinkScript creates a link key with its fields and its TTL in one step, so a link can never exist without a TTL.
// It writes nothing when the device is missing.
var newLinkScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 0 then return 0 end
redis.call('HSET', KEYS[1], 'device_id', ARGV[1], 'expires', ARGV[2], 'max_uses', ARGV[3], 'uses', '0')
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 1`)

func (s *RedisStore) NewLink(deviceID int64, ttl time.Duration, maxUses int) (string, error) {
	token := base64.RawURLEncoding.EncodeToString(randBytes(16))
	expires := int64(nowSec(s) + ttl.Seconds())
	ctx, cancel := opCtx()
	defer cancel()
	res, err := newLinkScript.Eval(ctx, s.rdb, []string{s.tokenKey(hashToken(token)), s.deviceKey(deviceID)},
		strconv.FormatInt(deviceID, 10), strconv.FormatInt(expires, 10), strconv.Itoa(maxUses),
		strconv.FormatInt((ttl+linkTTLMargin).Milliseconds(), 10)).Int()
	if err != nil {
		return "", s.wrap("creating link in redis", err)
	}
	if res == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// spendScript spends one use of a link and marks the device as enrolled, in one step. It writes nothing unless the
// link key and the device exist, the device is not revoked or expired, and the link has a use left and has not
// expired: a bare HINCRBY on an expired link would create a fresh hash with no TTL and revive it, and a write to a
// deleted device would leave a stray half-empty record. KEYS: link, device. ARGV: now (seconds), now as an integer.
var spendScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 or redis.call('EXISTS', KEYS[2]) == 0 then return 0 end
if redis.call('HGET', KEYS[2], 'revoked') == '1' then return 0 end
local dexp = tonumber(redis.call('HGET', KEYS[2], 'expires_at'))
if dexp == nil or dexp <= tonumber(ARGV[1]) then return 0 end
local uses = tonumber(redis.call('HGET', KEYS[1], 'uses'))
local max = tonumber(redis.call('HGET', KEYS[1], 'max_uses'))
local exp = tonumber(redis.call('HGET', KEYS[1], 'expires'))
if uses == nil or max == nil or exp == nil then return 0 end
if uses >= max or tonumber(ARGV[1]) >= exp then return 0 end
redis.call('HINCRBY', KEYS[1], 'uses', 1)
redis.call('HSET', KEYS[2], 'enrolled_at', ARGV[2])
redis.call('HINCRBY', KEYS[2], 'enrol_count', 1)
return 1`)

func (s *RedisStore) ConsumeToken(token string) (*Device, error) {
	ctx, cancel := opCtx()
	defer cancel()
	now := nowSec(s)
	tk := s.tokenKey(hashToken(token))
	h, err := s.rdb.HGetAll(ctx, tk).Result()
	if err != nil {
		return nil, s.wrap("reading link from redis", err)
	}
	if len(h) == 0 {
		return nil, nil
	}
	devID, e1 := strconv.ParseInt(h["device_id"], 10, 64)
	uses, e2 := strconv.ParseInt(h["uses"], 10, 64)
	maxUses, e3 := strconv.ParseInt(h["max_uses"], 10, 64)
	exp, e4 := strconv.ParseInt(h["expires"], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return nil, errors.New("corrupt link record")
	}
	if uses >= maxUses || float64(exp) <= now {
		return nil, nil
	}
	r, err := s.readDevice(ctx, devID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The device is checked before the spend so a revoked device never burns a use, like the SQL UPDATE's subquery.
	// A revoke landing between this check and the spend lets that one click through; the credential set is revoked
	// regardless, so the click gets a profile that no longer connects.
	if r.Revoked || float64(r.ExpiresAt) <= now {
		return nil, nil
	}
	ok, err := spendScript.Eval(ctx, s.rdb, []string{tk, s.deviceKey(devID)}, fnum(now), strconv.FormatInt(int64(now), 10)).Int()
	if err != nil {
		return nil, s.wrap("spending link in redis", err)
	}
	if ok != 1 {
		return nil, nil
	}
	d := r.Device
	d.EnrolledAt, d.Enrolments = int64(now), d.Enrolments+1
	return &d, nil
}

// touchScript moves a device's last_active time forward, only for an existing device.
var touchScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
local cur = tonumber(redis.call('HGET', KEYS[1], 'last_active')) or 0
if cur < tonumber(ARGV[1]) then redis.call('HSET', KEYS[1], 'last_active', ARGV[1]) end
return 1`)

// TouchActive records that the device's traffic grew at the given time. It never creates a record.
func (s *RedisStore) TouchActive(deviceID, at int64) error {
	ctx, cancel := opCtx()
	defer cancel()
	if err := touchScript.Eval(ctx, s.rdb, []string{s.deviceKey(deviceID)}, strconv.FormatInt(at, 10)).Err(); err != nil {
		return s.wrap("recording activity in redis", err)
	}
	return nil
}

func (s *RedisStore) approvalScope(name string) string { return "a:" + name }

type approvalRec struct {
	Fingerprint string `json:"fingerprint"`
	ApprovedAt  int64  `json:"approved_at"`
	NodeID      uint64 `json:"node_id"`
	MachineKey  string `json:"machine_key"`
}

func (s *RedisStore) ApprovalRows() (map[string]ApprovalRow, error) {
	ctx, cancel := opCtx()
	defer cancel()
	h, err := s.rdb.HGetAll(ctx, s.approvalsKey()).Result()
	if err != nil {
		return nil, s.wrap("reading approvals from redis", err)
	}
	out := map[string]ApprovalRow{}
	for name, raw := range h {
		pt, err := s.cipher.Open(s.prefix, s.approvalScope(name), []byte(raw))
		if err != nil {
			return nil, err
		}
		var a approvalRec
		if err := json.Unmarshal(pt, &a); err != nil {
			return nil, errors.New("corrupt approval record")
		}
		out[name] = ApprovalRow{Fingerprint: a.Fingerprint, NodeID: a.NodeID, MachineKey: a.MachineKey}
	}
	return out, nil
}

func (s *RedisStore) Approvals() (map[string]string, error) {
	rows, err := s.ApprovalRows()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n, r := range rows {
		out[n] = r.Fingerprint
	}
	return out, nil
}

func (s *RedisStore) Approve(name, fingerprint string) error {
	return s.ApproveNode(name, fingerprint, 0, "")
}

func (s *RedisStore) ApproveNode(name, fingerprint string, nodeID uint64, machineKey string) error {
	return s.putApproval(name, approvalRec{Fingerprint: fingerprint, ApprovedAt: s.clock().Unix(), NodeID: nodeID, MachineKey: machineKey})
}

func (s *RedisStore) putApproval(name string, a approvalRec) error {
	pt, err := json.Marshal(a)
	if err != nil {
		return err
	}
	sealed, err := s.cipher.Seal(s.prefix, s.approvalScope(name), pt)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if err := s.rdb.HSet(ctx, s.approvalsKey(), name, sealed).Err(); err != nil {
		return s.wrap("writing approval to redis", err)
	}
	return nil
}

func (s *RedisStore) Unapprove(name string) error {
	ctx, cancel := opCtx()
	defer cancel()
	if err := s.rdb.HDel(ctx, s.approvalsKey(), name).Err(); err != nil {
		return s.wrap("deleting approval in redis", err)
	}
	return nil
}
