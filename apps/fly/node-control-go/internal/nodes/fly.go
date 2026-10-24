package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"example.com/node-control/internal/core"
)

const (
	flyAPIBase = "https://api.machines.dev/v1"
	flyTimeout = 10 * time.Second
)

// notConfigured is a not-configured error with a specific message that still
// matches ErrNotConfigured.
type notConfigured string

func (e notConfigured) Error() string        { return string(e) }
func (e notConfigured) Is(target error) bool { return target == ErrNotConfigured }

type fly struct {
	env  core.Env
	hc   *http.Client
	base string
}

// NewFly returns the Fly Machines provider. The target's connection block is
// {"app": "<fly app>"}; the app defaults to Target.Name. The app is expected
// to run one machine, resolved on every call. Credentials: FLY_API_TOKEN.
func NewFly(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &fly{env: env, hc: hc, base: flyAPIBase}
}

type flyMachine struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func (f *fly) app(n Target) (string, error) {
	if a, ok := n.Block["app"].(string); ok && a != "" {
		return a, nil
	}
	if n.Name != "" {
		return n.Name, nil
	}
	return "", notConfigured(`node config needs "fly": {"app": ...}`)
}

func (f *fly) do(ctx context.Context, method, path string) ([]byte, int, error) {
	token := f.env.Getenv("FLY_API_TOKEN")
	if token == "" {
		return nil, 0, notConfigured("FLY_API_TOKEN is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, flyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, f.base+path, nil)
	if err != nil {
		return nil, 0, &APIError{Message: scrub(err.Error(), []string{f.env.Getenv("FLY_API_TOKEN")})}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, 0, &APIError{Message: scrub(err.Error(), []string{token})}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, &APIError{Message: scrub(err.Error(), []string{token})}
	}
	return b, resp.StatusCode, nil
}

func (f *fly) primary(ctx context.Context, app string) (flyMachine, error) {
	b, code, err := f.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(app)+"/machines")
	if err != nil {
		return flyMachine{}, err
	}
	if code < 200 || code >= 300 {
		return flyMachine{}, &APIError{Status: code, Message: fmt.Sprintf("listing machines: %d %s", code, scrub(string(b), []string{f.env.Getenv("FLY_API_TOKEN")}))}
	}
	var all []flyMachine
	if err := json.Unmarshal(b, &all); err != nil {
		return flyMachine{}, &APIError{Status: code, Message: "listing machines: bad response: " + err.Error()}
	}
	for _, m := range all {
		if m.State != "destroyed" {
			if m.State == "" {
				m.State = "unknown"
			}
			return m, nil
		}
	}
	return flyMachine{}, &APIError{Message: fmt.Sprintf("no machines found for app '%s'", app)}
}

func (f *fly) Status(ctx context.Context, n Target) (Status, error) {
	app, err := f.app(n)
	if err != nil {
		return nil, err
	}
	m, err := f.primary(ctx, app)
	if err != nil {
		return nil, err
	}
	return Status{"status": m.State, "machine_id": m.ID}, nil
}

func (f *fly) act(ctx context.Context, n Target, action, done, transient string) (string, string, error) {
	app, err := f.app(n)
	if err != nil {
		return "", "", err
	}
	m, err := f.primary(ctx, app)
	if err != nil {
		return "", "", err
	}
	if m.State == done {
		return m.State, m.State, nil
	}
	b, code, err := f.do(ctx, http.MethodPost, "/apps/"+url.PathEscape(app)+"/machines/"+url.PathEscape(m.ID)+"/"+action)
	if err != nil {
		return "", "", err
	}
	if code < 200 || code >= 300 {
		return "", "", &APIError{Status: code, Message: fmt.Sprintf("%s machine: %d %s", action, code, scrub(string(b), []string{f.env.Getenv("FLY_API_TOKEN")}))}
	}
	return m.State, transient, nil
}

// Start is idempotent: an already started machine is left alone.
func (f *fly) Start(ctx context.Context, n Target) (string, string, error) {
	return f.act(ctx, n, "start", "started", "starting")
}

// Stop is idempotent: an already stopped machine is left alone.
func (f *fly) Stop(ctx context.Context, n Target) (string, string, error) {
	return f.act(ctx, n, "stop", "stopped", "stopping")
}
