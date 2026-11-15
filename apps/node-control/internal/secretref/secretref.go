// Package secretref resolves secret references in the environment at start (#361).
//
// A secret variable (see credcheck.Candidates, plus REDIS_URL) may hold a reference instead of the
// value: gcpsm://projects/<project>/secrets/<secret>[/versions/<version>] reads that version
// (default latest) from Google Secret Manager with Application Default Credentials. On GCE the
// VM's service account needs roles/secretmanager.secretAccessor on each secret. A variable with a
// plain value is not changed, and without a reference nothing is read: the feature is off unless
// a reference is used. No error and no log line holds a value.
package secretref

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/credcheck"
)

// Prefix marks a Secret Manager reference.
const Prefix = "gcpsm://"

// Scope is the OAuth scope that Secret Manager access needs.
const Scope = "https://www.googleapis.com/auth/cloud-platform"

const (
	defaultBase = "https://secretmanager.googleapis.com/v1"
	callTimeout = 15 * time.Second
	// maxBody caps an answer: Secret Manager payloads are at most 64 KiB, base64 in JSON.
	maxBody = 128 << 10
)

// refRE is the reference form. Ids follow the Secret Manager rules (letters, digits, - and _).
var refRE = regexp.MustCompile(
	`^gcpsm://projects/([a-z][a-z0-9-]{4,28}[a-z0-9]|[0-9]{1,20})/secrets/([A-Za-z0-9_-]{1,255})` +
		`(?:/versions/(latest|[0-9]{1,20}))?$`)

// Resolver reads references. Token gives an OAuth access token; tests replace Base and Token.
type Resolver struct {
	Base  string // default https://secretmanager.googleapis.com/v1 (SECRETREF_GCP_URL)
	HTTP  *http.Client
	Token func(ctx context.Context) (string, error)
}

// Names lists the variables that may hold a reference: the secret variables and REDIS_URL, but
// not AWS_SECRET_ACCESS_KEY, which the AWS SDK reads from the process environment itself.
func Names(env core.Env) []string {
	var out []string
	for _, n := range credcheck.Candidates(env) {
		if n != sdkOwned {
			out = append(out, n)
		}
	}
	return append(out, "REDIS_URL")
}

// sdkOwned is read by the AWS SDK from the process environment, so a reference there cannot work.
const sdkOwned = "AWS_SECRET_ACCESS_KEY"

// Resolve returns env with each reference replaced by its secret value, and the names it
// resolved. With no reference it returns env unchanged and does not call Token.
func (r *Resolver) Resolve(ctx context.Context, env core.Env) (core.Env, []string, error) {
	if strings.HasPrefix(strings.TrimSpace(env.Getenv(sdkOwned)), Prefix) {
		return env, nil, errors.New(sdkOwned + ": a Secret Manager reference is not supported " +
			"here (the AWS SDK reads it): use the instance role or the value")
	}
	over := map[string]string{}
	var done []string
	for _, n := range Names(env) {
		v := strings.TrimSpace(env.Getenv(n))
		if !strings.HasPrefix(v, Prefix) {
			continue
		}
		val, err := r.read(ctx, v)
		if err != nil {
			return env, nil, fmt.Errorf("%s: %w", n, err)
		}
		over[n] = val
		done = append(done, n)
	}
	return env.With(over), done, nil
}

func (r *Resolver) read(ctx context.Context, ref string) (string, error) {
	m := refRE.FindStringSubmatch(ref)
	if m == nil {
		return "", errors.New("not a valid reference: want " + Prefix +
			"projects/<project>/secrets/<secret>[/versions/<version>]")
	}
	version := m[3]
	if version == "" {
		version = "latest"
	}
	tok, err := r.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("GCP credentials: %w", err)
	}
	base := strings.TrimRight(r.Base, "/")
	if base == "" {
		base = defaultBase
	}
	u := base + "/projects/" + m[1] + "/secrets/" + m[2] + "/versions/" + version + ":access"
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("secret manager: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("secret manager: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The error answer names the secret, never its value.
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return "", errors.New("secret manager: " + msg)
	}
	var out struct {
		Payload struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", errors.New("secret manager: the answer does not parse")
	}
	val, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return "", errors.New("secret manager: the payload is not base64")
	}
	// A value written with `echo` ends in a newline; no secret of node-control has one.
	s := strings.TrimRight(string(val), "\r\n")
	if s == "" {
		return "", errors.New("secret manager: the secret version is empty")
	}
	return s, nil
}
