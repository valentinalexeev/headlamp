// Package headscale is a small read-only client for the headscale REST API.
package headscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	scrubpkg "example.com/node-control/internal/scrub"
)

// ErrNotConfigured is returned when no API key is available.
var ErrNotConfigured = errors.New("HEADSCALE_API_KEY is not set")

const requestTimeout = 10 * time.Second

// Node is one headscale node as seen by the API.
type Node struct {
	ID              uint64
	MachineKey      string // the node's machine public key ("mkey:..."), empty when the API omits it
	Name, GivenName string
	Online          bool
	LastSeen        *time.Time
	Tags            []string
	IPAddresses     []string
	ApprovedRoutes  []string
	AvailableRoutes []string
}

// UnmarshalJSON tolerates the variations between headscale versions: the id
// as a string or number, tags under several keys, null timestamps.
func (n *Node) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID              json.RawMessage `json:"id"`
		MachineKey      string          `json:"machineKey"`
		MachineKeySnake string          `json:"machine_key"`
		Name            string          `json:"name"`
		GivenName       string          `json:"givenName"`
		GivenNameSnake  string          `json:"given_name"`
		Online          bool            `json:"online"`
		LastSeen        *time.Time      `json:"lastSeen"`
		LastSeenSnake   *time.Time      `json:"last_seen"`
		IPAddresses     []string        `json:"ipAddresses"`
		IPAddrSnake     []string        `json:"ip_addresses"`
		ApprovedRoutes  []string        `json:"approvedRoutes"`
		ApprovedSnake   []string        `json:"approved_routes"`
		AvailableRoutes []string        `json:"availableRoutes"`
		AvailableSnake  []string        `json:"available_routes"`
		Tags            []string        `json:"tags"`
		ForcedTags      []string        `json:"forcedTags"`
		ForcedSnake     []string        `json:"forced_tags"`
		ValidTags       []string        `json:"validTags"`
		ValidSnake      []string        `json:"valid_tags"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	id, err := parseID(raw.ID)
	if err != nil {
		return err
	}
	*n = Node{
		ID:              id,
		MachineKey:      first(raw.MachineKey, raw.MachineKeySnake),
		Name:            raw.Name,
		GivenName:       first(raw.GivenName, raw.GivenNameSnake),
		Online:          raw.Online,
		LastSeen:        raw.LastSeen,
		IPAddresses:     append(raw.IPAddresses, raw.IPAddrSnake...),
		ApprovedRoutes:  append(raw.ApprovedRoutes, raw.ApprovedSnake...),
		AvailableRoutes: append(raw.AvailableRoutes, raw.AvailableSnake...),
		Tags:            mergeUnique(raw.Tags, raw.ForcedTags, raw.ForcedSnake, raw.ValidTags, raw.ValidSnake),
	}
	if n.LastSeen == nil {
		n.LastSeen = raw.LastSeenSnake
	}
	return nil
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseID(raw json.RawMessage) (uint64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, nil
	}
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid node id %q", s)
	}
	return id, nil
}

func mergeUnique(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// Client talks to a headscale server. The API key is never logged or
// included in returned errors.
type Client struct {
	BaseURL, APIKey string
	HTTP            *http.Client
}

// ListNodes calls GET {BaseURL}/api/v1/node.
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	if c.APIKey == "" {
		return nil, ErrNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/api/v1/node", nil)
	if err != nil {
		return nil, errors.New("listing nodes: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error carries only the URL, never headers; still, be explicit.
		return nil, fmt.Errorf("listing nodes: %s", scrubpkg.Scrub(err.Error(), []string{c.APIKey}))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %s", scrubpkg.Scrub(err.Error(), []string{c.APIKey}))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(body))
		return nil, fmt.Errorf("listing nodes: %d %s", resp.StatusCode, scrubpkg.Scrub(msg, []string{c.APIKey}))
	}
	var out struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("listing nodes: decoding response: %s", scrubpkg.Scrub(err.Error(), []string{c.APIKey}))
	}
	return out.Nodes, nil
}

// NodeSource yields the current node list.
type NodeSource interface {
	Nodes(ctx context.Context) ([]Node, error)
}

type lister interface {
	ListNodes(ctx context.Context) ([]Node, error)
}

// DefaultTTL is the cache lifetime used by callers that have no better value.
const DefaultTTL = 15 * time.Second

// Cached wraps c with a TTL cache; concurrent callers share one fetch.
func Cached(c *Client, ttl time.Duration) NodeSource { return cached(c, ttl) }

type cache struct {
	src lister
	ttl time.Duration

	mu    sync.Mutex
	at    time.Time
	nodes []Node
}

func cached(src lister, ttl time.Duration) *cache { return &cache{src: src, ttl: ttl} }

// Nodes returns the cached list, refetching when it is older than the TTL.
// Failed fetches are not cached. The lock is held across the fetch so
// concurrent callers wait for, and share, a single request.
func (c *cache) Nodes(ctx context.Context) ([]Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < c.ttl {
		return c.nodes, nil
	}
	nodes, err := c.src.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	c.nodes, c.at = nodes, time.Now()
	return nodes, nil
}
