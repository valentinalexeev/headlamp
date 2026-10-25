package headscale

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestListUsers(t *testing.T) {
	c, got := recorder(t, 200, `{"users":[{"id":"3","name":"alice","email":"a@example.test"},{"id":"4","name":"bob"}]}`)
	us, err := c.ListUsers(context.Background())
	if err != nil || len(us) != 2 || us[0] != (User{3, "alice"}) || us[1].ID != 4 {
		t.Fatalf("%+v %v", us, err)
	}
	if got.method != "GET" || got.path != "/api/v1/user" || got.auth != "Bearer k1" {
		t.Fatalf("%+v", got)
	}
}

func TestListUsersRejectsNonNumericID(t *testing.T) {
	c, _ := recorder(t, 200, `{"users":[{"id":"abc","name":"alice"}]}`)
	if _, err := c.ListUsers(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestCreatePreAuthKeyRequestShape(t *testing.T) {
	c, got := recorder(t, 200, `{"preAuthKey":{"id":"9","key":"hskey-new","user":{"id":"3","name":"alice"},"reusable":true,"ephemeral":false,"used":false,"expiration":"2030-01-02T03:04:05Z","aclTags":["tag:example"]}}`)
	before := time.Now()
	if _, err := c.CreatePreAuthKey(context.Background(), 3, []string{"tag:example"}, time.Hour, false, true); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if got.method != "POST" || got.path != "/api/v1/preauthkey" || got.auth != "Bearer k1" {
		t.Fatalf("%+v", got)
	}
	var body struct {
		User       string   `json:"user"`
		Reusable   bool     `json:"reusable"`
		Ephemeral  bool     `json:"ephemeral"`
		Expiration string   `json:"expiration"`
		ACLTags    []string `json:"aclTags"`
	}
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	if body.User != "3" || !body.Reusable || body.Ephemeral || len(body.ACLTags) != 1 || body.ACLTags[0] != "tag:example" {
		t.Fatalf("%+v", body)
	}
	exp, err := time.Parse(time.RFC3339, body.Expiration)
	if err != nil || !strings.HasSuffix(body.Expiration, "Z") {
		t.Fatalf("%q %v", body.Expiration, err)
	}
	if exp.Before(before.Add(time.Hour).Add(-time.Second)) || exp.After(after.Add(time.Hour).Add(time.Second)) {
		t.Fatalf("expiration %v", exp)
	}
}

func TestCreatePreAuthKeyReturnsKeyOnce(t *testing.T) {
	c, _ := recorder(t, 200, `{"preAuthKey":{"id":"9","key":"hskey-new","user":{"id":"3","name":"alice"},"reusable":true,"used":false,"expiration":"2030-01-02T03:04:05Z","aclTags":["tag:example"]}}`)
	k, err := c.CreatePreAuthKey(context.Background(), 3, nil, time.Hour, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != 9 || k.Key != "hskey-new" || k.UserID != 3 || !k.Reusable || k.Expiration.Year() != 2030 || len(k.ACLTags) != 1 {
		t.Fatalf("%+v", k)
	}
}

func TestListPreAuthKeysNeverReturnsKey(t *testing.T) {
	c, got := recorder(t, 200, `{"preAuthKeys":[{"id":"9","key":"hskey-leak","user":{"id":"3"},"used":true,"ephemeral":true,"expiration":"2030-01-02T03:04:05Z","aclTags":["tag:example"]}]}`)
	ks, err := c.ListPreAuthKeys(context.Background())
	if err != nil || len(ks) != 1 {
		t.Fatalf("%+v %v", ks, err)
	}
	if ks[0].Key != "" || ks[0].ID != 9 || ks[0].UserID != 3 || !ks[0].Used || !ks[0].Ephemeral {
		t.Fatalf("%+v", ks[0])
	}
	if got.method != "GET" || got.path != "/api/v1/preauthkey" || got.auth != "Bearer k1" {
		t.Fatalf("%+v", got)
	}
}

func TestPreAuthKeyRejectsNonNumericID(t *testing.T) {
	c, _ := recorder(t, 200, `{"preAuthKeys":[{"id":"x1"}]}`)
	if _, err := c.ListPreAuthKeys(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestExpirePreAuthKeyBody(t *testing.T) {
	c, got := recorder(t, 200, `{}`)
	if err := c.ExpirePreAuthKey(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api/v1/preauthkey/expire" || got.auth != "Bearer k1" || got.body != `{"id":"12"}` {
		t.Fatalf("%+v", got)
	}
}
