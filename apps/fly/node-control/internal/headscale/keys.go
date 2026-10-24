package headscale

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// User is a headscale user.
type User struct {
	ID   uint64
	Name string
}

// PreAuthKey is a headscale pre-auth key. Key is set only by CreatePreAuthKey.
type PreAuthKey struct {
	ID                        uint64
	Key                       string
	UserID                    uint64
	Reusable, Ephemeral, Used bool
	Expiration                time.Time
	ACLTags                   []string
}

type apiUser struct {
	ID   json.RawMessage `json:"id"`
	Name string          `json:"name"`
}

type apiKey struct {
	ID         json.RawMessage `json:"id"`
	Key        string          `json:"key"`
	User       apiUser         `json:"user"`
	Reusable   bool            `json:"reusable"`
	Ephemeral  bool            `json:"ephemeral"`
	Used       bool            `json:"used"`
	Expiration time.Time       `json:"expiration"`
	ACLTags    []string        `json:"aclTags"`
}

func (k apiKey) convert() (PreAuthKey, error) {
	id, err := parseID(k.ID)
	if err != nil {
		return PreAuthKey{}, err
	}
	uid, err := parseID(k.User.ID)
	if err != nil {
		return PreAuthKey{}, err
	}
	return PreAuthKey{ID: id, Key: k.Key, UserID: uid, Reusable: k.Reusable, Ephemeral: k.Ephemeral,
		Used: k.Used, Expiration: k.Expiration, ACLTags: k.ACLTags}, nil
}

// ListUsers calls GET /api/v1/user.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	const op = "listing users"
	body, err := c.call(ctx, op, http.MethodGet, "/api/v1/user", nil, maxBody)
	if err != nil {
		return nil, err
	}
	var out struct {
		Users []apiUser `json:"users"`
	}
	if err := decode(op, body, &out); err != nil {
		return nil, err
	}
	users := make([]User, 0, len(out.Users))
	for _, u := range out.Users {
		id, err := parseID(u.ID)
		if err != nil {
			return nil, err
		}
		users = append(users, User{ID: id, Name: u.Name})
	}
	return users, nil
}

// CreatePreAuthKey calls POST /api/v1/preauthkey. The returned Key is the only
// time the secret is visible.
func (c *Client) CreatePreAuthKey(ctx context.Context, userID uint64, tags []string, ttl time.Duration, ephemeral, reusable bool) (PreAuthKey, error) {
	const op = "creating a pre-auth key"
	req := map[string]any{
		"user":       uintString(userID),
		"reusable":   reusable,
		"ephemeral":  ephemeral,
		"expiration": time.Now().Add(ttl).UTC().Format(time.RFC3339),
		"aclTags":    tags,
	}
	body, err := c.call(ctx, op, http.MethodPost, "/api/v1/preauthkey", req, maxBody)
	if err != nil {
		return PreAuthKey{}, err
	}
	var out struct {
		PreAuthKey apiKey `json:"preAuthKey"`
	}
	if err := decode(op, body, &out); err != nil {
		return PreAuthKey{}, err
	}
	return out.PreAuthKey.convert()
}

// ListPreAuthKeys calls GET /api/v1/preauthkey. Key is always blank.
func (c *Client) ListPreAuthKeys(ctx context.Context) ([]PreAuthKey, error) {
	const op = "listing pre-auth keys"
	body, err := c.call(ctx, op, http.MethodGet, "/api/v1/preauthkey", nil, maxBody)
	if err != nil {
		return nil, err
	}
	var out struct {
		Keys []apiKey `json:"preAuthKeys"`
	}
	if err := decode(op, body, &out); err != nil {
		return nil, err
	}
	keys := make([]PreAuthKey, 0, len(out.Keys))
	for _, k := range out.Keys {
		pk, err := k.convert()
		if err != nil {
			return nil, err
		}
		pk.Key = ""
		keys = append(keys, pk)
	}
	return keys, nil
}

// ExpirePreAuthKey calls POST /api/v1/preauthkey/expire.
func (c *Client) ExpirePreAuthKey(ctx context.Context, id uint64) error {
	_, err := c.call(ctx, "expiring a pre-auth key", http.MethodPost, "/api/v1/preauthkey/expire",
		map[string]string{"id": uintString(id)}, maxBody)
	return err
}

func uintString(n uint64) string { return strconv.FormatUint(n, 10) }
