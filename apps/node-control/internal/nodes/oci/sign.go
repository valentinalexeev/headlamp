// Package oci signs HTTP requests for the OCI REST API (draft-cavage
// HTTP Signatures, rsa-sha256), reproducing the official SDK's signer.
package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxSignedBody = 10 << 20

// Sign sets the Date header (from now) and the Authorization header on req,
// and for POST/PUT/PATCH also x-content-sha256, content-type (default
// application/json) and content-length, which are then part of the signature.
// keyID is "tenancy/user/fingerprint". The body is read through req.GetBody,
// so req.Body stays untouched and sendable.
func Sign(req *http.Request, keyID string, key *rsa.PrivateKey, now time.Time) error {
	if key == nil {
		return errors.New("oci: no private key")
	}
	if keyID == "" {
		return errors.New("oci: empty key id")
	}
	method := strings.ToLower(req.Method)
	headers := []string{"date", "(request-target)", "host"}
	switch method {
	case "get", "head", "delete":
	case "post", "put", "patch":
		var body []byte
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return errors.New("oci: request body cannot be replayed for signing")
			}
			rc, err := req.GetBody()
			if err != nil {
				return fmt.Errorf("oci: reading body: %w", err)
			}
			body, err = io.ReadAll(io.LimitReader(rc, maxSignedBody+1))
			rc.Close()
			if err != nil {
				return fmt.Errorf("oci: reading body: %w", err)
			}
			if len(body) > maxSignedBody {
				return errors.New("oci: request body too large to sign")
			}
		}
		sum := sha256.Sum256(body)
		req.Header.Set("x-content-sha256", base64.StdEncoding.EncodeToString(sum[:]))
		if req.Header.Get("content-type") == "" {
			req.Header.Set("content-type", "application/json")
		}
		req.Header.Set("content-length", strconv.Itoa(len(body)))
		headers = append(headers, "content-length", "content-type", "x-content-sha256")
	default:
		return fmt.Errorf("oci: cannot sign %s requests", req.Method)
	}

	req.Header.Set("date", now.UTC().Format(http.TimeFormat))
	signing := signingString(req, method, headers)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return fmt.Errorf("oci: signing: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`Signature algorithm="rsa-sha256",headers="%s",keyId="%s",signature="%s",version="1"`,
		strings.Join(headers, " "), keyID, base64.StdEncoding.EncodeToString(sig)))
	return nil
}

// signingString is the exact text that gets signed: one "name: value" line per
// signed header, joined by newlines. req must already carry its headers.
func signingString(req *http.Request, method string, headers []string) string {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	var sb strings.Builder
	for i, h := range headers {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(h + ": ")
		switch h {
		case "(request-target)":
			sb.WriteString(method + " " + req.URL.RequestURI())
		case "host":
			sb.WriteString(host)
		default:
			sb.WriteString(req.Header.Get(h))
		}
	}
	return sb.String()
}
