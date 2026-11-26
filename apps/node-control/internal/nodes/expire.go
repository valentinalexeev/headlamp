package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

// Expiring one tailnet node (#401). An admin removes the identity of a node that may be compromised: headscale
// expires the node, so it must register again, and the reusable pre-auth key it registered with (if it is still
// valid) is expired too, so the node can come back only with a new key. Deleting the node record stays in headscale.
//
//	GET  /nodes/tailnet.json            {"nodes": [{"id", "name", "tags", "online", "last_seen", "consequence"}]}
//	POST /nodes/tailnet/{hsid}/expire   {"name": "<the node's name>"}
//
// The name in the body must be the current name of the node with that id: a page that is out of date cannot expire
// a different node. Both routes need nodes admin (authz/routes.go); a tagged caller gets 403 there.

// Expirer is what expiring a node needs from headscale.
type Expirer interface {
	ListNodes(ctx context.Context) ([]headscale.Node, error)
	ExpireNode(ctx context.Context, id uint64) error
	ListPreAuthKeys(ctx context.Context) ([]headscale.PreAuthKey, error)
	ExpirePreAuthKey(ctx context.Context, id uint64) error
}

// WithExpirer turns on the "Expire node" action.
func WithExpirer(e Expirer) Option { return func(m *Module) { m.hs = e } }

const maxExpireBody = 4 << 10

// Tags that say what a node does, for the consequence of expiring it.
const (
	tagExit        = "tag:exit"
	tagBridge      = "tag:ingress"
	tagRunner      = "tag:task-runner"
	tagFunnel      = "tag:funnel-ingress"
	tagNodeControl = "tag:node-control"
)

const consequenceAll = "The node leaves the tailnet at once. It can join again only with a new one-time key " +
	"from \"Add a node\". Everything that goes through it stops, for example the WireGuard peers of a tailguard " +
	"node or the clients of a bridge."

// consequence says what stops when the node is expired, from its tags.
func consequence(n headscale.Node) string {
	var s string
	switch {
	case slices.Contains(n.Tags, tagNodeControl):
		s = "This is node-control: the console stops until it joins again. "
	case slices.Contains(n.Tags, tagBridge):
		s = "Clients of this bridge lose what it reaches over the tailnet, and node-control cannot send it " +
			"revocations and extensions. "
	case slices.Contains(n.Tags, tagExit):
		s = "Clients that use it as an exit node lose that route. "
	case slices.Contains(n.Tags, tagRunner):
		s = "Tasks and sessions on it stop. "
	case slices.Contains(n.Tags, tagFunnel):
		s = "The public names served through it stop. "
	case len(n.Tags) == 0:
		s = "This is a device of a person. "
	}
	return s + consequenceAll
}

type hsNodeView struct {
	ID          string     `json:"id"` // a string: a JavaScript number cannot hold every uint64
	Name        string     `json:"name"`
	Tags        []string   `json:"tags"`
	Online      bool       `json:"online"`
	LastSeen    *time.Time `json:"last_seen"`
	Consequence string     `json:"consequence"`
}

// nodeName is the name the console shows for a headscale node.
func nodeName(n headscale.Node) string {
	if n.GivenName != "" {
		return n.GivenName
	}
	return n.Name
}

func (m *Module) tailnetNodes(w http.ResponseWriter, req *http.Request) {
	ns, err := m.hs.ListNodes(req.Context())
	if errors.Is(err, headscale.ErrNotConfigured) {
		detail(w, 503, "the headscale API is not configured")
		return
	}
	if err != nil {
		detail(w, 502, err.Error())
		return
	}
	out := make([]hsNodeView, 0, len(ns))
	for _, n := range ns {
		tags := n.Tags
		if tags == nil {
			tags = []string{}
		}
		out = append(out, hsNodeView{ID: strconv.FormatUint(n.ID, 10), Name: nodeName(n), Tags: tags,
			Online: n.Online, LastSeen: n.LastSeen, Consequence: consequence(n)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, map[string]any{"nodes": out})
}

func (m *Module) expireNode(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseUint(req.PathValue("hsid"), 10, 64)
	if err != nil || id == 0 {
		detail(w, 404, "unknown tailnet node")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxExpireBody))
	if err != nil || json.Unmarshal(b, &body) != nil || body.Name == "" {
		detail(w, 422, "the body must be {\"name\": \"<the node's name>\"}")
		return
	}
	// The action must finish once started, even when the browser goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(req.Context()), 2*m.timeout)
	defer cancel()
	ns, err := m.hs.ListNodes(ctx)
	if err != nil {
		if errors.Is(err, headscale.ErrNotConfigured) {
			detail(w, 503, "the headscale API is not configured")
			return
		}
		detail(w, 502, err.Error())
		return
	}
	i := slices.IndexFunc(ns, func(n headscale.Node) bool { return n.ID == id })
	if i < 0 {
		detail(w, 404, "unknown tailnet node")
		return
	}
	n := ns[i]
	name := nodeName(n)
	if name != body.Name {
		detail(w, 409, "the node with this id is now named "+strconv.Quote(name)+": reload the page")
		return
	}
	label := name + " (id " + strconv.FormatUint(id, 10) + ")"
	if err := m.hs.ExpireNode(ctx, id); err != nil {
		m.record(req, "node.expire", label, "failed "+expireFailure(err))
		detail(w, 502, "headscale did not expire the node: "+err.Error())
		return
	}
	key, kerr := m.expireRegistrationKey(ctx, n)
	switch {
	case kerr != nil:
		m.record(req, "node.expire", label, "ok, key not expired "+expireFailure(kerr))
		writeJSON(w, 200, map[string]any{"ok": false, "node": name,
			"detail": "the node is expired, but its reusable registration key was not: expire key " +
				strconv.FormatUint(n.PreAuthKeyID, 10) + " in headscale"})
	case key != 0:
		m.record(req, "node.expire", label, "ok, key "+strconv.FormatUint(key, 10)+" expired")
		writeJSON(w, 200, map[string]any{"ok": true, "node": name, "expired_key": strconv.FormatUint(key, 10)})
	default:
		m.record(req, "node.expire", label, "ok")
		writeJSON(w, 200, map[string]any{"ok": true, "node": name})
	}
}

// expireRegistrationKey expires the pre-auth key n registered with when that key is reusable and still valid, so it
// cannot register the node again. It returns the id of the key it expired, or 0.
func (m *Module) expireRegistrationKey(ctx context.Context, n headscale.Node) (uint64, error) {
	if n.PreAuthKeyID == 0 {
		return 0, nil
	}
	keys, err := m.hs.ListPreAuthKeys(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now() // headscale's expiry is wall time
	for _, k := range keys {
		if k.ID == n.PreAuthKeyID && k.Reusable && (k.Expiration.IsZero() || k.Expiration.After(now)) {
			return k.ID, m.hs.ExpirePreAuthKey(ctx, k.ID)
		}
	}
	return 0, nil
}

// expireFailure names why a headscale call failed without copying its message.
func expireFailure(err error) string {
	switch {
	case errors.Is(err, headscale.ErrNotConfigured):
		return "(not_configured)"
	case errors.Is(err, context.DeadlineExceeded):
		return "(timeout)"
	default:
		return "(headscale_error)"
	}
}
