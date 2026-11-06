package controlsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Source fetches the configuration. etag is the version already applied; when it is still current the
// source answers changed=false and no config.
type Source interface {
	Fetch(ctx context.Context, etag string) (cfg Config, changed bool, err error)
}

type fileSource struct{ path string }

// NewFileSource reads the same shape from a local file (standalone use, no node-control). The version is
// the hex SHA-256 of the file bytes, so an edit is picked up and an unchanged file is not re-applied.
func NewFileSource(path string) Source { return fileSource{path} }

func (f fileSource) Fetch(_ context.Context, etag string) (Config, bool, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return Config{}, false, errors.New("cannot read the configuration file")
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, MaxBody+1))
	if err != nil {
		return Config{}, false, errors.New("cannot read the configuration file")
	}
	if len(raw) > MaxBody {
		return Config{}, false, errors.New("configuration file is larger than 1 MiB")
	}
	sum := sha256.Sum256(raw)
	version := hex.EncodeToString(sum[:])
	if version == etag {
		return Config{}, false, nil
	}
	cfg, err := parse(raw)
	if err != nil {
		return Config{}, false, err
	}
	cfg.Version = version
	return cfg, true, nil
}

type nodeControlSource struct {
	base   string
	client *http.Client
}

// NewNodeControlSource pulls GET {baseURL}/ingress/v1/config with If-None-Match.
func NewNodeControlSource(baseURL string, client *http.Client) Source {
	return nodeControlSource{strings.TrimRight(baseURL, "/"), client}
}

func (n nodeControlSource) Fetch(ctx context.Context, etag string) (Config, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+"/ingress/v1/config", nil)
	if err != nil {
		return Config{}, false, errors.New("bad configuration URL")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return Config{}, false, fmt.Errorf("fetching the configuration: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return Config{}, false, nil
	case http.StatusOK:
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Config{}, false, fmt.Errorf("node-control answered %d to the configuration request", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return Config{}, false, fmt.Errorf("reading the configuration: %w", err)
	}
	if len(raw) > MaxBody {
		return Config{}, false, errors.New("configuration is larger than 1 MiB")
	}
	cfg, err := parse(raw)
	if err != nil {
		return Config{}, false, err
	}
	if cfg.Version == "" {
		cfg.Version = strings.Trim(resp.Header.Get("ETag"), `"`)
	}
	if cfg.Version == "" {
		return Config{}, false, errors.New("configuration carries no version")
	}
	return cfg, true, nil
}
