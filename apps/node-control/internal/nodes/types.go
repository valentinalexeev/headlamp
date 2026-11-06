package nodes

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotConfigured means a provider has no credentials or settings.
var ErrNotConfigured = errors.New("provider is not configured")

// APIError is an error answer from a cloud provider API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("provider API error %d: %s", e.Status, e.Message)
}

// Target is one controllable (or status-only) node.
type Target struct {
	ID          string // UI/API id
	Name        string // reported node name (the VM is located by it)
	TailnetName string // name used to match headscale
	Provider    string // "" = status-only
	Block       map[string]any
}

// Status is a provider status answer: "status" always, plus optionally
// "detail", "shape" and "machine_id".
type Status = map[string]any

// Provider starts, stops and inspects the VM behind a Target.
type Provider interface {
	Status(ctx context.Context, n Target) (Status, error)
	Start(ctx context.Context, n Target) (prev, cur string, err error)
	Stop(ctx context.Context, n Target) (prev, cur string, err error)
}
