package enroll

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteSQLiteRoundTripFromPythonFixture(t *testing.T) {
	x := loadExpected(t)
	src, dst, _ := importFixture(t, "python_enroll.db", x)
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	snap, err := dst.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rolled-back.db")
	if err := WriteSQLite(snap, path, []byte(x.Key)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v, %v", fi, err)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + ext); err == nil {
			t.Fatalf("a %s file was left behind: the result must be one self-contained file", ext)
		}
	}
	// The previous release opens it as it is, and serves exactly what the fixture served.
	s, err := OpenStore(path, []byte(x.Key), func() time.Time { return time.Unix(x.Now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkSet(t, s, x.Modern)
	n, _, err := s.CreateDevice("after", "o", "c", x.Now+100)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != 4 {
		t.Fatalf("next id = %d, want 4 (3 devices in the fixture)", n.ID)
	}
}

func TestWriteSQLiteRefusesAnExistingFileAndCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "there.db")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSQLite(Snapshot{}, existing, testKey); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep me" {
		t.Fatal("an existing file was modified")
	}
	// A snapshot that cannot be written (duplicate ids) leaves no partial database behind.
	bad := Snapshot{Devices: []SnapDevice{
		{ID: 1, Name: "a", UUIDs: map[string]string{"auto": "u1"}},
		{ID: 1, Name: "b", UUIDs: map[string]string{"auto": "u2"}},
	}}
	p := filepath.Join(dir, "bad.db")
	if err := WriteSQLite(bad, p, testKey); err == nil {
		t.Fatal("duplicate ids were accepted")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("a partial database was left behind")
	}
}

func TestWriteSQLiteKeepsTheIdCounterAheadOfTheNewestDevice(t *testing.T) {
	for name, devices := range map[string][]SnapDevice{
		"with devices": {{ID: 1, Name: "a", UUIDs: map[string]string{"auto": "u"}}, {ID: 2, Name: "b", UUIDs: map[string]string{"auto": "v"}}},
		"no devices":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.db")
			if err := WriteSQLite(Snapshot{Seq: 10, Devices: devices}, p, testKey); err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(p, testKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			n, _, err := s.CreateDevice("new", "o", "c", time.Now().Unix()+100)
			if err != nil || n.ID != 11 {
				t.Fatalf("new id = %d, %v; ids up to 10 were issued before", n.ID, err)
			}
		})
	}
}

func TestWriteSQLiteLinksAndApprovals(t *testing.T) {
	snap := Snapshot{
		Seq:       1,
		Devices:   []SnapDevice{{ID: 1, Name: "a", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u"}}},
		Links:     []SnapLink{{Hash: hashToken("tok"), DeviceID: 1, Expires: 4_000_000_000, MaxUses: 1}},
		Approvals: []SnapApproval{{Name: "ing", Fingerprint: "fp", ApprovedAt: 5, NodeID: 7, MachineKey: "mkey"}},
	}
	p := filepath.Join(t.TempDir(), "c.db")
	if err := WriteSQLite(snap, p, testKey); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(p, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.ConsumeToken("tok")
	mustDevice(t, d, err, "restored link")
	d, err = s.ConsumeToken("tok")
	mustNil(t, d, err, "restored link, second use")
	rows, err := s.ApprovalRows()
	if err != nil || rows["ing"] != (ApprovalRow{Fingerprint: "fp", NodeID: 7, MachineKey: "mkey"}) {
		t.Fatalf("approvals = %+v, %v", rows, err)
	}
}

func TestJSONExportRestoresIntoAFreshRedis(t *testing.T) {
	x := loadExpected(t)
	src, dst, _ := importFixture(t, "python_enroll.db", x)
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	snap, _ := dst.Snapshot()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSnapshot(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := newTestRedisStore(t, &fakeClock{now: time.Unix(x.Now, 0)}, testCipher(t))
	fresh.key = []byte(x.Key)
	if _, _, _, err := ImportSnapshot(parsed, fresh, false); err != nil {
		t.Fatal(err)
	}
	checkSet(t, fresh, x.Modern)
	if _, _, _, err := ImportSnapshot(parsed, fresh, false); err == nil {
		t.Fatal("a second restore into a populated store must be refused")
	}
}

func TestParseSnapshotRefusesBadInputWithoutEchoingIt(t *testing.T) {
	for name, in := range map[string]string{
		"not json":         `SECRET-VALUE`,
		"unknown field":    `{"seq":1,"devices":[],"approvals":[],"extra":"SECRET-VALUE"}`,
		"trailing data":    `{"seq":1,"devices":[],"approvals":[]} SECRET-VALUE`,
		"duplicate id":     `{"seq":2,"devices":[{"id":1,"uuids":{"auto":"u"}},{"id":1,"uuids":{"auto":"v"}}],"approvals":[]}`,
		"zero id":          `{"seq":0,"devices":[{"id":0,"uuids":{"auto":"u"}}],"approvals":[]}`,
		"unknown mode":     `{"seq":1,"devices":[{"id":1,"uuids":{"SECRET-VALUE":"u"}}],"approvals":[]}`,
		"empty uuid":       `{"seq":1,"devices":[{"id":1,"uuids":{"auto":""}}],"approvals":[]}`,
		"no uuids":         `{"seq":1,"devices":[{"id":1,"uuids":{}}],"approvals":[]}`,
		"link no device":   `{"seq":0,"devices":[],"links":[{"hash":"h","device_id":9,"max_uses":1}],"approvals":[]}`,
		"unnamed approval": `{"seq":0,"devices":[],"approvals":[{"fingerprint":"f"}]}`,
		"negative seq":     `{"seq":-1,"devices":[],"approvals":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSnapshot(strings.NewReader(in))
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "SECRET-VALUE") {
				t.Fatalf("error echoes the input: %v", err)
			}
		})
	}
	if _, err := ParseSnapshot(strings.NewReader(`{"seq":1,"devices":[{"id":1,"uuids":{"auto":"u"}}],"approvals":[]}`)); err != nil {
		t.Fatalf("a valid snapshot was refused: %v", err)
	}
}
