package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Registrar publishes and withdraws services with funnel-ingress.
type Registrar interface {
	Put(ctx context.Context, ns, service string, port int) error
	Delete(ctx context.Context, ns, service string) error
}

// APIError is a non-success answer from the registration API.
type APIError struct{ Status int }

func (e *APIError) Error() string { return fmt.Sprintf("registration API answered %d", e.Status) }

type httpRegistrar struct {
	base   string
	client *http.Client
}

// NewHTTPRegistrar talks to the registration API at baseURL. The request carries only the namespace, the
// service and the port: the ingress takes the backend address from the caller's tailnet identity.
func NewHTTPRegistrar(baseURL string, client *http.Client) Registrar {
	return httpRegistrar{strings.TrimRight(baseURL, "/"), client}
}

func (h httpRegistrar) Put(ctx context.Context, ns, service string, port int) error {
	body, _ := json.Marshal(struct {
		NS   string `json:"ns"`
		Port int    `json:"port"`
	}{ns, port})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.base+"/v1/routes/"+url.PathEscape(service), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return h.do(req)
}

func (h httpRegistrar) Delete(ctx context.Context, ns, service string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.base+"/v1/routes/"+url.PathEscape(service)+"?ns="+url.QueryEscape(ns), nil)
	if err != nil {
		return err
	}
	return h.do(req)
}

func (h httpRegistrar) do(req *http.Request) error {
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return &APIError{Status: resp.StatusCode}
}
