package enroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Snapshot is the whole enrolment state in a store-neutral form, used to move it between stores and to back it up.
// Ids, epochs, UUIDs and link hashes are carried verbatim, so subscription secrets (derived from id and epoch) and
// one-time links keep working after a move.
type Snapshot struct {
	// Seq is the highest device id ever issued (SQLite's AUTOINCREMENT counter), so ids are never reused.
	Seq       int64          `json:"seq"`
	Devices   []SnapDevice   `json:"devices"`
	Links     []SnapLink     `json:"links,omitempty"` // not exported from Redis: its links cannot be listed without a scan
	Approvals []SnapApproval `json:"approvals"`
}

type SnapDevice struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Owner     string            `json:"owner"`
	Client    string            `json:"client"`
	ExpiresAt int64             `json:"expires_at"`
	Revoked   bool              `json:"revoked"`
	Epoch     int64             `json:"epoch"`
	UUIDs     map[string]string `json:"uuids"`
}

type SnapLink struct {
	Hash     string `json:"hash"` // SHA-256 hex of the token; the token itself is never stored
	DeviceID int64  `json:"device_id"`
	Expires  int64  `json:"expires"`
	MaxUses  int64  `json:"max_uses"`
	Uses     int64  `json:"uses"`
}

type SnapApproval struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	ApprovedAt  int64  `json:"approved_at"`
	NodeID      uint64 `json:"node_id"`
	MachineKey  string `json:"machine_key"`
}

// ErrPopulated means the target store already holds enrolment data and force was not set.
var ErrPopulated = errors.New("the target already holds enrolment data (use force to overwrite records with the same ids)")

func (s *Store) hasTable(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0, err
}

func (s *Store) hasColumn(ctx context.Context, table, col string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info(?) WHERE name=?", table, col).Scan(&n)
	return n > 0, err
}

// Snapshot reads the whole SQLite database.
func (s *Store) Snapshot() (Snapshot, error) {
	ctx := context.Background()
	var snap Snapshot
	// A database from before secrets were derived has no secret_epoch column: its epoch is 0.
	epochCol := "0"
	if ok, err := s.hasColumn(ctx, "devices", "secret_epoch"); err != nil {
		return snap, err
	} else if ok {
		epochCol = "secret_epoch"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, owner, client, expires_at, revoked, "+epochCol+" FROM devices ORDER BY id")
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var d SnapDevice
		var revoked int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Owner, &d.Client, &d.ExpiresAt, &revoked, &d.Epoch); err != nil {
			rows.Close()
			return snap, err
		}
		d.Revoked = revoked != 0
		d.UUIDs = map[string]string{}
		snap.Devices = append(snap.Devices, d)
		if d.ID > snap.Seq {
			snap.Seq = d.ID
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snap, err
	}
	idx := map[int64]int{}
	for i, d := range snap.Devices {
		idx[d.ID] = i
	}
	ur, err := s.db.QueryContext(ctx, "SELECT device_id, mode, uuid FROM device_uuids")
	if err != nil {
		return snap, err
	}
	for ur.Next() {
		var id int64
		var mode, u string
		if err := ur.Scan(&id, &mode, &u); err != nil {
			ur.Close()
			return snap, err
		}
		if i, ok := idx[id]; ok {
			snap.Devices[i].UUIDs[mode] = u
		}
	}
	err = ur.Err()
	ur.Close()
	if err != nil {
		return snap, err
	}
	// AUTOINCREMENT never reuses an id, even one whose row was deleted: carry its counter, not just max(id).
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name='devices'").Scan(&seq); err == nil && seq.Valid && seq.Int64 > snap.Seq {
		snap.Seq = seq.Int64
	}
	if ok, err := s.hasTable(ctx, "tokens"); err != nil {
		return snap, err
	} else if ok {
		tr, err := s.db.QueryContext(ctx, "SELECT token_hash, device_id, expires, max_uses, uses FROM tokens ORDER BY id")
		if err != nil {
			return snap, err
		}
		for tr.Next() {
			var l SnapLink
			if err := tr.Scan(&l.Hash, &l.DeviceID, &l.Expires, &l.MaxUses, &l.Uses); err != nil {
				tr.Close()
				return snap, err
			}
			snap.Links = append(snap.Links, l)
		}
		err = tr.Err()
		tr.Close()
		if err != nil {
			return snap, err
		}
	}
	// A database from before ingress approvals existed has no such table.
	if ok, err := s.hasTable(ctx, "ingress_approvals"); err != nil || !ok {
		return snap, err
	}
	ar, err := s.db.QueryContext(ctx, "SELECT name, fingerprint, approved_at, node_id, machine_key FROM ingress_approvals ORDER BY name")
	if err != nil {
		return snap, err
	}
	defer ar.Close()
	for ar.Next() {
		var a SnapApproval
		var nid int64
		if err := ar.Scan(&a.Name, &a.Fingerprint, &a.ApprovedAt, &nid, &a.MachineKey); err != nil {
			return snap, err
		}
		a.NodeID = uint64(nid)
		snap.Approvals = append(snap.Approvals, a)
	}
	return snap, ar.Err()
}

// Snapshot reads devices and approvals. One-time links are not included: Redis cannot list them without a scan, and
// they are short-lived (the operator creates a new one).
func (s *RedisStore) Snapshot() (Snapshot, error) {
	ctx, cancel := opCtx()
	defer cancel()
	var snap Snapshot
	seq, err := s.seq(ctx)
	if err != nil {
		return snap, err
	}
	snap.Seq = seq
	recs, err := s.listRecs(ctx)
	if err != nil {
		return snap, err
	}
	for _, r := range recs {
		snap.Devices = append(snap.Devices, SnapDevice{ID: r.ID, Name: r.Name, Owner: r.Owner, Client: r.Client,
			ExpiresAt: r.ExpiresAt, Revoked: r.Revoked, Epoch: r.Epoch, UUIDs: r.UUIDs})
	}
	h, err := s.rdb.HGetAll(ctx, s.approvalsKey()).Result()
	if err != nil {
		return snap, s.wrap("reading approvals from redis", err)
	}
	for name, raw := range h {
		pt, err := s.cipher.Open(s.prefix, s.approvalScope(name), []byte(raw))
		if err != nil {
			return snap, err
		}
		var a approvalRec
		if err := json.Unmarshal(pt, &a); err != nil {
			return snap, errors.New("corrupt approval record")
		}
		snap.Approvals = append(snap.Approvals, SnapApproval{Name: name, Fingerprint: a.Fingerprint, ApprovedAt: a.ApprovedAt, NodeID: a.NodeID, MachineKey: a.MachineKey})
	}
	sort.Slice(snap.Approvals, func(i, j int) bool { return snap.Approvals[i].Name < snap.Approvals[j].Name })
	return snap, nil
}

// restoreLinkScript recreates a link with the TTL it has left and the uses it has already spent, in one step.
var restoreLinkScript = redis.NewScript(`
redis.call('HSET', KEYS[1], 'device_id', ARGV[1], 'expires', ARGV[2], 'max_uses', ARGV[3], 'uses', ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 1`)

// Restore writes a snapshot into the store. The counter is written last and doubles as the "complete" marker: an
// interrupted restore leaves no counter, the store still reads as empty, and running it again rewrites everything
// (every write is idempotent). It refuses a store that already has a counter unless force is set; force overwrites
// records with the same ids and never deletes others. Links that are already expired or used up are skipped.
func (s *RedisStore) Restore(snap Snapshot, force bool) (devices, links, approvals int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*redisOpTimeout)
	defer cancel()
	has, err := s.rdb.HExists(ctx, s.metaKey(), "seq").Result()
	if err != nil {
		return 0, 0, 0, s.wrap("checking the target in redis", err)
	}
	if has && !force {
		return 0, 0, 0, ErrPopulated
	}
	maxID := snap.Seq
	for _, d := range snap.Devices {
		if d.ID <= 0 {
			return 0, 0, 0, fmt.Errorf("device with invalid id %d", d.ID)
		}
		if d.ID > maxID {
			maxID = d.ID
		}
		uu := d.UUIDs
		if uu == nil {
			uu = map[string]string{}
		}
		if err := s.putDevice(ctx, Device{ID: d.ID, Name: d.Name, Owner: d.Owner, Client: d.Client, ExpiresAt: d.ExpiresAt, Revoked: d.Revoked, UUIDs: uu}, d.Epoch); err != nil {
			return 0, 0, 0, err
		}
		devices++
	}
	for _, a := range snap.Approvals {
		if err := s.putApproval(a.Name, approvalRec{Fingerprint: a.Fingerprint, ApprovedAt: a.ApprovedAt, NodeID: a.NodeID, MachineKey: a.MachineKey}); err != nil {
			return 0, 0, 0, err
		}
		approvals++
	}
	now := nowSec(s)
	for _, l := range snap.Links {
		left := float64(l.Expires) - now
		if left <= 0 || l.Uses >= l.MaxUses {
			continue
		}
		ttl := time.Duration(left*float64(time.Second)) + linkTTLMargin
		if err := restoreLinkScript.Eval(ctx, s.rdb, []string{s.tokenKey(l.Hash)},
			strconv.FormatInt(l.DeviceID, 10), strconv.FormatInt(l.Expires, 10), strconv.FormatInt(l.MaxUses, 10),
			strconv.FormatInt(l.Uses, 10), strconv.FormatInt(ttl.Milliseconds(), 10)).Err(); err != nil {
			return 0, 0, 0, s.wrap("restoring a link in redis", err)
		}
		links++
	}
	if err := s.rdb.HSet(ctx, s.metaKey(), "seq", strconv.FormatInt(maxID, 10)).Err(); err != nil {
		return 0, 0, 0, s.wrap("writing the device counter to redis", err)
	}
	return devices, links, approvals, nil
}

// Import copies the SQLite database into Redis and verifies the result: the devices and approvals read back from
// Redis must equal the source. It never writes to the SQLite file.
func Import(src *Store, dst *RedisStore, force bool) (devices, links, approvals int, err error) {
	snap, err := src.Snapshot()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reading the source database: %w", err)
	}
	return ImportSnapshot(snap, dst, force)
}

// ImportSnapshot restores snap into dst and verifies the result by reading it back.
func ImportSnapshot(snap Snapshot, dst *RedisStore, force bool) (devices, links, approvals int, err error) {
	devices, links, approvals, err = dst.Restore(snap, force)
	if err != nil {
		return 0, 0, 0, err
	}
	got, err := dst.Snapshot()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("verifying the import: %w", err)
	}
	if err := sameState(snap, got); err != nil {
		return 0, 0, 0, fmt.Errorf("verifying the import: %w", err)
	}
	return devices, links, approvals, nil
}

// ParseSnapshot reads an exported snapshot (the JSON `enroll-export` writes) and checks its shape. Unknown fields,
// duplicate or non-positive ids, unknown modes and empty UUIDs are refused; error texts never echo values from the file.
func ParseSnapshot(r io.Reader) (Snapshot, error) {
	var snap Snapshot
	dec := json.NewDecoder(io.LimitReader(r, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return Snapshot{}, errors.New("not a valid enrolment export (JSON shape does not match)")
	}
	if dec.More() {
		return Snapshot{}, errors.New("not a valid enrolment export (trailing data)")
	}
	known := map[string]bool{}
	for _, m := range Modes {
		known[m] = true
	}
	seen := map[int64]bool{}
	for i, d := range snap.Devices {
		if d.ID <= 0 || seen[d.ID] {
			return Snapshot{}, fmt.Errorf("device entry %d has an invalid or duplicate id", i+1)
		}
		seen[d.ID] = true
		if len(d.UUIDs) == 0 {
			return Snapshot{}, fmt.Errorf("device entry %d has no UUIDs", i+1)
		}
		for m, u := range d.UUIDs {
			if !known[m] || u == "" {
				return Snapshot{}, fmt.Errorf("device entry %d has an unknown mode or an empty UUID", i+1)
			}
		}
	}
	for i, l := range snap.Links {
		if !seen[l.DeviceID] || l.Hash == "" || l.MaxUses <= 0 {
			return Snapshot{}, fmt.Errorf("link entry %d is invalid or names a missing device", i+1)
		}
	}
	for i, a := range snap.Approvals {
		if a.Name == "" {
			return Snapshot{}, fmt.Errorf("approval entry %d has no name", i+1)
		}
	}
	if snap.Seq < 0 {
		return Snapshot{}, errors.New("negative device counter")
	}
	return snap, nil
}

func sameState(want, got Snapshot) error {
	if got.Seq < want.Seq {
		return fmt.Errorf("device counter %d is below the source's %d", got.Seq, want.Seq)
	}
	wd := append([]SnapDevice(nil), want.Devices...)
	gd := append([]SnapDevice(nil), got.Devices...)
	// With force, the target may hold extra records; every source record must be there and equal.
	byID := map[int64]SnapDevice{}
	for _, d := range gd {
		byID[d.ID] = d
	}
	for _, d := range wd {
		g, ok := byID[d.ID]
		if !ok || !reflect.DeepEqual(d, g) {
			return fmt.Errorf("device %d differs after the copy", d.ID)
		}
	}
	ga := map[string]SnapApproval{}
	for _, a := range got.Approvals {
		ga[a.Name] = a
	}
	for _, a := range want.Approvals {
		if g, ok := ga[a.Name]; !ok || g != a {
			return fmt.Errorf("approval %q differs after the copy", a.Name)
		}
	}
	return nil
}
