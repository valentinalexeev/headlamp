package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Outcome names what went wrong with a failed push.
const (
	OutcomeUnreachable  = "unreachable"  // no connection / transport error
	OutcomeUnauthorized = "unauthorized" // 401/403: bearer token mismatch
	OutcomeConflict     = "conflict"     // 409: unconfirmed empty set refused
	OutcomeRejected     = "rejected"     // any other non-2xx, typically 503
)

var (
	putTimeout   = 10 * time.Second
	statsTimeout = 2 * time.Second // the admin page waits on it
)

// AgentUnavailable means the push did not take effect. Its message never carries payloads,
// credentials or the token.
type AgentUnavailable struct {
	Outcome string
	Status  int // HTTP status, 0 when there was no response
}

func (e *AgentUnavailable) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("agent answered %d", e.Status)
	}
	return "agent " + e.Outcome
}

// Agent is the ingress agent's API.
type Agent interface {
	PutCredentials(ctx context.Context, creds []Credential, confirmEmpty bool) error
	Stats(ctx context.Context) map[string]any
}

type agentClient struct {
	base    string
	resolve func(ctx context.Context) (string, error) // when set, the base URL is resolved on every call
	token   string
	hc      *http.Client
}

// baseURL is the agent's base URL for this call.
func (a *agentClient) baseURL(ctx context.Context) (string, error) {
	if a.resolve == nil {
		return a.base, nil
	}
	b, err := a.resolve(ctx)
	if err != nil || b == "" {
		return "", &AgentUnavailable{Outcome: OutcomeUnreachable}
	}
	return strings.TrimRight(b, "/"), nil
}

// NewAgentFunc is NewAgent with the base URL resolved on every call (the address of a discovered
// node can change). A resolve failure is an "unreachable" outcome; its text is never reported.
func NewAgentFunc(resolve func(ctx context.Context) (string, error), token string, hc *http.Client) Agent {
	if hc == nil {
		hc = newHTTPClient(nil)
	}
	return &agentClient{resolve: resolve, token: token, hc: hc}
}

// maxAgentBody caps how much of an agent response is read.
const maxAgentBody = 1 << 20

// newHTTPClient builds the agent client: it never follows redirects (a 3xx is a rejection, and
// the bearer token must not travel to another host) and ignores proxy environment variables.
func newHTTPClient(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil, DialContext: dial},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// NewAgent returns a client for the agent at baseURL. A nil hc means a plain client that does not
// follow redirects and ignores proxy environment variables.
func NewAgent(baseURL, token string, hc *http.Client) Agent {
	if hc == nil {
		hc = newHTTPClient(nil)
	}
	return &agentClient{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// HTTPClientFor builds an HTTP client that connects through dial (for example Tailnet.Dial).
func HTTPClientFor(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	return newHTTPClient(dial)
}

func (a *agentClient) PutCredentials(ctx context.Context, creds []Credential, confirmEmpty bool) error {
	if creds == nil {
		creds = []Credential{}
	}
	body, err := json.Marshal(map[string]any{"credentials": creds})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, putTimeout)
	defer cancel()
	base, err := a.baseURL(ctx)
	if err != nil {
		return err
	}
	url := base + "/v1/credentials"
	if confirmEmpty {
		url += "?confirm_empty=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return &AgentUnavailable{Outcome: OutcomeUnreachable}
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return &AgentUnavailable{Outcome: OutcomeUnreachable}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxAgentBody))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return errorFor(resp.StatusCode)
}

func errorFor(status int) *AgentUnavailable {
	switch status {
	case 401, 403:
		return &AgentUnavailable{Outcome: OutcomeUnauthorized, Status: status}
	case 409:
		return &AgentUnavailable{Outcome: OutcomeConflict, Status: status}
	}
	return &AgentUnavailable{Outcome: OutcomeRejected, Status: status}
}

// Stats returns per-device traffic keyed by opaque id; an empty non-nil map on any failure.
func (a *agentClient) Stats(ctx context.Context) map[string]any {
	empty := map[string]any{}
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	base, err := a.baseURL(ctx)
	if err != nil {
		return empty
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/stats", nil)
	if err != nil {
		return empty
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.hc.Do(req)
	if err != nil {
		return empty
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return empty
	}
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAgentBody)).Decode(&out); err != nil || out == nil {
		return empty
	}
	return out
}
