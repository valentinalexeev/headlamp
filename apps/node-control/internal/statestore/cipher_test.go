package statestore

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

var cipherTestKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestCipherSealOpenRoundTrip(t *testing.T) {
	c, err := NewCipher(cipherTestKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal("app:enroll", "d:1", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hello")) || !bytes.HasPrefix(sealed, encMagic) {
		t.Fatalf("not sealed: %q", sealed)
	}
	pt, err := c.Open("app:enroll", "d:1", sealed)
	if err != nil || string(pt) != "hello" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
	// Bound to its location.
	if _, err := c.Open("app:enroll", "d:2", sealed); err == nil {
		t.Fatal("opened at another id")
	}
	if _, err := c.Open("other:enroll", "d:1", sealed); err == nil {
		t.Fatal("opened under another scope")
	}
	// Two seals of the same value differ (random nonce).
	again, _ := c.Seal("app:enroll", "d:1", []byte("hello"))
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused")
	}
}

func TestCipherRefusesTamperingTruncationAndMissingKey(t *testing.T) {
	c, _ := NewCipher(cipherTestKey)
	sealed, _ := c.Seal("s", "i", []byte("value"))
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := c.Open("s", "i", tampered); err == nil {
		t.Fatal("tampered value opened")
	}
	if _, err := c.Open("s", "i", sealed[:len(encMagic)+3]); err == nil {
		t.Fatal("truncated value opened")
	}
	if _, err := c.Open("s", "i", []byte("plain")); err == nil {
		t.Fatal("an unsealed value was accepted by a sealing cipher")
	}
	var off *Cipher
	if _, err := off.Open("s", "i", sealed); err == nil {
		t.Fatal("ciphertext handed back as if it were plaintext")
	}
	if got, err := off.Open("s", "i", []byte("plain")); err != nil || string(got) != "plain" {
		t.Fatalf("passthrough = %q, %v", got, err)
	}
}

func TestCipherKeyValidationNeverEchoesTheKey(t *testing.T) {
	if c, err := NewCipher(""); c != nil || err != nil {
		t.Fatalf("empty key = %v, %v (encryption off)", c, err)
	}
	for _, bad := range []string{"not base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		_, err := NewCipher(bad)
		if err == nil || strings.Contains(err.Error(), bad) {
			t.Fatalf("NewCipher(%q) = %v", bad, err)
		}
	}
}

func TestCipherIsCompatibleWithEncryptedStore(t *testing.T) {
	// Same envelope as Encrypted, so the two can be reasoned about as one format.
	c, _ := NewCipher(cipherTestKey)
	sealed, _ := c.Seal("scope", "id", []byte("v"))
	if !bytes.HasPrefix(sealed, []byte("ncenc1:")) {
		t.Fatalf("envelope prefix = %q", sealed[:7])
	}
}
