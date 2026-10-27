package statestore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"tailscale.com/ipn"
)

// encMagic prefixes every encrypted value: magic | 12-byte nonce | AES-256-GCM ciphertext.
var encMagic = []byte("ncenc1:")

// ErrUnreadable marks a stored value this layer refuses to hand back: it does
// not decrypt, is truncated, is plaintext while encryption is on, or is
// ciphertext while it is off. Test with errors.Is; the messages stay as they
// are (callers that only show them see no change).
var ErrUnreadable = errors.New("stored state unreadable")

// unreadable is an error with its own message that matches ErrUnreadable.
type unreadable string

func (u unreadable) Error() string        { return string(u) }
func (u unreadable) Is(target error) bool { return target == ErrUnreadable }

// Encrypted encrypts values before they reach the wrapped store (ported from botboard's encstore.go). The
// additional authenticated data is the location (scope + state id), so a value copied to another key or app
// fails to decrypt. With no key it passes values through but refuses to hand back ciphertext as if it were
// state. Error texts never contain key material.
type Encrypted struct {
	inner ipn.StateStore
	aead  cipher.AEAD // nil: encryption off
	scope string
}

// ValidateStateKey checks a NODE_CONTROL_STATE_KEY value (empty is valid: encryption off) without echoing it.
func ValidateStateKey(b64key string) error {
	_, err := NewEncrypted(nil, b64key, "")
	return err
}

// NewEncrypted returns a store that encrypts with the base64-encoded 32-byte key (empty: off). scope binds the
// ciphertext to its location, normally the Redis hash key.
func NewEncrypted(inner ipn.StateStore, b64key, scope string) (*Encrypted, error) {
	e := &Encrypted{inner: inner, scope: scope}
	if b64key == "" {
		return e, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64key)
	if err != nil || len(key) != 32 {
		return nil, errors.New("NODE_CONTROL_STATE_KEY must be 32 random bytes, base64-encoded (openssl rand -base64 32)")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("cannot use NODE_CONTROL_STATE_KEY")
	}
	if e.aead, err = cipher.NewGCM(blk); err != nil {
		return nil, errors.New("cannot use NODE_CONTROL_STATE_KEY")
	}
	return e, nil
}

func (e *Encrypted) aad(id ipn.StateKey) []byte { return []byte(e.scope + "\x00" + string(id)) }

func (e *Encrypted) ReadState(id ipn.StateKey) ([]byte, error) {
	b, err := e.inner.ReadState(id)
	if err != nil {
		return nil, err
	}
	isEnc := bytes.HasPrefix(b, encMagic)
	switch {
	case e.aead == nil && isEnc:
		return nil, unreadable("stored state is encrypted: set NODE_CONTROL_STATE_KEY")
	case e.aead == nil:
		return b, nil
	case !isEnc:
		return nil, unreadable(fmt.Sprintf("stored state %q is not encrypted: delete it to re-register the node, then restart", e.scope+"/"+string(id)))
	}
	b = b[len(encMagic):]
	if len(b) < e.aead.NonceSize() {
		return nil, unreadable("stored state is truncated")
	}
	nonce, ct := b[:e.aead.NonceSize()], b[e.aead.NonceSize():]
	pt, err := e.aead.Open(nil, nonce, ct, e.aad(id))
	if err != nil {
		return nil, unreadable(fmt.Sprintf("cannot decrypt stored state %q (wrong NODE_CONTROL_STATE_KEY?)", e.scope+"/"+string(id)))
	}
	return pt, nil
}

func (e *Encrypted) WriteState(id ipn.StateKey, bs []byte) error {
	if e.aead == nil || bs == nil {
		return e.inner.WriteState(id, bs)
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	out := append(append([]byte(nil), encMagic...), nonce...)
	return e.inner.WriteState(id, e.aead.Seal(out, nonce, bs, e.aad(id)))
}
