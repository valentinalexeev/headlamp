package headscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var versionRe = regexp.MustCompile(`^v?[0-9][0-9A-Za-z.+-]{0,40}$`)

// maxVersionBody is the most of a /version answer that is read.
const maxVersionBody = 4096

// Version asks headscale for its own version: GET {BaseURL}/version, which needs no API key
// (#195). The answer is JSON with a "version" field (headscale 0.23 and later) or the plain
// version text.
func (c *Client) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	url := strings.TrimRight(c.BaseURL, "/") + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("headscale version: invalid request")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("headscale version: the server did not answer")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxVersionBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("headscale version: the server answered %d", resp.StatusCode)
	}
	v := strings.TrimSpace(string(body))
	var obj struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.Version != "" {
		v = strings.TrimSpace(obj.Version)
	}
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("headscale version: the answer has no version")
	}
	return v, nil
}
