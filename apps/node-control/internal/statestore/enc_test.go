package statestore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
)

func newB64Key(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestEncRoundTrip(t *testing.T) {
	raw := new(mem.Store)
	s, err := NewEncrypted(raw, newB64Key(t), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteState("a", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	stored, _ := raw.ReadState("a")
	if bytes.Contains(stored, []byte("secret")) || !bytes.HasPrefix(stored, []byte("ncenc1:")) {
		t.Fatalf("value not encrypted: %q", stored)
	}
	if got, err := s.ReadState("a"); err != nil || string(got) != "secret" {
		t.Fatalf("roundtrip: %q, %v", got, err)
	}
}

func TestEncDeleteReachesTheBackend(t *testing.T) {
	r, mr := newTestRedis(t)
	s, _ := NewEncrypted(r, newB64Key(t), testKey)
	s.WriteState("a", []byte("secret"))
	if err := s.WriteState("a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadState("a"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("after delete: %v", err)
	}
	if mr.Exists(testKey) {
		t.Fatal("field still stored")
	}
}

func TestEncRejectsBadKey(t *testing.T) {
	for name, k := range map[string]string{"short": "short", "not base64": "!!!", "16 bytes": base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		t.Run(name, func(t *testing.T) {
			_, err := NewEncrypted(new(mem.Store), k, "s")
			if err == nil {
				t.Fatal("bad key accepted")
			}
			if strings.Contains(err.Error(), k) && k != "" {
				t.Fatalf("error echoes the key: %v", err)
			}
		})
	}
}

func TestEncWrongKey(t *testing.T) {
	raw := new(mem.Store)
	s, _ := NewEncrypted(raw, newB64Key(t), testKey)
	s.WriteState("a", []byte("secret"))
	other, _ := NewEncrypted(raw, newB64Key(t), testKey)
	if _, err := other.ReadState("a"); err == nil || !strings.Contains(err.Error(), "wrong NODE_CONTROL_STATE_KEY") {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestEncPlaintextStoredWithKey(t *testing.T) {
	raw := new(mem.Store)
	raw.WriteState("p", []byte("plain"))
	s, _ := NewEncrypted(raw, newB64Key(t), testKey)
	if _, err := s.ReadState("p"); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("plaintext with key: %v", err)
	}
}

func TestEncCiphertextWithoutKey(t *testing.T) {
	raw := new(mem.Store)
	s, _ := NewEncrypted(raw, newB64Key(t), testKey)
	s.WriteState("a", []byte("secret"))
	off, err := NewEncrypted(raw, "", testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off.ReadState("a"); err == nil || !strings.Contains(err.Error(), "set NODE_CONTROL_STATE_KEY") {
		t.Fatalf("ciphertext without key: %v", err)
	}
	// off mode is a plain passthrough
	if err := off.WriteState("p", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got, _ := off.ReadState("p"); string(got) != "x" {
		t.Fatal("passthrough broken")
	}
}

func TestEncBoundToKeyAndID(t *testing.T) {
	raw := new(mem.Store)
	key := newB64Key(t)
	s, _ := NewEncrypted(raw, key, testKey)
	s.WriteState("a", []byte("secret"))
	stored, _ := raw.ReadState("a")

	t.Run("other state id", func(t *testing.T) {
		raw.WriteState("b", stored)
		if _, err := s.ReadState("b"); err == nil {
			t.Fatal("ciphertext accepted under a different id")
		}
	})
	t.Run("other scope", func(t *testing.T) {
		moved, _ := NewEncrypted(raw, key, "other-app:tsnet")
		if _, err := moved.ReadState("a"); err == nil {
			t.Fatal("ciphertext accepted under a different scope")
		}
	})
}

func TestEncTruncated(t *testing.T) {
	raw := new(mem.Store)
	s, _ := NewEncrypted(raw, newB64Key(t), testKey)
	raw.WriteState("a", []byte("ncenc1:short"))
	if _, err := s.ReadState("a"); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated: %v", err)
	}
	raw.WriteState("a", []byte("ncenc1:"))
	if _, err := s.ReadState("a"); err == nil {
		t.Fatal("magic only accepted")
	}
}

func TestEncErrorsHaveNoKeyMaterial(t *testing.T) {
	raw := new(mem.Store)
	key := newB64Key(t)
	s, _ := NewEncrypted(raw, key, testKey)
	s.WriteState("a", []byte("secret"))
	other, _ := NewEncrypted(raw, newB64Key(t), testKey)
	_, err := other.ReadState("a")
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("error: %v", err)
	}
}

// Every refusal of a stored value matches ErrUnreadable, so callers can tell it
// from a backend failure without reading the text; a missing key does not.
func TestEncRefusalsMatchErrUnreadable(t *testing.T) {
	raw := new(mem.Store)
	key := newB64Key(t)
	s, _ := NewEncrypted(raw, key, testKey)
	s.WriteState("ok", []byte("secret"))
	other, _ := NewEncrypted(raw, newB64Key(t), testKey)
	off, _ := NewEncrypted(raw, "", testKey)
	raw.WriteState("plain", []byte("plain"))
	raw.WriteState("short", []byte("ncenc1:short"))
	for name, read := range map[string]func() error{
		"wrong key":              func() error { _, err := other.ReadState("ok"); return err },
		"plaintext":              func() error { _, err := s.ReadState("plain"); return err },
		"truncated":              func() error { _, err := s.ReadState("short"); return err },
		"ciphertext without key": func() error { _, err := off.ReadState("ok"); return err },
	} {
		if err := read(); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("%s: %v does not match ErrUnreadable", name, err)
		}
	}
	if _, err := s.ReadState("missing"); errors.Is(err, ErrUnreadable) || !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("missing: %v", err)
	}
}
