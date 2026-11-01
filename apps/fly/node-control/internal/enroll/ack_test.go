package enroll

import (
	"sync"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

func sampleAck(id, meaning string) core.Acknowledgement {
	return core.Acknowledgement{Module: "enroll", ID: id, Meaning: meaning, By: "alice", At: time.Unix(2_000_000, 0)}
}

func TestContractAcknowledgements(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		if got, err := e.b.Acknowledgements(); err != nil || len(got) != 0 {
			t.Fatalf("empty store: %+v %v", got, err)
		}
		a := sampleAck("bridge-sync-bridge-2", "m1")
		a.Until = time.Unix(2_086_400, 0)
		b := sampleAck("devices-expiring", "m2")
		other := sampleAck("devices-expiring", "m3")
		other.Module = "funnel" // the same id from another module is another item
		for _, x := range []core.Acknowledgement{a, b, other} {
			if err := e.b.PutAcknowledgement(x); err != nil {
				t.Fatal(err)
			}
		}
		got, err := e.b.Acknowledgements()
		if err != nil || len(got) != 3 {
			t.Fatalf("%+v %v", got, err)
		}
		byKey := map[string]core.Acknowledgement{}
		for _, x := range got {
			byKey[x.Module+"/"+x.ID] = x
		}
		ga := byKey["enroll/bridge-sync-bridge-2"]
		if ga.Meaning != "m1" || ga.By != "alice" || !ga.At.Equal(a.At) || !ga.Until.Equal(a.Until) {
			t.Errorf("%+v", ga)
		}
		if gb := byKey["enroll/devices-expiring"]; gb.Meaning != "m2" || !gb.Until.IsZero() {
			t.Errorf("no expiry reads as zero: %+v", gb)
		}

		// Another acknowledgement of the same item replaces it, and a delete of the old one then changes nothing.
		newer := b
		newer.Meaning, newer.By, newer.At = "m9", "bob", b.At.Add(time.Minute)
		if err := e.b.PutAcknowledgement(newer); err != nil {
			t.Fatal(err)
		}
		if removed, err := e.b.DeleteAcknowledgement(b); err != nil || removed {
			t.Errorf("deleting what was replaced: %v %v", removed, err)
		}
		if got, _ := e.b.Acknowledgements(); len(got) != 3 {
			t.Errorf("replacing must not add a row: %+v", got)
		}
		if removed, err := e.b.DeleteAcknowledgement(newer); err != nil || !removed {
			t.Errorf("deleting the current one: %v %v", removed, err)
		}
		if removed, err := e.b.DeleteAcknowledgement(newer); err != nil || removed {
			t.Errorf("deleting it twice: %v %v", removed, err)
		}
		got, _ = e.b.Acknowledgements()
		if len(got) != 2 {
			t.Errorf("%+v", got)
		}
	})
}

func TestContractAcknowledgementsConcurrent(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		a := sampleAck("x", "m")
		if err := e.b.PutAcknowledgement(a); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		removed := 0
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := e.b.DeleteAcknowledgement(a)
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					removed++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if removed != 1 {
			t.Errorf("exactly one delete removes it, got %d", removed)
		}
	})
}

func TestModuleAckStoreOnlyWhenActive(t *testing.T) {
	if (&Module{}).AckStore() != nil {
		t.Error("an inactive module has nothing to persist to")
	}
	f := newRegFx(t, nil)
	if attentionFor(f).AckStore() == nil {
		t.Error("an active module offers its store")
	}
}

func TestAcknowledgementsAreNotInTheEnrolmentActivity(t *testing.T) {
	f := newRegFx(t, nil)
	f.store.Audit("alice", core.ActionAcknowledge, "policy/policy: something the caller may not see")
	entries, err := attentionFor(f).Activity(t.Context(), "bob", 10)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the core gates these by the module of the item: %+v %v", entries, err)
	}
}
