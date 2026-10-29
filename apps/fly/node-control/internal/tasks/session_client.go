package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"example.com/node-control/internal/core"
)

// SessionInfo is the runner's view of one interactive browser session.
type SessionInfo struct {
	ID        string
	State     string // "running" or "ended"
	Reason    string // why it ended: ttl, idle, stopped or failed
	Port      int
	StartedAt time.Time
	ExpiresAt time.Time
}

// SessionClient is the control side of a node's session endpoints. addr is the
// node's tailnet IP. It is separate from Runner so Runner's fakes stay as they are.
type SessionClient interface {
	// Current returns the node's session, or nil when it has none.
	Current(ctx context.Context, addr string) (*SessionInfo, error)
	Start(ctx context.Context, addr string, ttl, idle int) (SessionInfo, error)
	Stop(ctx context.Context, addr, id string) error
}

// sessionClient reuses the runner client's dialing, token and scrubbing. It is
// its own type because runnerClient already has a Start for runs.
type sessionClient struct{ *runnerClient }

// NewSessionClient dials runners through tn on the given port with the same
// token the run client uses.
func NewSessionClient(tn core.Tailnet, port int, token string) SessionClient {
	return &sessionClient{newRunnerClient(tn, port, token, nil)}
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

type sessionWire struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Port      int       `json:"port"`
	Reason    string    `json:"reason"`
}

func (c *sessionClient) Current(ctx context.Context, addr string) (*SessionInfo, error) {
	code, data, err := c.do(ctx, "session status", http.MethodGet, addr, "/v1/sessions", nil)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, c.wrap("session status", errors.New("sessions are not enabled on this node"))
	default:
		return nil, c.statusErr("session status", code)
	}
	var out struct {
		Session *sessionWire `json:"session"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, c.wrap("session status", errors.New("undecodable answer"))
	}
	s := out.Session
	if s == nil {
		return nil, nil
	}
	if s.ID == "" || (s.State != "running" && s.State != "ended") || (s.State == "running" && !validPort(s.Port)) {
		return nil, c.wrap("session status", errors.New("invalid session in answer"))
	}
	return &SessionInfo{ID: s.ID, State: s.State, Reason: s.Reason, Port: s.Port, StartedAt: s.StartedAt, ExpiresAt: s.ExpiresAt}, nil
}

func (c *sessionClient) Start(ctx context.Context, addr string, ttl, idle int) (SessionInfo, error) {
	body := map[string]int{"ttl_seconds": ttl, "idle_seconds": idle}
	code, data, err := c.do(ctx, "session start", http.MethodPost, addr, "/v1/sessions", body)
	if err != nil {
		return SessionInfo{}, err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
	case http.StatusConflict:
		return SessionInfo{}, ErrRunnerBusy
	case http.StatusNotFound:
		return SessionInfo{}, ErrRunUnknown
	default:
		return SessionInfo{}, c.statusErr("session start", code)
	}
	var out sessionWire
	if err := json.Unmarshal(data, &out); err != nil || out.ID == "" || !validPort(out.Port) {
		return SessionInfo{}, c.wrap("session start", errors.New("answer has no usable session"))
	}
	return SessionInfo{ID: out.ID, State: "running", Port: out.Port, ExpiresAt: out.ExpiresAt}, nil
}

func (c *sessionClient) Stop(ctx context.Context, addr, id string) error {
	code, _, err := c.do(ctx, "session stop", http.MethodDelete, addr, "/v1/sessions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrRunUnknown
	}
	return c.statusErr("session stop", code)
}
