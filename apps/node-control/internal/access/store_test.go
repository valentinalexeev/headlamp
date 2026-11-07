package access

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
)

func stores(t *testing.T) map[string]Store {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sq.Close() })
	mr := miniredis.RunT(t)
	rs, err := NewRedisStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "nc:admin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	return map[string]Store{"sqlite": sq, "redis": rs}
}

// The contract every admin store keeps: empty at first, compare-and-set on the version, one audit entry per change,
// newest first.
func TestStoreContract(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			cfg, v, err := s.Load()
			if err != nil || v != 0 || len(cfg.Grants) != 0 || len(cfg.Roles) != 0 {
				t.Fatalf("empty store: %+v %d %v", cfg, v, err)
			}
			want := authz.Config{Roles: map[string][]string{"ops": {"nodes.operate"}}, Grants: []authz.Grant{{Subject: "bob", Roles: []string{"ops"}}}}
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			v1, err := s.Save(want, 0, Entry{At: at, Actor: "owner", Action: "grant", Target: "bob", Detail: "ops"})
			if err != nil || v1 != 1 {
				t.Fatalf("save: %d %v", v1, err)
			}
			if _, err := s.Save(authz.Config{}, 0, Entry{At: at, Actor: "x", Action: "import"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("a stale version must conflict: %v", err)
			}
			got, v, err := s.Load()
			if err != nil || v != 1 || fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("load: %+v %d %v", got, v, err)
			}
			if cur, err := s.Version(); err != nil || cur != 1 {
				t.Fatalf("version: %d %v", cur, err)
			}
			if _, err := s.Save(authz.Config{}, 1, Entry{At: at.Add(time.Second), Actor: "owner", Action: "revoke", Target: "bob"}); err != nil {
				t.Fatal(err)
			}
			log, err := s.Audit(10)
			if err != nil || len(log) != 2 || log[0].Action != "revoke" || log[1].Action != "grant" || log[1].Detail != "ops" || !log[1].At.Equal(at) {
				t.Fatalf("audit: %+v %v", log, err)
			}
			if one, _ := s.Audit(1); len(one) != 1 {
				t.Fatalf("limit: %d", len(one))
			}
		})
	}
}

func TestStoreAuditIsCapped(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			v := int64(0)
			for i := 0; i < auditKeep+5; i++ {
				var err error
				if v, err = s.Save(authz.Config{}, v, Entry{At: time.Unix(int64(i), 0), Actor: "a", Action: fmt.Sprint(i)}); err != nil {
					t.Fatal(err)
				}
			}
			log, _ := s.Audit(auditKeep * 2)
			if len(log) != auditKeep || log[0].Action != fmt.Sprint(auditKeep+4) {
				t.Fatalf("kept %d, newest %q", len(log), log[0].Action)
			}
		})
	}
}
