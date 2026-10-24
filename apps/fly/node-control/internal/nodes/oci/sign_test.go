package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type vectorCase struct {
	Name    string            `json:"name"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`

	SigningString string `json:"signing_string"`
	SignatureB64  string `json:"signature_b64"`
}

type vectors struct {
	KeyID  string       `json:"key_id"`
	PubDER string       `json:"public_key_pkix_der_b64"`
	Date   string       `json:"date"`
	Cases  []vectorCase `json:"cases"`
}

// testKey is generated in memory for the run; no private key is stored anywhere.
var (
	keyOnce sync.Once
	testRSA *rsa.PrivateKey
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testRSA = k
	})
	return testRSA
}

// loadVectors returns the SDK vectors and the public key their signatures were made with.
func loadVectors(t *testing.T) (vectors, *rsa.PublicKey) {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/oci_signature_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(v.PubDER)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := pk.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("public key is %T", pk)
	}
	return v, pub
}

func authField(auth, name string) string {
	m := regexp.MustCompile(name + `="([^"]*)"`).FindStringSubmatch(auth)
	if m == nil {
		return ""
	}
	return m[1]
}

// RSA PKCS#1 v1.5 is deterministic: the SDK's signature verifies against the
// SHA-256 of the signing string exactly when that string is byte for byte what
// the SDK signed. So the signing string Go builds is checked against the SDK's
// signature with the public key alone, and Sign itself is exercised with a key
// generated at test time.
func TestSignMatchesSDKVectors(t *testing.T) {
	v, sdkPub := loadVectors(t)
	key := testKey(t)
	now, err := http.ParseTime(v.Date)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 3 {
		t.Fatalf("want at least 3 vectors, got %d", len(v.Cases))
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var req *http.Request
			var err error
			if c.Body == "" {
				req, err = http.NewRequest(c.Method, c.URL, nil)
			} else {
				req, err = http.NewRequest(c.Method, c.URL, strings.NewReader(c.Body))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := Sign(req, v.KeyID, key, now); err != nil {
				t.Fatal(err)
			}
			for _, h := range []string{"date", "x-content-sha256", "content-length"} {
				want, signed := c.Headers[h]
				if got := req.Header.Get(h); signed && got != want {
					t.Errorf("%s:\n got  %q\n want %q", h, got, want)
				} else if !signed && got != "" {
					t.Errorf("%s set unexpectedly: %q", h, got)
				}
			}
			if _, signed := c.Headers["content-length"]; signed && req.Header.Get("content-type") != c.Headers["content-type"] {
				t.Errorf("content-type %q want %q", req.Header.Get("content-type"), c.Headers["content-type"])
			}

			auth := req.Header.Get("Authorization")
			sdkAuth := c.Headers["authorization"]
			if authField(auth, "headers") != authField(sdkAuth, "headers") || authField(auth, "keyId") != authField(sdkAuth, "keyId") ||
				authField(auth, "algorithm") != authField(sdkAuth, "algorithm") || authField(auth, "version") != authField(sdkAuth, "version") {
				t.Errorf("authorization parameters differ:\n got  %q\n want %q", auth, sdkAuth)
			}
			names := strings.Split(authField(auth, "headers"), " ")
			goString := signingString(req, strings.ToLower(c.Method), names)
			if goString != c.SigningString {
				t.Fatalf("signing string differs:\n got  %q\n want %q", goString, c.SigningString)
			}
			sdkSig, err := base64.StdEncoding.DecodeString(c.SignatureB64)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(goString))
			if err := rsa.VerifyPKCS1v15(sdkPub, crypto.SHA256, digest[:], sdkSig); err != nil {
				t.Fatalf("SDK signature does not verify over Go's signing string: %v", err)
			}
			// The signature Go produced verifies over the same string with the run's key.
			mySig, err := base64.StdEncoding.DecodeString(authField(auth, "signature"))
			if err != nil {
				t.Fatal(err)
			}
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], mySig); err != nil {
				t.Fatalf("Sign produced a signature that does not verify: %v", err)
			}
			// The body must still be sendable after signing.
			if c.Body != "" {
				buf := make([]byte, 64)
				n, _ := req.Body.Read(buf)
				if string(buf[:n]) != c.Body {
					t.Errorf("body consumed or altered: %q", buf[:n])
				}
			}
		})
	}
}

func TestSignRejectsBadInput(t *testing.T) {
	v, _ := loadVectors(t)
	key := testKey(t)
	now := time.Unix(1_000_000, 0)
	req, _ := http.NewRequest("TRACE", "https://example.com/x", nil)
	if err := Sign(req, v.KeyID, key, now); err == nil {
		t.Error("unsupported method must be rejected")
	}
	req, _ = http.NewRequest("GET", "https://example.com/x", nil)
	if err := Sign(req, v.KeyID, nil, now); err == nil {
		t.Error("nil key must be rejected")
	}
	if err := Sign(req, "", key, now); err == nil {
		t.Error("empty key id must be rejected")
	}
	// A body that cannot be replayed cannot be hashed without consuming it.
	req, _ = http.NewRequest("POST", "https://example.com/x", strings.NewReader("abc"))
	req.GetBody = nil
	if err := Sign(req, v.KeyID, key, now); err == nil {
		t.Error("non-replayable body must be rejected")
	}
}
