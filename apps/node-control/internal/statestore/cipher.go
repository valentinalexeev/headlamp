package statestore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// Cipher seals arbitrary values for a shared Redis with the same envelope as Encrypted (magic | 12-byte nonce |
// AES-256-GCM ciphertext). The additional authenticated data is scope + id, so a value copied to another field or
// app fails to open. A nil Cipher passes values through. Error texts never contain key material.
type Cipher struct{ aead cipher.AEAD }

// NewCipher returns a Cipher for the base64-encoded 32-byte key. An empty key returns a nil Cipher (encryption off).
func NewCipher(b64key string) (*Cipher, error) {
	if b64key == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("NODE_CONTROL_STATE_KEY must be 32 random bytes, base64-encoded (openssl rand -base64 32)")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("cannot use NODE_CONTROL_STATE_KEY")
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, errors.New("cannot use NODE_CONTROL_STATE_KEY")
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) aad(scope, id string) []byte { return []byte(scope + "\x00" + id) }

// Seal encrypts pt for the location scope/id. On a nil Cipher it returns pt unchanged.
func (c *Cipher) Seal(scope, id string, pt []byte) ([]byte, error) {
	if c == nil {
		return pt, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append([]byte(nil), encMagic...), nonce...)
	return c.aead.Seal(out, nonce, pt, c.aad(scope, id)), nil
}

// Open decrypts a value sealed for scope/id. On a nil Cipher it returns b unless b is ciphertext.
func (c *Cipher) Open(scope, id string, b []byte) ([]byte, error) {
	isEnc := bytes.HasPrefix(b, encMagic)
	switch {
	case c == nil && isEnc:
		return nil, errors.New("stored value is encrypted: set NODE_CONTROL_STATE_KEY")
	case c == nil:
		return b, nil
	case !isEnc:
		return nil, errors.New("stored value is not encrypted")
	}
	b = b[len(encMagic):]
	if len(b) < c.aead.NonceSize() {
		return nil, errors.New("stored value is truncated")
	}
	nonce, ct := b[:c.aead.NonceSize()], b[c.aead.NonceSize():]
	pt, err := c.aead.Open(nil, nonce, ct, c.aad(scope, id))
	if err != nil {
		return nil, errors.New("cannot decrypt stored value (wrong NODE_CONTROL_STATE_KEY?)")
	}
	return pt, nil
}
