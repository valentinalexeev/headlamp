package enroll

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Values of Ingress.UUIDs: which credential an ingress accepts for a device and mode.
const (
	uuidsBase    = "base"    // the UUID stored for the device
	uuidsDerived = "derived" // a UUID computed from the server secret, different per ingress
)

const minDeriveKey = 16

// DeriveUUID computes the credential of one device mode on one ingress: the first 16 bytes of
// HMAC-SHA256(key, "ingress-uuid" 0x00 ingress 0x00 mode 0x00 base) with the version-4 and
// RFC 4122 variant bits set, in canonical lower-case 8-4-4-4-12 form. Without the key nobody can
// compute another ingress's value, so a credential taken from one ingress is useless on the rest.
// A key shorter than 16 bytes yields "" and the caller must fail.
func DeriveUUID(key []byte, ingress, mode, base string) string {
	if len(key) < minDeriveKey {
		return ""
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("ingress-uuid"))
	for _, part := range []string{ingress, mode, base} {
		m.Write([]byte{0})
		m.Write([]byte(part))
	}
	b := m.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// WithKey returns the config carrying a copy of the server secret used to derive credentials.
func (c IngressConfig) WithKey(key []byte) IngressConfig {
	c.key = append([]byte(nil), key...)
	return c
}

// UUIDFor is the only place that decides which UUID an ingress uses for a device mode: the stored
// base UUID for a "base" ingress, the derived one otherwise. An ingress whose field is unset is
// "base" when it is the only one and "derived" when there are several (the safe default).
func (c IngressConfig) UUIDFor(ing Ingress, mode, base string) (string, error) {
	kind := ing.UUIDs
	if kind == "" {
		kind = uuidsDerived
		if c.Single() {
			kind = uuidsBase
		}
	}
	if kind == uuidsBase {
		return base, nil
	}
	u := DeriveUUID(c.key, ing.Name, mode, base)
	if u == "" {
		return "", errors.New("derived credentials need ENROLL_SECRET_KEY")
	}
	return u, nil
}
