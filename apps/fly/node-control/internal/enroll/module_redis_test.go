package enroll

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/core"
)

func redisModEnv(t *testing.T, mr *miniredis.Miniredis) *modEnv {
	t.Helper()
	x := loadExpected(t)
	e := newModEnv(t, "http://agent.test")
	delete(e.m, "ENROLL_DB")
	e.m["ENROLL_SECRET_KEY"] = x.Key
	e.m["ENROLL_REDIS"] = "1"
	e.m["REDIS_URL"] = "redis://:s3cretpw@" + mr.Addr()
	e.m["NODE_CONTROL_STATE_KEY"] = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	return e
}

func listBody(t *testing.T, m *Module, env core.Env) (*core.Routes, string) {
	t.Helper()
	r := routesFor(env)
	if err := m.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}
	rec := httptest.NewRecorder()
	r.Admin.ServeHTTP(rec, httptest.NewRequest("GET", "/enroll/devices", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /enroll/devices = %d", rec.Code)
	}
	return r, rec.Body.String()
}

func TestModuleRedisImportsOnceAndServes(t *testing.T) {
	mr := miniredis.RunT(t)
	e := redisModEnv(t, mr)
	src := copyFixture(t, "python_enroll.db")
	e.m["ENROLL_IMPORT_FROM"] = src

	m := New(e.env()).(*Module)
	_, body := listBody(t, m, e.env())
	if !strings.Contains(body, "device-a") || !strings.Contains(body, "device-b") {
		t.Fatalf("imported devices missing from the admin list: %s", body)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A second start (a rebuilt machine, the file still there) must not import again or overwrite what Redis holds.
	m2 := New(e.env()).(*Module)
	_, body2 := listBody(t, m2, e.env())
	if body2 != body {
		t.Fatalf("second start changed the list:\n%s\nvs\n%s", body, body2)
	}
	m2.Stop(context.Background())
}

func TestModuleRedisFreshStartWithoutSourceFile(t *testing.T) {
	mr := miniredis.RunT(t)
	e := redisModEnv(t, mr)
	e.m["ENROLL_IMPORT_FROM"] = e.dir + "/does-not-exist.db"
	m := New(e.env()).(*Module)
	_, body := listBody(t, m, e.env())
	if strings.Contains(body, "device-a") {
		t.Fatalf("unexpected devices: %s", body)
	}
	m.Stop(context.Background())
}

func TestModuleRedisConfigurationErrorsAreClearAndSecretFree(t *testing.T) {
	mr := miniredis.RunT(t)
	e := redisModEnv(t, mr)
	for _, tc := range []struct {
		drop []string
		want string
	}{
		{[]string{"REDIS_URL"}, "REDIS_URL"},
		{[]string{"NODE_CONTROL_STATE_KEY"}, "NODE_CONTROL_STATE_KEY"},
	} {
		env := e.without(tc.drop...)
		err := New(env).Register(routesFor(env))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("dropping %v: %v", tc.drop, err)
		}
		if strings.Contains(err.Error(), "s3cretpw") {
			t.Fatalf("secret in error: %v", err)
		}
	}
	// An unreachable Redis fails at start and never prints the URL or password.
	mr.Close()
	err := New(e.env()).Register(routesFor(e.env()))
	if err == nil || strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("unreachable redis: %v", err)
	}
}

func TestModuleRedisDoesNotTouchTheSourceFile(t *testing.T) {
	mr := miniredis.RunT(t)
	e := redisModEnv(t, mr)
	src := copyFixture(t, "python_enroll.db")
	before, _ := os.ReadFile(src)
	e.m["ENROLL_IMPORT_FROM"] = src
	m := New(e.env()).(*Module)
	listBody(t, m, e.env())
	m.Stop(context.Background())
	after, _ := os.ReadFile(src)
	if string(before) != string(after) {
		t.Fatal("the source database changed during the import")
	}
}
