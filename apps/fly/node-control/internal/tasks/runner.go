package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/scrub"
)

var (
	// ErrRunUnknown: the runner has no such run (never existed, or it
	// restarted and lost its table).
	ErrRunUnknown = errors.New("runner: unknown run")
	// ErrRunnerBusy: the runner already has an active run.
	ErrRunnerBusy = errors.New("runner: busy")
	// ErrRunnerRejected: the runner refused the request itself (422 or other 4xx).
	ErrRunnerRejected = errors.New("runner: request rejected")
	// ErrRunnerUnauthorized: the runner did not accept our bearer token.
	ErrRunnerUnauthorized = errors.New("runner: unauthorized")
)

const (
	runnerRequestTimeout = 10 * time.Second
	runnerMaxBody        = 64 << 10
)

// StartReq is what a node's runner needs to begin a run.
type StartReq struct {
	Script         string
	Params         map[string]any
	Secrets        map[string]string
	TimeoutSeconds int
}

// RunStatus is the runner's view of one run.
type RunStatus struct {
	State   string
	Result  map[string]any
	Error   string
	LogTail string
}

// Runner is the control side of a node's task-runner. addr is the node's
// tailnet IP.
type Runner interface {
	// Health reports whether the runner is reachable and idle.
	Health(ctx context.Context, addr string) (healthy bool, err error)
	Start(ctx context.Context, addr string, req StartReq) (id string, err error)
	Status(ctx context.Context, addr, id string) (RunStatus, error)
	Cancel(ctx context.Context, addr, id string) error
}

type runnerClient struct {
	hc      *http.Client
	port    string
	token   string
	secrets []string // token plus every secret value, scrubbed out of errors
}

// NewRunnerClient dials runners through tn on the given port. secrets are
// values that must never appear in an error; the token is always included.
func NewRunnerClient(tn core.Tailnet, port int, token string, secrets []string) Runner {
	return newRunnerClient(tn, port, token, secrets)
}

func newRunnerClient(tn core.Tailnet, port int, token string, secrets []string) *runnerClient {
	return &runnerClient{
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return tn.Dial(ctx, network, addr)
				},
				Proxy:             nil,
				DisableKeepAlives: true,
			},
			// A runner never redirects; following one would send the token elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		port:    fmt.Sprint(port),
		token:   token,
		secrets: append([]string{token}, secrets...),
	}
}

// scrubbedError carries text already cleaned of secrets while keeping the
// cause reachable for errors.Is (context deadlines, sentinels).
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

func (c *runnerClient) wrap(op string, err error) error {
	return &scrubbedError{msg: scrub.Scrub(fmt.Sprintf("runner %s: %v", op, err), c.secrets), err: err}
}

// do sends one request and returns the status and the (bounded) body.
func (c *runnerClient) do(ctx context.Context, op, method, addr, path string, body any) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, runnerRequestTimeout)
	defer cancel()

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, c.wrap(op, err)
		}
		rd = bytes.NewReader(b)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(addr, c.port), Path: path}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, c.wrap(op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// net/http's own text includes the URL; the scrub pass covers any secret in it.
		return 0, nil, c.wrap(op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, runnerMaxBody))
	if err != nil {
		return 0, nil, c.wrap(op, err)
	}
	return resp.StatusCode, data, nil
}

// statusErr maps an unexpected status to an error. The body is deliberately
// left out: the runner can echo script output there.
func (c *runnerClient) statusErr(op string, code int) error {
	switch {
	case code == http.StatusUnauthorized:
		return ErrRunnerUnauthorized
	case code >= 400 && code < 500:
		return fmt.Errorf("%w: %s: status %d", ErrRunnerRejected, op, code)
	}
	return fmt.Errorf("runner %s: status %d", op, code)
}

func (c *runnerClient) Health(ctx context.Context, addr string) (bool, error) {
	code, data, err := c.do(ctx, "health", http.MethodGet, addr, "/v1/health", nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return false, ErrRunnerUnauthorized
	default:
		return false, nil
	}
	var h struct {
		Busy bool `json:"busy"`
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return false, c.wrap("health", errors.New("undecodable answer"))
	}
	return !h.Busy, nil
}

func (c *runnerClient) Start(ctx context.Context, addr string, r StartReq) (string, error) {
	params := r.Params
	if params == nil {
		params = map[string]any{}
	}
	secrets := r.Secrets
	if secrets == nil {
		secrets = map[string]string{}
	}
	body := map[string]any{
		"script":          r.Script,
		"params":          params,
		"secrets":         secrets,
		"timeout_seconds": r.TimeoutSeconds,
	}
	code, data, err := c.do(ctx, "start", http.MethodPost, addr, "/v1/runs", body)
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
	case http.StatusConflict:
		return "", ErrRunnerBusy
	default:
		return "", c.statusErr("start", code)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.ID == "" {
		return "", c.wrap("start", errors.New("answer has no run id"))
	}
	return out.ID, nil
}

func (c *runnerClient) Status(ctx context.Context, addr, id string) (RunStatus, error) {
	code, data, err := c.do(ctx, "status", http.MethodGet, addr, "/v1/runs/"+url.PathEscape(id), nil)
	if err != nil {
		return RunStatus{}, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return RunStatus{}, ErrRunUnknown
	default:
		return RunStatus{}, c.statusErr("status", code)
	}
	var out struct {
		State   string         `json:"state"`
		Result  map[string]any `json:"result"`
		Error   string         `json:"error"`
		LogTail string         `json:"log_tail"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return RunStatus{}, c.wrap("status", errors.New("undecodable answer"))
	}
	switch out.State {
	case "running", "succeeded", "failed", "cancelled":
	default:
		return RunStatus{}, c.wrap("status", fmt.Errorf("unknown run state %q", out.State))
	}
	// Result, Error and LogTail come from the script and are the caller's to
	// scrub where they are stored; here they are passed through unchanged.
	return RunStatus{State: out.State, Result: out.Result, Error: out.Error, LogTail: out.LogTail}, nil
}

func (c *runnerClient) Cancel(ctx context.Context, addr, id string) error {
	code, _, err := c.do(ctx, "cancel", http.MethodDelete, addr, "/v1/runs/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrRunUnknown
	}
	return c.statusErr("cancel", code)
}
