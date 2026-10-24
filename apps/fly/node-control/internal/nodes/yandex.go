package nodes

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"example.com/node-control/internal/core"
)

const (
	yandexIAMURL     = "https://iam.api.cloud.yandex.net/iam/v1/tokens"
	yandexComputeURL = "https://compute.api.cloud.yandex.net/compute/v1/instances"
	yandexTimeout    = 20 * time.Second
	yandexSlack      = 5 * time.Minute
	yandexJWTTTL     = time.Hour
)

type yandex struct {
	env core.Env
	hc  *http.Client
	// iamURL and computeURL are overridden in tests.
	iamURL     string
	computeURL string
	now        func() time.Time
	ids        idCache // name -> instance id for idCacheTTL

	mu      sync.Mutex
	token   string
	jwt     string // the assertion behind token, kept only for scrubbing
	expires time.Time
}

// NewYandex returns the Yandex Compute provider, talking to the REST API.
// The target's block is {"yandex": {"instance_name", "instance_id"}}, all
// optional: the instance is found by name in the folder (the id is cached for 15 s)
// (default Target.Name) unless instance_id pins one. Credentials:
// YANDEX_SA_KEY_JSON (service account key with id, service_account_id and
// private_key) and YANDEX_FOLDER_ID (needed for the lookup by name, which
// requires list permission). Start and stop fire the long-running operation
// and do not wait for it.
func NewYandex(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &yandex{env: env, hc: hc, iamURL: yandexIAMURL, computeURL: yandexComputeURL, now: time.Now}
}

type yandexKey struct {
	id, saID string
	key      *rsa.PrivateKey
	secrets  []string
}

// parseRSAPEM accepts PKCS#8 and PKCS#1 PEM text.
func parseRSAPEM(text string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(text))
	if blk == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("not an RSA key")
		}
		return rk, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unsupported key encoding")
}

// key parses YANDEX_SA_KEY_JSON; errors never include its content.
func (y *yandex) key() (*yandexKey, error) {
	raw := y.env.Getenv("YANDEX_SA_KEY_JSON")
	if raw == "" {
		return nil, notConfigured("YANDEX_SA_KEY_JSON is not set")
	}
	var doc struct {
		ID         string `json:"id"`
		SAID       string `json:"service_account_id"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, notConfigured("YANDEX_SA_KEY_JSON is not valid JSON")
	}
	if doc.ID == "" || doc.SAID == "" || doc.PrivateKey == "" {
		return nil, notConfigured("YANDEX_SA_KEY_JSON needs id, service_account_id and private_key")
	}
	k, err := parseRSAPEM(doc.PrivateKey)
	if err != nil {
		return nil, notConfigured("YANDEX_SA_KEY_JSON private_key is not a usable RSA private key: " + err.Error())
	}
	secrets := []string{raw, doc.PrivateKey}
	if esc, err := json.Marshal(doc.PrivateKey); err == nil && len(esc) > 2 {
		secrets = append(secrets, string(esc[1:len(esc)-1]))
	}
	for _, line := range strings.Split(doc.PrivateKey, "\n") {
		if line = strings.TrimSpace(line); len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			secrets = append(secrets, line)
		}
	}
	return &yandexKey{id: doc.ID, saID: doc.SAID, key: k, secrets: secrets}, nil
}

// do sends req and returns the body; non-2xx becomes an APIError. Secrets are
// scrubbed from every message.
func (y *yandex) do(req *http.Request, label string, secrets []string) ([]byte, error) {
	resp, err := y.hc.Do(req)
	if err != nil {
		return nil, &APIError{Message: label + ": " + scrub(err.Error(), secrets)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &APIError{Status: resp.StatusCode, Message: label + ": " + scrub(err.Error(), secrets)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("%s: %d %s", label, resp.StatusCode, scrub(string(b), secrets))}
	}
	return b, nil
}

// iam returns an IAM token, cached until shortly before it expires.
func (y *yandex) iam(ctx context.Context, k *yandexKey) (token, assertion string, err error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.token != "" && y.expires.Sub(y.now()) > yandexSlack {
		return y.token, y.jwt, nil
	}
	now := y.now()
	tok := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
		"iss": k.saID,
		"aud": yandexIAMURL,
		"iat": now.Unix(),
		"exp": now.Add(yandexJWTTTL).Unix(),
	})
	tok.Header["kid"] = k.id
	signed, err := tok.SignedString(k.key)
	if err != nil {
		return "", "", &APIError{Message: "iam auth failed: signing: " + scrub(err.Error(), k.secrets)}
	}
	secrets := append(append([]string{}, k.secrets...), signed)
	payload, _ := json.Marshal(map[string]string{"jwt": signed})
	ctx, cancel := context.WithTimeout(ctx, yandexTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.iamURL, bytes.NewReader(payload))
	if err != nil {
		return "", "", &APIError{Message: "iam auth failed: " + scrub(err.Error(), secrets)}
	}
	req.Header.Set("Content-Type", "application/json")
	b, err := y.do(req, "iam auth failed", secrets)
	if err != nil {
		return "", "", err
	}
	var doc struct {
		Token   string `json:"iamToken"`
		Expires string `json:"expiresAt"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Token == "" {
		return "", "", &APIError{Message: "iam auth failed: bad response"}
	}
	exp, err := time.Parse(time.RFC3339, doc.Expires)
	if err != nil {
		exp = now.Add(yandexJWTTTL) // unknown expiry: assume the short lifetime
	}
	y.token, y.jwt, y.expires = doc.Token, signed, exp
	return doc.Token, signed, nil
}

// call performs an authorized compute request.
func (y *yandex) call(ctx context.Context, k *yandexKey, method, path string, query url.Values) ([]byte, error) {
	tok, assertion, err := y.iam(ctx, k)
	if err != nil {
		return nil, err
	}
	secrets := append(append([]string{}, k.secrets...), tok, assertion)
	u := y.computeURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, yandexTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return y.do(req, method+" "+path, secrets)
}

func (y *yandex) block(n Target) (name, id string) {
	b := n.Block
	name, _ = b["instance_name"].(string)
	id, _ = b["instance_id"].(string)
	if name == "" {
		name = n.Name
	}
	return
}

// resolve returns the key and instance id for the target.
func (y *yandex) resolve(ctx context.Context, n Target) (*yandexKey, string, error) {
	k, err := y.key()
	if err != nil {
		return nil, "", err
	}
	name, id := y.block(n)
	if id != "" {
		return k, id, nil
	}
	if name == "" {
		return nil, "", notConfigured(`node needs a name, or "yandex": {"instance_name": ...} or {"instance_id": ...}`)
	}
	if strings.ContainsAny(name, `"\`) {
		return nil, "", notConfigured("instance name must not contain a double quote or backslash")
	}
	folder := y.env.Getenv("YANDEX_FOLDER_ID")
	if folder == "" {
		return nil, "", notConfigured("YANDEX_FOLDER_ID is not set (or pin \"instance_id\")")
	}
	key := y.cacheKey(n)
	if cached, ok := y.ids.get(key, y.now()); ok {
		return k, cached, nil
	}
	b, err := y.call(ctx, k, http.MethodGet, "", url.Values{"folderId": {folder}, "filter": {`name="` + name + `"`}})
	if err != nil {
		return nil, "", err
	}
	var list struct {
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, "", &APIError{Message: "listing instances: bad response"}
	}
	switch len(list.Instances) {
	case 0:
		return nil, "", &APIError{Message: fmt.Sprintf("no instance named '%s' in the folder", scrub(name, nil))}
	case 1:
		y.ids.put(key, list.Instances[0].ID, y.now())
		return k, list.Instances[0].ID, nil
	}
	return nil, "", &APIError{Message: fmt.Sprintf("%d instances named '%s'; refusing to guess", len(list.Instances), scrub(name, nil))}
}

// cacheKey identifies a name lookup; the pinned-id case never uses it.
func (y *yandex) cacheKey(n Target) string {
	name, _ := y.block(n)
	return y.env.Getenv("YANDEX_FOLDER_ID") + "|" + name
}

func (y *yandex) get(ctx context.Context, k *yandexKey, id string) (string, error) {
	b, err := y.call(ctx, k, http.MethodGet, "/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	var doc struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", &APIError{Message: "reading instance: bad response"}
	}
	if doc.Status == "" {
		return "UNKNOWN", nil
	}
	return doc.Status, nil
}

func (y *yandex) Status(ctx context.Context, n Target) (Status, error) {
	k, id, err := y.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	st, err := y.get(ctx, k, id)
	if err != nil {
		y.ids.forgetOnNotFound(y.cacheKey(n), err)
		return nil, err
	}
	return Status{"status": st}, nil
}

func (y *yandex) power(ctx context.Context, n Target, want, transient, action string) (string, string, error) {
	k, id, err := y.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	prev, err := y.get(ctx, k, id)
	if err != nil {
		y.ids.forgetOnNotFound(y.cacheKey(n), err)
		return "", "", err
	}
	if prev == want || prev == transient {
		return prev, prev, nil
	}
	if _, err := y.call(ctx, k, http.MethodPost, "/"+url.PathEscape(id)+":"+action, nil); err != nil {
		y.ids.forgetOnNotFound(y.cacheKey(n), err)
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a RUNNING or already STARTING instance is left alone.
func (y *yandex) Start(ctx context.Context, n Target) (string, string, error) {
	return y.power(ctx, n, "RUNNING", "STARTING", "start")
}

// Stop is idempotent: a STOPPED or already STOPPING instance is left alone.
func (y *yandex) Stop(ctx context.Context, n Target) (string, string, error) {
	return y.power(ctx, n, "STOPPED", "STOPPING", "stop")
}
