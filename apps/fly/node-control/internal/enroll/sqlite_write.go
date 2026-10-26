package enroll

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// WriteSQLite writes a snapshot as a new enrolment SQLite database that the SQLite Store (and so the previous release)
// opens as it is: same schema, same ids, epochs, UUIDs and approvals, and the AUTOINCREMENT counter raised to
// the snapshot's counter so ids are never reused. It refuses an existing path and creates the file mode 0600. The file
// holds device names, owners and credentials: treat it as a secret. secretKey is ENROLL_SECRET_KEY (it only has to be
// valid; it is not stored in the file).
func WriteSQLite(snap Snapshot, path string, secretKey []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite it", path)
		}
		return err
	}
	f.Close()
	ok := false
	defer func() {
		if !ok {
			os.Remove(path) // never leave a half-written database behind
			os.Remove(path + "-wal")
			os.Remove(path + "-shm")
		}
	}()
	s, err := OpenStore(path, secretKey, nil)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	maxID := snap.Seq
	for _, d := range snap.Devices {
		revoked := 0
		if d.Revoked {
			revoked = 1
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO devices (id, name, owner, client, expires_at, revoked, secret_epoch) VALUES (?,?,?,?,?,?,?)",
			d.ID, d.Name, d.Owner, d.Client, d.ExpiresAt, revoked, d.Epoch); err != nil {
			return fmt.Errorf("writing device %d: %w", d.ID, err)
		}
		for mode, u := range d.UUIDs {
			if _, err := tx.ExecContext(ctx, "INSERT INTO device_uuids (device_id, mode, uuid) VALUES (?,?,?)", d.ID, mode, u); err != nil {
				return fmt.Errorf("writing device %d: %w", d.ID, err)
			}
		}
		if d.ID > maxID {
			maxID = d.ID
		}
	}
	for _, l := range snap.Links {
		if _, err := tx.ExecContext(ctx, "INSERT INTO tokens (device_id, token_hash, expires, max_uses, uses) VALUES (?,?,?,?,?)",
			l.DeviceID, l.Hash, l.Expires, l.MaxUses, l.Uses); err != nil {
			return fmt.Errorf("writing a link: %w", err)
		}
	}
	for _, a := range snap.Approvals {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO ingress_approvals (name, fingerprint, approved_at, node_id, machine_key) VALUES (?,?,?,?,?)",
			a.Name, a.Fingerprint, a.ApprovedAt, int64(a.NodeID), a.MachineKey); err != nil {
			return fmt.Errorf("writing an approval: %w", err)
		}
	}
	// AUTOINCREMENT keeps its counter in sqlite_sequence: raise it so a deleted id is never handed out again.
	res, err := tx.ExecContext(ctx, "UPDATE sqlite_sequence SET seq=? WHERE name='devices'", maxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO sqlite_sequence (name, seq) VALUES ('devices', ?)", maxID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Fold the WAL into the main file so the result is one self-contained file.
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	ok = true
	return nil
}
