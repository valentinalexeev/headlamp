package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

// Egress is the bridge path self-test (#196): one HTTPS request through the SOCKS port of the
// bridge's exit node (the via-exit outbound of xray). Every component can look healthy while this
// path is broken, for example when the exit node of the SOCKS node is offline, or when the
// headscale policy has no autogroup:internet for the bridge (#259). Then clients connect and get
// no reply.
type Egress struct {
	Socks    string        // host:port of the SOCKS5 listener (the tailscale-userspace-socks node)
	URL      string        // https URL to request through it
	Interval time.Duration // between probes; default 1 minute
	Timeout  time.Duration // of one probe; default 15 s
	// Dial is the dialer of the SOCKS connection (tests). nil: net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS is the TLS config of the request through the tunnel. nil: the system roots.
	TLS *tls.Config
	Now func() time.Time

	mu   sync.Mutex
	last EgressResult
}

// EgressResult is the answer of GET /v1/egress. Error is a short class, never a URL with secrets.
type EgressResult struct {
	OK       bool   `json:"ok"`
	Checked  int64  `json:"checked"`            // unix seconds of the last probe; 0: not yet
	Since    int64  `json:"since,omitempty"`    // unix seconds since the current state holds
	Error    string `json:"error,omitempty"`    // why the last probe failed
	Duration int64  `json:"duration_ms"`        // of the last probe
	Status   int    `json:"status,omitempty"`   // HTTP status of the last answer
	Target   string `json:"target,omitempty"`   // host of the probe URL
	Disabled bool   `json:"disabled,omitempty"` // no probe configured
}

const (
	defaultEgressURL      = "https://www.gstatic.com/generate_204"
	defaultEgressInterval = time.Minute
	defaultEgressTimeout  = 15 * time.Second
	statusLineMax         = 64 // bytes read for "HTTP/1.1 204 ..."
	httpServerError       = 500
	httpsPort             = 443
)

// SOCKS5 (RFC 1928) values used by the probe.
const (
	socksVersion     = 5
	socksNoAuth      = 0
	socksCmdConnect  = 1
	socksAtypIPv4    = 1
	socksAtypDomain  = 3
	socksAtypIPv6    = 4
	socksReplyOK     = 0
	socksPortLen     = 2
	socksMaxHostLen  = 255
	socksReplyHeader = 4
)

// LoadEgress reads INGRESS_EGRESS_SOCKS, INGRESS_EGRESS_URL and INGRESS_EGRESS_CA. An empty SOCKS
// address turns the probe off.
func LoadEgress(getenv func(string) string) (*Egress, error) {
	socks := getenv("INGRESS_EGRESS_SOCKS")
	if socks == "" {
		return nil, nil
	}
	if _, _, err := net.SplitHostPort(socks); err != nil {
		return nil, &InfoConfigError{Var: "INGRESS_EGRESS_SOCKS"}
	}
	raw := getenv("INGRESS_EGRESS_URL")
	if raw == "" {
		raw = defaultEgressURL
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil {
		return nil, &InfoConfigError{Var: "INGRESS_EGRESS_URL"}
	}
	e := &Egress{Socks: socks, URL: raw}
	// INGRESS_EGRESS_CA (tests only): a PEM file of the CA of the probe URL. Production uses the
	// system roots.
	if f := getenv("INGRESS_EGRESS_CA"); f != "" {
		pem, err := os.ReadFile(f) //nolint:gosec // a path the operator sets in the env file
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, &InfoConfigError{Var: "INGRESS_EGRESS_CA"}
		}
		e.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return e, nil
}

func (e *Egress) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Result is the last probe.
func (e *Egress) Result() EgressResult {
	if e == nil {
		return EgressResult{Disabled: true}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

// Probe runs one probe and records it.
func (e *Egress) Probe(ctx context.Context) EgressResult {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultEgressTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := e.now()
	status, err := e.fetch(ctx)
	u, _ := url.Parse(e.URL)
	r := EgressResult{OK: err == nil && status > 0 && status < httpServerError,
		Checked: e.now().Unix(), Duration: e.now().Sub(start).Milliseconds(), Status: status,
		Target: u.Hostname()}
	if err != nil {
		r.Error = err.Error()
	} else if !r.OK {
		r.Error = "HTTP " + strconv.Itoa(status)
	}
	e.mu.Lock()
	r.Since = e.last.Since
	if e.last.Checked == 0 || e.last.OK != r.OK {
		r.Since = r.Checked
	}
	e.last = r
	e.mu.Unlock()
	return r
}

// Loop probes at once, then every Interval, until ctx ends.
func (e *Egress) Loop(ctx context.Context) {
	iv := e.Interval
	if iv <= 0 {
		iv = defaultEgressInterval
	}
	for {
		e.Probe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
		}
	}
}

// fetch does a SOCKS5 CONNECT by host name (the exit node resolves it, as for clients), TLS, then
// a HEAD request. It returns the HTTP status.
func (e *Egress) fetch(ctx context.Context) (int, error) {
	u, err := url.Parse(e.URL)
	if err != nil {
		return 0, errors.New("bad URL")
	}
	port := httpsPort
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	dial := e.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	c, err := dial(ctx, "tcp", e.Socks)
	if err != nil {
		return 0, errors.New("SOCKS port unreachable")
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := socksConnect(c, u.Hostname(), port); err != nil {
		return 0, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if e.TLS != nil {
		cfg = e.TLS.Clone()
	}
	cfg.ServerName = u.Hostname()
	t := tls.Client(c, cfg)
	if err := t.HandshakeContext(ctx); err != nil {
		return 0, errors.New("TLS through the exit node failed")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	req := "HEAD " + path + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nConnection: close\r\nUser-Agent: ingress-agent-egress\r\n\r\n"
	if _, err := t.Write([]byte(req)); err != nil {
		return 0, errors.New("no answer through the exit node")
	}
	buf := make([]byte, statusLineMax)
	n, _ := t.Read(buf)
	var proto string
	var status int
	if _, err := fmt.Sscanf(string(buf[:n]), "%s %d", &proto, &status); err != nil || status == 0 {
		return 0, errors.New("no HTTP answer through the exit node")
	}
	return status, nil
}

// socksConnect is a SOCKS5 CONNECT without auth, by domain name.
func socksConnect(c net.Conn, host string, port int) error {
	if len(host) > socksMaxHostLen || port <= 0 || port > math.MaxUint16 {
		return errors.New("bad probe target")
	}
	if _, err := c.Write([]byte{socksVersion, 1, socksNoAuth}); err != nil {
		return errors.New("SOCKS port unreachable")
	}
	r := make([]byte, socksPortLen)
	if _, err := readFull(c, r); err != nil || r[0] != socksVersion || r[1] != socksNoAuth {
		return errors.New("SOCKS greeting refused")
	}
	hostLen := byte(len(host)) //nolint:gosec // len(host) <= socksMaxHostLen (255), checked above
	req := append([]byte{socksVersion, socksCmdConnect, 0, socksAtypDomain, hostLen}, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return errors.New("SOCKS port unreachable")
	}
	head := make([]byte, socksReplyHeader)
	if _, err := readFull(c, head); err != nil {
		return errors.New("no SOCKS answer")
	}
	if head[1] != socksReplyOK {
		// tailscaled answers a refused or unroutable CONNECT with a failure code: no exit node,
		// the exit node is offline, or the policy drops the traffic (#259).
		return fmt.Errorf("the exit node refused the connection (SOCKS reply %d)", head[1])
	}
	var skip int
	switch head[3] {
	case socksAtypIPv4:
		skip = net.IPv4len + socksPortLen
	case socksAtypIPv6:
		skip = net.IPv6len + socksPortLen
	case socksAtypDomain:
		l := make([]byte, 1)
		if _, err := readFull(c, l); err != nil {
			return errors.New("no SOCKS answer")
		}
		skip = int(l[0]) + socksPortLen
	default:
		return errors.New("bad SOCKS answer")
	}
	if _, err := readFull(c, make([]byte, skip)); err != nil {
		return errors.New("no SOCKS answer")
	}
	return nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
