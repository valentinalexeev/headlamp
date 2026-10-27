package funnel

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Errors returned by the store. Callers test them with errors.Is.
var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
	ErrBusy     = errors.New("a change is already in progress")
	ErrInUse    = errors.New("still in use")
	ErrInvalid  = errors.New("invalid input")
)

// Namespace states.
const (
	StatePending = "pending"
	StateActive  = "active"
	StateFailed  = "failed"
)

const (
	maxRanges   = 32
	maxNote     = 200
	maxReason   = 500
	maxDetail   = 500
	maxShort    = 100 // actor, action, node id
	maxTarget   = 253
	maxStateLen = 1 << 20
	auditKeep   = 1000
)

// Range is an inclusive port range.
type Range struct{ Lo, Hi int }

// Namespace is a named group of ports. While a port change awaits confirmation, Ports still holds
// the confirmed set and PendingPorts the proposed one.
type Namespace struct {
	Name         string
	Ports        []Range
	PendingPorts []Range
	State, Note  string
	ChangeID     int64
}

// ManualRoute points a service of a namespace at a fixed target.
type ManualRoute struct {
	NS, Service, Target string
	Port                int
	Scheme              string // "http" or "https"; empty means "http"
	InsecureSkipVerify  bool
}

// Block withdraws a service of a namespace.
type Block struct {
	NS, Service, Reason, Actor string
	At                         time.Time
}

// AuditEntry is one audit row: who did what, with a short detail.
type AuditEntry struct {
	ID                    int64
	At                    time.Time
	Actor, Action, Detail string
}

// Store keeps the funnel state. It is small so the backing database can change.
type Store interface {
	CreateNamespace(name string, ports []Range, note string, changeID int64) error
	SetPendingPorts(name string, ports []Range, changeID int64) error
	Namespaces() ([]Namespace, error)
	MarkActive(name string) error
	MarkFailed(name string) error
	DeleteNamespace(name string) error
	PutManual(r ManualRoute) error
	DeleteManual(ns, service string) error
	ManualRoutes() ([]ManualRoute, error)
	Block(ns, service, reason, actor string) error
	Unblock(ns, service string) error
	Blocks() ([]Block, error)
	SaveState(nodeID string, body []byte, receivedAt time.Time) error
	LatestState() (body []byte, receivedAt time.Time, ok bool, err error)
	Audit(actor, action, detail string) error
	AuditLog(limit int) ([]AuditEntry, error)
	Close() error
}

const schema = `
CREATE TABLE IF NOT EXISTS namespace (
    name TEXT PRIMARY KEY,
    ports TEXT NOT NULL,
    pending_ports TEXT,
    state TEXT NOT NULL,
    note TEXT NOT NULL,
    change_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS manual_route (
    ns TEXT NOT NULL,
    service TEXT NOT NULL,
    target TEXT NOT NULL,
    port INTEGER NOT NULL,
    scheme TEXT NOT NULL,
    insecure INTEGER NOT NULL,
    PRIMARY KEY (ns, service)
);
CREATE TABLE IF NOT EXISTS block (
    ns TEXT NOT NULL,
    service TEXT NOT NULL,
    reason TEXT NOT NULL,
    actor TEXT NOT NULL,
    ts INTEGER NOT NULL,
    PRIMARY KEY (ns, service)
);
CREATE TABLE IF NOT EXISTS ingress_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    node_id TEXT NOT NULL,
    body BLOB NOT NULL,
    received INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    detail TEXT NOT NULL
);
`

type sqliteStore struct {
	db    *sql.DB
	clock func() time.Time
}

// OpenSQLite opens (creating when needed) the database at path. A file that is not a database is
// an error; it is never deleted or recreated. A nil clock means time.Now. It uses one connection
// and no WAL to keep memory low.
func OpenSQLite(path string, clock func() time.Time) (Store, error) {
	if clock == nil {
		clock = time.Now
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(30000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open funnel db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open funnel db: %w", err)
	}
	return &sqliteStore{db: db, clock: clock}, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// inTx runs f in an immediate transaction and commits when f succeeds.
func (s *sqliteStore) inTx(f func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// --- validation ---

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

func isSpaceOrControl(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

func checkName(what, v string) error {
	if !nameRE.MatchString(v) {
		return invalid("%s must be 1-63 characters: lower-case letters, digits and '-', not starting or ending with '-'", what)
	}
	return nil
}

// checkText bounds a free-text field and refuses control characters.
func checkText(what, v string, max int) error {
	if utf8.RuneCountInString(v) > max {
		return invalid("%s is longer than %d characters", what, max)
	}
	if strings.ContainsFunc(v, unicode.IsControl) {
		return invalid("%s contains control characters", what)
	}
	return nil
}

// normalizeRanges validates ports and returns them sorted with overlapping or adjacent ranges merged.
func normalizeRanges(in []Range) ([]Range, error) {
	if len(in) == 0 || len(in) > maxRanges {
		return nil, invalid("a namespace needs 1 to %d port ranges", maxRanges)
	}
	rs := slices.Clone(in)
	for _, r := range rs {
		if r.Lo < 1 || r.Lo > r.Hi || r.Hi > 65535 {
			return nil, invalid("port range must satisfy 1 <= low <= high <= 65535")
		}
	}
	slices.SortFunc(rs, func(a, b Range) int { return a.Lo - b.Lo })
	out := rs[:1]
	for _, r := range rs[1:] {
		if last := &out[len(out)-1]; r.Lo <= last.Hi+1 {
			last.Hi = max(last.Hi, r.Hi)
		} else {
			out = append(out, r)
		}
	}
	return out, nil
}

func encodeRanges(rs []Range) string {
	b, _ := json.Marshal(rs)
	return string(b)
}

func decodeRanges(s sql.NullString) ([]Range, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	var rs []Range
	if err := json.Unmarshal([]byte(s.String), &rs); err != nil {
		return nil, fmt.Errorf("stored port ranges: %w", err)
	}
	return rs, nil
}

// --- namespaces ---

func (s *sqliteStore) CreateNamespace(name string, ports []Range, note string, changeID int64) error {
	if err := checkName("namespace name", name); err != nil {
		return err
	}
	rs, err := normalizeRanges(ports)
	if err != nil {
		return err
	}
	if err := checkText("note", note, maxNote); err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		var state string
		err := tx.QueryRow("SELECT state FROM namespace WHERE name = ?", name).Scan(&state)
		switch {
		case err == nil && state != StateFailed:
			return fmt.Errorf("namespace %q: %w", name, ErrExists)
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.Exec(`INSERT OR REPLACE INTO namespace (name, ports, pending_ports, state, note, change_id)
			VALUES (?, ?, NULL, ?, ?, ?)`, name, encodeRanges(rs), StatePending, note, changeID)
		return err
	})
}

func (s *sqliteStore) SetPendingPorts(name string, ports []Range, changeID int64) error {
	rs, err := normalizeRanges(ports)
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		state, pending, err := nsState(tx, name)
		if err != nil {
			return err
		}
		if state != StateActive || pending.Valid {
			return fmt.Errorf("namespace %q: %w", name, ErrBusy)
		}
		_, err = tx.Exec("UPDATE namespace SET pending_ports = ?, change_id = ? WHERE name = ?",
			encodeRanges(rs), changeID, name)
		return err
	})
}

// nsState returns the state and the pending ports of a namespace, or ErrNotFound.
func nsState(tx *sql.Tx, name string) (string, sql.NullString, error) {
	var state string
	var pending sql.NullString
	err := tx.QueryRow("SELECT state, pending_ports FROM namespace WHERE name = ?", name).Scan(&state, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return "", pending, fmt.Errorf("namespace %q: %w", name, ErrNotFound)
	}
	return state, pending, err
}

func (s *sqliteStore) Namespaces() ([]Namespace, error) {
	rows, err := s.db.Query(`SELECT name, ports, pending_ports, state, note, change_id
		FROM namespace ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Namespace
	for rows.Next() {
		var n Namespace
		var ports string
		var pending sql.NullString
		if err := rows.Scan(&n.Name, &ports, &pending, &n.State, &n.Note, &n.ChangeID); err != nil {
			return nil, err
		}
		if n.Ports, err = decodeRanges(sql.NullString{String: ports, Valid: true}); err != nil {
			return nil, err
		}
		if n.PendingPorts, err = decodeRanges(pending); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *sqliteStore) MarkActive(name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		state, pending, err := nsState(tx, name)
		if err != nil {
			return err
		}
		switch {
		case state == StatePending:
			_, err = tx.Exec("UPDATE namespace SET state = ? WHERE name = ?", StateActive, name)
		case state == StateActive && pending.Valid:
			_, err = tx.Exec("UPDATE namespace SET ports = pending_ports, pending_ports = NULL WHERE name = ?", name)
		}
		return err
	})
}

func (s *sqliteStore) MarkFailed(name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		state, pending, err := nsState(tx, name)
		if err != nil {
			return err
		}
		switch {
		case state == StatePending:
			_, err = tx.Exec("UPDATE namespace SET state = ? WHERE name = ?", StateFailed, name)
		case state == StateActive && pending.Valid:
			_, err = tx.Exec("UPDATE namespace SET pending_ports = NULL WHERE name = ?", name)
		}
		return err
	})
}

func (s *sqliteStore) DeleteNamespace(name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		if _, _, err := nsState(tx, name); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM manual_route WHERE ns = ?", name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("namespace %q has manual routes: %w", name, ErrInUse)
		}
		_, err := tx.Exec("DELETE FROM namespace WHERE name = ?", name)
		return err
	})
}

// --- manual routes ---

func (s *sqliteStore) PutManual(r ManualRoute) error {
	if err := checkName("namespace name", r.NS); err != nil {
		return err
	}
	if err := checkName("service name", r.Service); err != nil {
		return err
	}
	if r.Target == "" || len(r.Target) > maxTarget || strings.ContainsFunc(r.Target, isSpaceOrControl) {
		return invalid("target must be 1-%d characters without spaces", maxTarget)
	}
	if r.Port < 1 || r.Port > 65535 {
		return invalid("port must be 1-65535")
	}
	switch r.Scheme {
	case "":
		r.Scheme = "http"
	case "http", "https":
	default:
		return invalid("scheme must be http or https")
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO manual_route (ns, service, target, port, scheme, insecure)
		VALUES (?, ?, ?, ?, ?, ?)`, r.NS, r.Service, r.Target, r.Port, r.Scheme, r.InsecureSkipVerify)
	return err
}

func (s *sqliteStore) DeleteManual(ns, service string) error {
	return s.deleteRow("manual route", "DELETE FROM manual_route WHERE ns = ? AND service = ?", ns, service)
}

func (s *sqliteStore) deleteRow(what, query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

func (s *sqliteStore) ManualRoutes() ([]ManualRoute, error) {
	rows, err := s.db.Query("SELECT ns, service, target, port, scheme, insecure FROM manual_route ORDER BY ns, service")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManualRoute
	for rows.Next() {
		var r ManualRoute
		if err := rows.Scan(&r.NS, &r.Service, &r.Target, &r.Port, &r.Scheme, &r.InsecureSkipVerify); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- blocks ---

func (s *sqliteStore) Block(ns, service, reason, actor string) error {
	if err := checkName("namespace name", ns); err != nil {
		return err
	}
	if err := checkName("service name", service); err != nil {
		return err
	}
	if err := checkText("reason", reason, maxReason); err != nil {
		return err
	}
	if err := checkText("actor", actor, maxShort); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO block (ns, service, reason, actor, ts) VALUES (?, ?, ?, ?, ?)`,
		ns, service, reason, actor, s.clock().Unix())
	return err
}

func (s *sqliteStore) Unblock(ns, service string) error {
	return s.deleteRow("block", "DELETE FROM block WHERE ns = ? AND service = ?", ns, service)
}

func (s *sqliteStore) Blocks() ([]Block, error) {
	rows, err := s.db.Query("SELECT ns, service, reason, actor, ts FROM block ORDER BY ns, service")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		var b Block
		var ts int64
		if err := rows.Scan(&b.NS, &b.Service, &b.Reason, &b.Actor, &ts); err != nil {
			return nil, err
		}
		b.At = time.Unix(ts, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- ingress state ---

func (s *sqliteStore) SaveState(nodeID string, body []byte, receivedAt time.Time) error {
	if nodeID == "" {
		return invalid("node id is empty")
	}
	if err := checkText("node id", nodeID, maxShort); err != nil {
		return err
	}
	if len(body) > maxStateLen {
		return invalid("ingress state is larger than %d bytes", maxStateLen)
	}
	_, err := s.db.Exec("INSERT OR REPLACE INTO ingress_state (id, node_id, body, received) VALUES (1, ?, ?, ?)",
		nodeID, body, receivedAt.Unix())
	return err
}

func (s *sqliteStore) LatestState() ([]byte, time.Time, bool, error) {
	var body []byte
	var ts int64
	err := s.db.QueryRow("SELECT body, received FROM ingress_state WHERE id = 1").Scan(&body, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return body, time.Unix(ts, 0), true, nil
}

// --- audit ---

var newlines = strings.NewReplacer("\r", " ", "\n", " ")

func (s *sqliteStore) Audit(actor, action, detail string) error {
	detail = newlines.Replace(detail)
	for _, f := range []struct {
		what, v string
		max     int
	}{{"actor", actor, maxShort}, {"action", action, maxShort}, {"detail", detail, maxDetail}} {
		if err := checkText(f.what, f.v, f.max); err != nil {
			return err
		}
	}
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec("INSERT INTO audit (ts, actor, action, detail) VALUES (?, ?, ?, ?)",
			s.clock().Unix(), actor, action, detail)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec("DELETE FROM audit WHERE id <= ?", id-auditKeep)
		return err
	})
}

func (s *sqliteStore) AuditLog(limit int) ([]AuditEntry, error) {
	limit = min(max(limit, 1), auditKeep)
	rows, err := s.db.Query("SELECT id, ts, actor, action, detail FROM audit ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
