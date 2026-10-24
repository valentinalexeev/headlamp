package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"example.com/node-control/internal/core"
	scrubpkg "example.com/node-control/internal/scrub"
)

const (
	selectelAuthURL = "https://cloud.api.selcloud.ru/identity/v3"
	selectelTimeout = 20 * time.Second
	// A token lives 3000s from issue in our books and is replaced once less
	// than tokenSlack remains.
	selectelTokenTTL = 3000 * time.Second
	selectelSlack    = 300 * time.Second
	maxErrBody       = 512
)

var selectelStatus = map[string]string{
	"ACTIVE": "RUNNING", "SHUTOFF": "STOPPED", "BUILD": "STARTING", "HARD_REBOOT": "STARTING",
}

type selectelToken struct {
	token   string
	compute string
	expires time.Time
}

type selectel struct {
	env     core.Env
	hc      *http.Client
	authURL string
	now     func() time.Time
	ids     idCache // name -> instance id for idCacheTTL

	mu     sync.Mutex
	tokens map[string]selectelToken // by region
}

// NewSelectel returns the Selectel Cloud (OpenStack) provider. The target's
// block is {"region": ..., "instance_name": ..., "instance_id": ...}, all
// optional: the instance is found by name (the id is cached for 15 s) (default
// Target.Name) unless instance_id pins one; region falls back to
// SELECTEL_REGION. Credentials: SELECTEL_ACCOUNT_ID (Keystone domain),
// SELECTEL_PROJECT_ID, SELECTEL_USER, SELECTEL_PASSWORD. The Keystone URL
// can be overridden with SELECTEL_AUTH_URL.
func NewSelectel(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &selectel{
		env:     env,
		hc:      hc,
		authURL: strings.TrimRight(env.Default("SELECTEL_AUTH_URL", selectelAuthURL), "/"),
		now:     time.Now,
		tokens:  map[string]selectelToken{},
	}
}

func (s *selectel) block(n Target) (region, name, id string) {
	b := n.Block
	region, _ = b["region"].(string)
	name, _ = b["instance_name"].(string)
	id, _ = b["instance_id"].(string)
	if region == "" {
		region = s.env.Getenv("SELECTEL_REGION")
	}
	if name == "" {
		name = n.Name
	}
	return
}

func (s *selectel) require(name string) (string, error) {
	v := s.env.Getenv(name)
	if v == "" {
		return "", notConfigured(name + " is not set")
	}
	return v, nil
}

// secrets lists values that must never reach error text.
func (s *selectel) secrets(extra ...string) []string {
	out := append([]string{}, extra...)
	return append(out, s.env.Getenv("SELECTEL_PASSWORD"))
}

// scrub removes secrets from text and bounds its length (shared by every provider).
func scrub(text string, secrets []string) string { return scrubpkg.Scrub(text, secrets) }

// auth returns the token and compute endpoint for the region; the token is
// cached until shortly before expiry.
func (s *selectel) auth(ctx context.Context, region string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.tokens[region]; ok && c.expires.Sub(s.now()) > selectelSlack {
		return c.token, c.compute, nil
	}
	user, err := s.require("SELECTEL_USER")
	if err != nil {
		return "", "", err
	}
	domain, err := s.require("SELECTEL_ACCOUNT_ID")
	if err != nil {
		return "", "", err
	}
	password, err := s.require("SELECTEL_PASSWORD")
	if err != nil {
		return "", "", err
	}
	project, err := s.require("SELECTEL_PROJECT_ID")
	if err != nil {
		return "", "", err
	}
	payload, _ := json.Marshal(map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{"user": map[string]any{
			"name": user, "domain": map[string]string{"name": domain}, "password": password,
		}}},
		"scope": map[string]any{"project": map[string]string{"id": project}},
	}})
	ctx, cancel := context.WithTimeout(ctx, selectelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.authURL+"/auth/tokens", bytes.NewReader(payload))
	if err != nil {
		return "", "", &APIError{Message: "keystone auth failed: " + scrub(err.Error(), s.secrets())}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", "", &APIError{Message: "keystone auth failed: " + scrub(err.Error(), s.secrets())}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", &APIError{Status: resp.StatusCode, Message: "keystone auth failed: " + scrub(err.Error(), s.secrets())}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("keystone auth failed: %d %s", resp.StatusCode, scrub(string(b), s.secrets()))}
	}
	token := resp.Header.Get("X-Subject-Token")
	var doc struct {
		Token struct {
			Catalog []struct {
				Type      string `json:"type"`
				Endpoints []struct {
					Interface string `json:"interface"`
					Region    string `json:"region"`
					URL       string `json:"url"`
				} `json:"endpoints"`
			} `json:"catalog"`
		} `json:"token"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || token == "" {
		return "", "", &APIError{Status: resp.StatusCode, Message: "keystone auth failed: bad response"}
	}
	compute := ""
	for _, svc := range doc.Token.Catalog {
		if svc.Type != "compute" {
			continue
		}
		for _, ep := range svc.Endpoints {
			if ep.Interface == "public" && ep.Region == region && compute == "" {
				compute = strings.TrimRight(ep.URL, "/")
			}
		}
	}
	if compute == "" {
		return "", "", &APIError{Message: fmt.Sprintf("no public compute endpoint for region '%s' in the catalog", scrub(region, nil))}
	}
	s.tokens[region] = selectelToken{token: token, compute: compute, expires: s.now().Add(selectelTokenTTL)}
	return token, compute, nil
}

// call performs an authenticated compute request; non-2xx becomes an APIError.
func (s *selectel) call(ctx context.Context, region, method, path string, query url.Values, body any) ([]byte, error) {
	token, compute, err := s.auth(ctx, region)
	if err != nil {
		return nil, err
	}
	secrets := s.secrets(token)
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	u := compute + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, selectelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	req.Header.Set("X-Auth-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &APIError{Status: resp.StatusCode, Message: scrub(err.Error(), secrets)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("%s %s: %d %s", method, path, resp.StatusCode, scrub(string(b), secrets))}
	}
	return b, nil
}

// resolve returns the region and instance id for the target.
func (s *selectel) resolve(ctx context.Context, n Target) (string, string, error) {
	region, name, id := s.block(n)
	if id == "" && name == "" {
		return "", "", notConfigured(`node needs a name, or "selectel": {"instance_name": ...} or {"instance_id": ...}`)
	}
	if region == "" {
		return "", "", notConfigured(`set "selectel": {"region": ...} (or the SELECTEL_REGION env var)`)
	}
	if id != "" {
		return region, id, nil
	}
	key := s.cacheKey(n)
	if cached, ok := s.ids.get(key, s.now()); ok {
		return region, cached, nil
	}
	// Nova matches `name` as a regexp; anchor it so "a" never matches "a2".
	b, err := s.call(ctx, region, http.MethodGet, "/servers", url.Values{"name": {"^" + regexp.QuoteMeta(name) + "$"}}, nil)
	if err != nil {
		return "", "", err
	}
	var doc struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", "", &APIError{Message: "listing instances: bad response"}
	}
	switch len(doc.Servers) {
	case 0:
		return "", "", &APIError{Message: fmt.Sprintf("no instance named '%s' in the project", scrub(name, nil))}
	case 1:
		s.ids.put(key, doc.Servers[0].ID, s.now())
		return region, doc.Servers[0].ID, nil
	}
	return "", "", &APIError{Message: fmt.Sprintf("%d instances named '%s'; refusing to guess", len(doc.Servers), scrub(name, nil))}
}

// cacheKey identifies a name lookup; the pinned-id case never uses it.
func (s *selectel) cacheKey(n Target) string {
	region, name, _ := s.block(n)
	return region + "|" + name
}

func (s *selectel) rawStatus(ctx context.Context, region, id string) (string, error) {
	b, err := s.call(ctx, region, http.MethodGet, "/servers/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return "", err
	}
	var doc struct {
		Server struct {
			Status string `json:"status"`
		} `json:"server"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Server.Status == "" {
		return "", &APIError{Message: "reading instance: bad response"}
	}
	return doc.Server.Status, nil
}

func normStatus(raw string) string {
	if v, ok := selectelStatus[raw]; ok {
		return v
	}
	return raw
}

func (s *selectel) Status(ctx context.Context, n Target) (Status, error) {
	region, id, err := s.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	raw, err := s.rawStatus(ctx, region, id)
	if err != nil {
		s.ids.forgetOnNotFound(s.cacheKey(n), err)
		return nil, err
	}
	return Status{"status": normStatus(raw)}, nil
}

func (s *selectel) power(ctx context.Context, n Target, want, action, transient string) (string, string, error) {
	region, id, err := s.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	raw, err := s.rawStatus(ctx, region, id)
	if err != nil {
		s.ids.forgetOnNotFound(s.cacheKey(n), err)
		return "", "", err
	}
	prev := normStatus(raw)
	if prev == want {
		return prev, prev, nil
	}
	if _, err := s.call(ctx, region, http.MethodPost, "/servers/"+url.PathEscape(id)+"/action", nil, map[string]any{action: nil}); err != nil {
		s.ids.forgetOnNotFound(s.cacheKey(n), err)
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a running instance is left alone.
func (s *selectel) Start(ctx context.Context, n Target) (string, string, error) {
	return s.power(ctx, n, "RUNNING", "os-start", "STARTING")
}

// Stop is idempotent: a stopped instance is left alone.
func (s *selectel) Stop(ctx context.Context, n Target) (string, string, error) {
	return s.power(ctx, n, "STOPPED", "os-stop", "STOPPING")
}
