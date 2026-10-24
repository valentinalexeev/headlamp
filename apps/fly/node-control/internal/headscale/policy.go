package headscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	scrubpkg "example.com/node-control/internal/scrub"
)

const (
	maxPolicyBody = 4 << 20
	maxBody       = 1 << 20
	maxErrMessage = 2 << 10
)

// APIError is a 4xx answer from headscale (including 401 and 403); Message
// is the server's own error text. It never contains the API key.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("headscale API %d: %s", e.Status, e.Message) }

// call sends one authenticated JSON request and returns the response body of
// a 2xx answer. op names the operation in errors ("setting the policy").
func (c *Client) call(ctx context.Context, op, method, path string, in any, limit int64) ([]byte, error) {
	if c.APIKey == "" {
		return nil, ErrNotConfigured
	}
	secrets := []string{c.APIKey}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if in != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(in); err != nil {
			return nil, fmt.Errorf("%s: encoding request", op)
		}
		rd = bytes.NewReader(bytes.TrimRight(buf.Bytes(), "\n"))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid request", op)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", op, scrubpkg.Scrub(err.Error(), secrets))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("%s: %s", op, scrubpkg.Scrub(err.Error(), secrets))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return body, nil
	case resp.StatusCode >= 400 && resp.StatusCode <= 499:
		return nil, &APIError{Status: resp.StatusCode, Message: errMessage(resp, body, secrets)}
	default:
		// The body is deliberately left out: only the status is reported.
		return nil, fmt.Errorf("%s: unexpected status %d", op, resp.StatusCode)
	}
}

// errMessage extracts the grpc-gateway "message" field, falling back to the
// trimmed body, then to the status text.
func errMessage(resp *http.Response, body []byte, secrets []string) string {
	var st struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &st) == nil {
		msg = strings.TrimSpace(st.Message)
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	msg = scrubpkg.Scrub(msg, secrets)
	if len(msg) > maxErrMessage {
		msg = strings.ToValidUTF8(msg[:maxErrMessage], "")
	}
	return msg
}

func decode(op string, body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding response: %v", op, err)
	}
	return nil
}

// GetPolicy calls GET /api/v1/policy and returns the policy text and the
// time headscale last updated it.
func (c *Client) GetPolicy(ctx context.Context) (text, updatedAt string, err error) {
	body, err := c.call(ctx, "reading the policy", http.MethodGet, "/api/v1/policy", nil, maxPolicyBody)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Policy    string `json:"policy"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := decode("reading the policy", body, &out); err != nil {
		return "", "", err
	}
	return out.Policy, out.UpdatedAt, nil
}

// SetPolicy calls PUT /api/v1/policy. A rejected policy is an *APIError.
func (c *Client) SetPolicy(ctx context.Context, text string) error {
	_, err := c.call(ctx, "setting the policy", http.MethodPut, "/api/v1/policy",
		map[string]string{"policy": text}, maxPolicyBody)
	return err
}

// CheckPolicy calls POST /api/v1/policy/check, which validates without applying.
func (c *Client) CheckPolicy(ctx context.Context, text string) error {
	_, err := c.call(ctx, "checking the policy", http.MethodPost, "/api/v1/policy/check",
		map[string]string{"policy": text}, maxPolicyBody)
	return err
}
