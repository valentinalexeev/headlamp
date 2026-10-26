package enroll

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContractDeleteDevice(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		keep, keepSecret := e.create(t, "keep", time.Hour)
		gone, goneSecret := e.create(t, "gone", time.Hour)
		tok, err := e.b.NewLink(gone.ID, time.Hour, 3)
		if err != nil {
			t.Fatal(err)
		}

		// An active device is never deleted, and nothing changes.
		if err := e.b.DeleteDevice(gone.ID); !errors.Is(err, ErrNotRevoked) {
			t.Fatalf("deleting an active device: %v, want ErrNotRevoked", err)
		}
		if _, err := e.b.GetDevice(gone.ID); err != nil {
			t.Fatalf("the active device vanished: %v", err)
		}
		if err := e.b.DeleteDevice(999); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleting an unknown id: %v, want ErrNotFound", err)
		}

		if err := e.b.Revoke(gone.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.b.DeleteDevice(gone.ID); err != nil {
			t.Fatalf("deleting a revoked device: %v", err)
		}
		if _, err := e.b.GetDevice(gone.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetDevice after delete: %v", err)
		}
		devs, err := e.b.ListDevices()
		if err != nil || len(devs) != 1 || devs[0].ID != keep.ID {
			t.Fatalf("ListDevices = %+v, %v", devs, err)
		}
		if _, err := e.b.SubscriptionSecret(gone.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SubscriptionSecret after delete: %v", err)
		}
		mustNil(t, mustVerify(e.b, goneSecret), nil, "secret of a deleted device")
		mustDevice(t, mustVerify(e.b, keepSecret), nil, "secret of the device that stayed")
		d, err := e.b.ConsumeToken(tok)
		mustNil(t, d, err, "link of a deleted device")
		creds, err := e.b.ActiveCredentials()
		if err != nil || len(creds) != 3 {
			t.Fatalf("credentials = %+v, %v (the other device's three must remain)", creds, err)
		}

		// The id is not issued again, and a second delete says the device is gone.
		n, _ := e.create(t, "new", time.Hour)
		if n.ID <= gone.ID {
			t.Fatalf("new device got id %d; %d was already issued", n.ID, gone.ID)
		}
		if err := e.b.DeleteDevice(gone.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
	})
}

func TestSQLiteDeleteRemovesEveryRow(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("phone", 0)
	if _, err := e.store.NewLink(d.ID, time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	e.store.Revoke(d.ID)
	if err := e.store.DeleteDevice(d.ID); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"devices", "device_uuids", "tokens"} {
		var n int
		if err := e.store.db.QueryRow("SELECT count(*) FROM " + tbl).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s still has %d rows (%v)", tbl, n, err)
		}
	}
}

func TestRedisDeleteLeavesNoDeviceKeyAndKeepsTheCounter(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	s.Revoke(d.ID)
	if err := s.DeleteDevice(d.ID); err != nil {
		t.Fatal(err)
	}
	if e.mr.Exists(s.deviceKey(d.ID)) {
		t.Fatal("the device key is still there")
	}
	if got := e.mr.HGet(s.metaKey(), "seq"); got != strconv.FormatInt(d.ID, 10) {
		t.Fatalf("counter = %q, want it kept at %d", got, d.ID)
	}
	// A delete of a missing id must not create anything.
	if err := s.DeleteDevice(77); !errors.Is(err, ErrNotFound) || e.mr.Exists(s.deviceKey(77)) {
		t.Fatalf("delete of a missing id: %v", err)
	}
}

func TestAdminDeleteDevice(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	path := "/enroll/devices/" + strconv.FormatInt(id, 10)

	t.Run("needs a JSON content type", func(t *testing.T) {
		if rec := f.do("DELETE", path, "text/plain", ""); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("an active device is refused with 409 and stays", func(t *testing.T) {
		rec := f.do("DELETE", path, "application/json", "{}")
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "revoke") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if _, err := f.store.GetDevice(id); err != nil {
			t.Fatal("the device was deleted")
		}
	})
	t.Run("unknown and malformed ids", func(t *testing.T) {
		if rec := f.do("DELETE", "/enroll/devices/999", "application/json", "{}"); rec.Code != http.StatusNotFound {
			t.Fatalf("unknown: %d", rec.Code)
		}
		if rec := f.do("DELETE", "/enroll/devices/abc", "application/json", "{}"); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("malformed: %d", rec.Code)
		}
	})
	t.Run("a revoked device is deleted and leaves the list", func(t *testing.T) {
		if rec := f.post(idPath(id, "revoke"), map[string]any{}); rec.Code != 200 {
			t.Fatalf("revoke %d", rec.Code)
		}
		pushesBefore := len(f.agent.pushes)
		rec := f.do("DELETE", path, "application/json", "{}")
		if rec.Code != 200 || decode(t, rec)["ok"] != true {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		list := decode(t, f.do("GET", "/enroll/devices", "", ""))
		if devs, _ := list["devices"].([]any); len(devs) != 0 {
			t.Fatalf("devices after delete: %v", devs)
		}
		if len(f.agent.pushes) != pushesBefore {
			t.Fatal("a delete must not push anything to the agent (only active devices are ever sent)")
		}
		if rec := f.do("DELETE", path, "application/json", "{}"); rec.Code != http.StatusNotFound {
			t.Fatalf("second delete: %d", rec.Code)
		}
	})
}

func TestPageHidesRevokedDevicesAndOffersDelete(t *testing.T) {
	page := string(enrollHTML)
	for _, want := range []string{
		`id="showrevoked"`, "Show revoked devices", `id="revokedcount"`,
		`showRevoked ? all : all.filter((d) => !d.revoked)`, `let showRevoked = false;`, "enrollShowRevoked",
		`method: "DELETE"`, `btn("Delete", "danger"`, "permanently",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The script must not be duplicated or spliced: one load(), one row-button block, one Delete.
	for what, n := range map[string]int{`async function load()`: 1, `btn("New link"`: 1, `btn("Delete"`: 1, `function showSync`: 1,
		`tr.appendChild(td); body.appendChild(tr);`: 2} { // the second is the ingress table's own row
		if got := strings.Count(page, what); got != n {
			t.Errorf("%q occurs %d times, want %d", what, got, n)
		}
	}
	// On a revoked row only Delete is offered: the other three buttons live in the else branch.
	i := strings.Index(page, "if (d.revoked) {")
	if i < 0 {
		t.Fatal("revoked branch not found")
	}
	k := strings.Index(page[i:], "} else {")
	if k < 0 {
		t.Fatal("else branch not found")
	}
	j := i + k
	revokedBranch, rest := page[i:j], page[j:]
	for _, b := range []string{`btn("New link"`, `btn("Extend"`, `btn("Revoke"`} {
		if strings.Contains(revokedBranch, b) || !strings.Contains(rest, b) {
			t.Errorf("%s must be offered only on non-revoked rows", b)
		}
	}
	if strings.Contains(page, `localStorage.setItem("enrollShowRevoked", showRevoked ? "1" : "0"); } catch`) == false {
		t.Error("the choice must be remembered inside a try/catch")
	}
}

func TestPageShowsEnrolledAndLastActive(t *testing.T) {
	page := string(enrollHTML)
	for _, want := range []string{
		"<th", ">Enrolled</th>", ">Last active</th>", "d.enrol_count > 0", "d.enrolled_at", "d.last_active > 0", "function ago(t)",
		"Not a connection log",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The values are written with textContent (no markup injection from stored data).
	if strings.Contains(page, "innerHTML") {
		t.Error("the page must not use innerHTML")
	}
}
