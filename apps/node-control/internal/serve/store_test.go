package serve

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) Store {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "serve.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStoreServices(t *testing.T) {
	st := openTest(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	g := Service{ID: "grafana", Name: "grafana.svc.example.com", Namespace: "mon", Created: now,
		Keys: []Key{{ID: 7, Expires: now.Add(24 * time.Hour), Created: now}}}
	if err := st.CreateService(g); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateService(Service{ID: "other", Name: g.Name, Namespace: "x", Created: now}); !errors.Is(err, ErrExists) {
		t.Fatalf("a duplicate name: %v", err)
	}
	if err := st.CreateService(Service{ID: "grafana", Name: "z.svc.example.com", Created: now}); !errors.Is(err, ErrExists) {
		t.Fatalf("a duplicate id: %v", err)
	}
	// a key id belongs to one service only
	if err := st.CreateService(Service{ID: "k", Name: "k.svc.example.com", Created: now, Keys: []Key{{ID: 7, Created: now}}}); !errors.Is(err, ErrExists) {
		t.Fatalf("a shared key id: %v", err)
	}
	if _, err := st.Service("k"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed create must leave nothing behind")
	}

	// rotation: the new key comes first, both keys belong to the service until the old one is retired
	if err := st.AddKey("grafana", Key{ID: 9, Expires: now.Add(48 * time.Hour), Created: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Service("grafana")
	if err != nil || len(got.Keys) != 2 || got.Keys[0].ID != 9 || got.Keys[1].ID != 7 {
		t.Fatalf("after rotation: %+v %v", got, err)
	}
	if err := st.RetireKey("grafana", 7); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireKey("grafana", 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retire twice: %v", err)
	}
	if err := st.AddKey("missing", Key{ID: 11, Created: now}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a key for a missing service: %v", err)
	}

	all, err := st.Services()
	if err != nil || len(all) != 1 || len(all[0].Keys) != 1 || all[0].Keys[0].ID != 9 || !all[0].Created.Equal(now) {
		t.Fatalf("list: %+v %v", all, err)
	}
	if err := st.DeleteService("grafana"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteService("grafana"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// the keys go with the service, so the id is free again
	if err := st.CreateService(Service{ID: "n", Name: "n.svc.example.com", Created: now, Keys: []Key{{ID: 9, Created: now}}}); err != nil {
		t.Fatalf("key id after delete: %v", err)
	}
}

func TestStoreSecretsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serve.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSecret("ca"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	_ = st.PutSecret("ca", []byte("one"))
	_ = st.PutSecret("ca", []byte("two"))
	_ = st.CreateService(Service{ID: "a", Name: "a.svc.example.com", Created: time.Now()})
	st.Close()
	st, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, err := st.GetSecret("ca"); err != nil || string(v) != "two" {
		t.Fatalf("secret after reopen: %q %v", v, err)
	}
	if _, err := st.Service("a"); err != nil {
		t.Fatalf("service after reopen: %v", err)
	}
}
