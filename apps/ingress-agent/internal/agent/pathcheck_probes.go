package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PathResult is one run of the path check. Errors are short classes (timeout, reset, ...),
// never a URL, an address or a server message.
type PathResult struct {
	Checked  int64    `json:"checked"`
	Duration int64    `json:"duration_ms"`
	Class    string   `json:"class"`
	Findings []string `json:"findings,omitempty"`

	DNS       *PathProbe     `json:"dns,omitempty"`        // the central name resolves as expected
	TCP       *PathProbe     `json:"tcp,omitempty"`        // TCP connect to central
	TLS       *PathProbe     `json:"tls,omitempty"`        // TLS with the real server name
	TLSNoSNI  *PathProbe     `json:"tls_no_sni,omitempty"` // TLS to the same address, no name
	HTTP      *PathProbe     `json:"http,omitempty"`       // GET of the headscale health path
	DERP      *PathProbe     `json:"derp,omitempty"`       // DERP upgrade and server key frame
	STUN      *PathProbe     `json:"stun,omitempty"`       // STUN binding over UDP
	Ref       *PathProbe     `json:"ref,omitempty"`        // TCP and TLS to the reference host
	Blob      *PathTransfer  `json:"blob,omitempty"`       // transfer from central
	RefBlob   *PathTransfer  `json:"ref_blob,omitempty"`   // the same transfer from the reference
	Tailscale *PathTailscale `json:"tailscale,omitempty"`  // passive: the edge's own tailscaled
}

// PathProbe is the result of one probe.
type PathProbe struct {
	OK    bool   `json:"ok"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

// PathTransfer is the result of a timed download.
type PathTransfer struct {
	OK       bool   `json:"ok"`
	Bytes    int64  `json:"bytes"`
	Ms       int64  `json:"ms"`
	TTFBMs   int64  `json:"ttfb_ms"`
	MaxGapMs int64  `json:"max_gap_ms"`
	Kbps     int64  `json:"kbps"`
	Stalled  bool   `json:"stalled,omitempty"`
	Error    string `json:"error,omitempty"`
}

// PathTailscale is what the edge's own tailscaled reports.
type PathTailscale struct {
	State    string `json:"state"`
	Health   int    `json:"health_warnings"`
	ExitPath string `json:"exit_path,omitempty"` // direct, relay:<region>, idle, none
	Error    string `json:"error,omitempty"`
}

const (
	pathProbeTimeout   = 10 * time.Second
	pathSTUNTimeout    = 3 * time.Second
	pathSTUNTries      = 2
	pathBlobTimeout    = 60 * time.Second
	pathStallAfter     = 10 * time.Second
	pathTSTimeout      = 5 * time.Second
	pathMaxTSOutput    = 4 << 20
	pathMaxHeader      = 4096
	pathReadBuf        = 32 << 10
	bitsPerByte        = 8
	httpStatusOK       = 200
	httpStatusSwitch   = 101
	derpFrameServerKey = 1
	derpFrameHeader    = 5
	stunHeaderLen      = 20
	stunTxIDLen        = 12
	stunBindReq        = 0x0001
	stunBindResp       = 0x0101
	stunMagicCookie    = 0x2112A442
	stunMaxPacket      = 512
	maxDNSAddrs        = 2
)

// derpMagic opens the first frame a DERP server sends ("DERP" and a key emoji).
var derpMagic = []byte{'D', 'E', 'R', 'P', 0xf0, 0x9f, 0x94, 0x91}

// probe runs every configured probe once.
func (p *Pathcheck) probe(ctx context.Context, withBlob bool) PathResult {
	var r PathResult
	if p.RefURL != "" {
		r.Ref = p.probeRef(ctx)
	}
	addrs, dns := p.resolve(ctx)
	r.DNS = dns
	if len(addrs) > 0 {
		addr := net.JoinHostPort(addrs[0].String(), strconv.Itoa(p.Port))
		r.TCP = p.timed(ctx, func(ctx context.Context) error {
			c, err := p.dial(ctx, "tcp", addr)
			if err == nil {
				_ = c.Close()
			}
			return err
		})
		if r.TCP.OK {
			p.probeTLS(ctx, addr, &r)
		}
	}
	if p.STUN != "" {
		r.STUN = p.timed(ctx, p.stun)
	}
	if withBlob {
		r.Blob = p.transfer(ctx, p.BlobURL)
		if p.RefURL != "" && r.Ref != nil && r.Ref.OK {
			r.RefBlob = p.transfer(ctx, p.RefURL)
		}
	}
	if p.Tailscale != nil {
		r.Tailscale = p.tailscale(ctx)
	}
	return r
}

func (p *Pathcheck) probeTLS(ctx context.Context, addr string, r *PathResult) {
	r.TLS = p.timed(ctx, func(ctx context.Context) error {
		c, err := p.tlsConn(ctx, addr, p.Host)
		if err == nil {
			_ = c.Close()
		}
		return err
	})
	r.TLSNoSNI = p.timed(ctx, func(ctx context.Context) error { return p.reachTLS(ctx, addr) })
	if !r.TLS.OK {
		return
	}
	if p.HealthPath != "" {
		r.HTTP = p.timed(ctx, func(ctx context.Context) error { return p.health(ctx, addr) })
	}
	if p.DERPPath != "" {
		r.DERP = p.timed(ctx, func(ctx context.Context) error { return p.derp(ctx, addr) })
	}
}

// timed runs f with the probe timeout and records its time and error class.
func (p *Pathcheck) timed(ctx context.Context, f func(context.Context) error) *PathProbe {
	ctx, cancel := context.WithTimeout(ctx, pathProbeTimeout)
	defer cancel()
	start := p.now()
	err := f(ctx)
	r := &PathProbe{OK: err == nil, Ms: p.now().Sub(start).Milliseconds()}
	if err != nil {
		r.Error = errClass(err)
	}
	return r
}

func (p *Pathcheck) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (p *Pathcheck) tlsConfig(name string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.TLS != nil {
		cfg = p.TLS.Clone()
	}
	cfg.ServerName = name
	return cfg
}

// resolve looks up the central name. An IP target needs no lookup.
func (p *Pathcheck) resolve(ctx context.Context) ([]net.IP, *PathProbe) {
	if ip := net.ParseIP(p.Host); ip != nil {
		return []net.IP{ip}, nil
	}
	var addrs []net.IP
	pr := p.timed(ctx, func(ctx context.Context) error {
		var err error
		addrs, err = p.lookup(ctx, p.Host)
		if err != nil {
			return err
		}
		if len(addrs) == 0 {
			return errNoAddress
		}
		if len(p.ExpectIPs) > 0 && !anyExpected(addrs, p.ExpectIPs) {
			return errUnexpectedAddress
		}
		return nil
	})
	// On an unexpected address, the probes go on with it: they tell what the network serves.
	return addrs, pr
}

func (p *Pathcheck) lookup(ctx context.Context, host string) ([]net.IP, error) {
	if p.Lookup != nil {
		return p.Lookup(ctx, host)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	// IPv4 first: the edge dials IPv4 for its own traffic.
	var v4, v6 []net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = append(v4, ip)
		} else {
			v6 = append(v6, ip)
		}
	}
	out := append(v4, v6...)
	if len(out) > maxDNSAddrs {
		out = out[:maxDNSAddrs]
	}
	return out, nil
}

func anyExpected(got, want []net.IP) bool {
	for _, g := range got {
		for _, w := range want {
			if g.Equal(w) {
				return true
			}
		}
	}
	return false
}

func (p *Pathcheck) tlsConn(ctx context.Context, addr, name string) (*tls.Conn, error) {
	c, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	t := tls.Client(c, p.tlsConfig(name))
	if err := t.HandshakeContext(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return t, nil
}

// reachTLS sends a ClientHello with no server name to the same address. Any answer of the
// server, a finished handshake or a TLS alert, means the address is reachable at the TLS
// layer. Together with a failed TLS probe with the real name, it shows a block by name.
func (p *Pathcheck) reachTLS(ctx context.Context, addr string) error {
	c, err := p.dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	//nolint:gosec // reachability only: no data is sent and the certificate does not matter
	t := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	err = t.HandshakeContext(ctx)
	if err == nil || strings.Contains(err.Error(), "remote error") {
		return nil
	}
	return err
}

// health does GET <HealthPath> on headscale and expects 200.
func (p *Pathcheck) health(ctx context.Context, addr string) error {
	t, err := p.tlsConn(ctx, addr, p.Host)
	if err != nil {
		return err
	}
	defer t.Close()
	req := "GET " + p.HealthPath + " HTTP/1.1\r\nHost: " + p.hostHeader() +
		"\r\nConnection: close\r\nUser-Agent: ingress-agent-pathcheck\r\n\r\n"
	if _, err := io.WriteString(t, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReaderSize(t, pathMaxHeader), nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != httpStatusOK {
		return httpStatusError(resp.StatusCode)
	}
	return nil
}

// derp opens the DERP upgrade and reads the server key frame, which the server sends first.
func (p *Pathcheck) derp(ctx context.Context, addr string) error {
	t, err := p.tlsConn(ctx, addr, p.Host)
	if err != nil {
		return err
	}
	defer t.Close()
	req := "GET " + p.DERPPath + " HTTP/1.1\r\nHost: " + p.hostHeader() +
		"\r\nUpgrade: DERP\r\nConnection: Upgrade\r\nUser-Agent: ingress-agent-pathcheck\r\n\r\n"
	if _, err := io.WriteString(t, req); err != nil {
		return err
	}
	br := bufio.NewReaderSize(t, pathMaxHeader)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	// A 101 answer has no body; the frames follow on br.
	defer resp.Body.Close()
	if resp.StatusCode != httpStatusSwitch {
		return httpStatusError(resp.StatusCode)
	}
	head := make([]byte, derpFrameHeader+len(derpMagic))
	if _, err := io.ReadFull(br, head); err != nil {
		return err
	}
	if head[0] != derpFrameServerKey || !bytes.Equal(head[derpFrameHeader:], derpMagic) {
		return errNotDERP
	}
	return nil
}

func (p *Pathcheck) hostHeader() string {
	if p.Port == httpsPort {
		return p.Host
	}
	return net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
}

// stun sends a STUN binding request and waits for the matching answer.
func (p *Pathcheck) stun(ctx context.Context) error {
	c, err := p.dial(ctx, "udp", p.STUN)
	if err != nil {
		return err
	}
	defer c.Close()
	req := make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(req[0:2], stunBindReq)
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	if _, err := rand.Read(req[8:stunHeaderLen]); err != nil {
		return err
	}
	buf := make([]byte, stunMaxPacket)
	err = errNoAnswer
	for range pathSTUNTries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = c.SetDeadline(p.now().Add(pathSTUNTimeout))
		if _, err = c.Write(req); err != nil {
			continue
		}
		n, rerr := c.Read(buf)
		if rerr != nil {
			err = rerr
			continue
		}
		if n >= stunHeaderLen && binary.BigEndian.Uint16(buf[0:2]) == stunBindResp &&
			bytes.Equal(buf[8:stunHeaderLen], req[8:stunHeaderLen]) {
			return nil
		}
		err = errNotSTUN
	}
	return err
}

// probeRef checks TCP and TLS to the reference host, to tell a bad uplink of the edge from
// a block on the way to central.
func (p *Pathcheck) probeRef(ctx context.Context) *PathProbe {
	return p.timed(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.RefURL, nil)
		if err != nil {
			return err
		}
		resp, err := p.client(pathProbeTimeout).Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})
}

func (p *Pathcheck) client(timeout time.Duration) *http.Client {
	tr := &http.Transport{DisableKeepAlives: true, ForceAttemptHTTP2: false,
		TLSClientConfig: p.tlsConfig(""), DialContext: p.dial, Proxy: nil}
	return &http.Client{Transport: tr, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
}

// transfer reads up to BlobBytes from u and records the rate, the time to the first byte and
// the longest gap between reads. No progress for StallAfter ends it as a stall: a common
// signature of throttling that lets a connection start and then holds it.
func (p *Pathcheck) transfer(ctx context.Context, u string) *PathTransfer {
	ctx, cancel := context.WithTimeout(ctx, pathBlobTimeout)
	defer cancel()
	stall := p.StallAfter
	if stall <= 0 {
		stall = pathStallAfter
	}
	r := &PathTransfer{}
	start := p.now()
	defer func() {
		r.Ms = p.now().Sub(start).Milliseconds()
		if r.Ms > 0 {
			r.Kbps = r.Bytes * bitsPerByte / r.Ms
		}
	}()
	resp, err := p.startTransfer(ctx, u, stall)
	if err != nil {
		r.Error = errClass(err)
		return r
	}
	defer resp.Body.Close()
	r.TTFBMs = p.now().Sub(start).Milliseconds()
	stalled := p.readTransfer(ctx, resp.Body, stall, r)
	switch {
	case stalled:
		r.Stalled, r.Error = true, "stalled"
	case r.Bytes < p.BlobBytes && r.Error == "":
		r.Error = "short"
	}
	r.OK = r.Error == ""
	return r
}

func (p *Pathcheck) startTransfer(ctx context.Context, u string,
	stall time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(p.BlobBytes-1, 10))
	req.Header.Set("User-Agent", "ingress-agent-pathcheck")
	resp, err := p.client(pathBlobTimeout + stall).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, httpStatusError(resp.StatusCode)
	}
	return resp, nil
}

// readTransfer reads the body into r; it returns true when the transfer stalled.
func (p *Pathcheck) readTransfer(ctx context.Context, body io.Reader, stall time.Duration,
	r *PathTransfer) bool {
	type chunk struct {
		n   int
		err error
	}
	ch := make(chan chunk)
	go func() {
		buf := make([]byte, pathReadBuf)
		for {
			n, err := body.Read(buf)
			select {
			case ch <- chunk{n, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	last := p.now()
	for r.Bytes < p.BlobBytes {
		select {
		case c := <-ch:
			now := p.now()
			if c.n > 0 {
				r.MaxGapMs = max(r.MaxGapMs, now.Sub(last).Milliseconds())
				last = now
				r.Bytes = min(r.Bytes+int64(c.n), p.BlobBytes)
			}
			if c.err != nil {
				if !errors.Is(c.err, io.EOF) {
					r.Error = errClass(c.err)
				}
				return false
			}
		case <-time.After(stall):
			r.MaxGapMs = max(r.MaxGapMs, p.now().Sub(last).Milliseconds())
			return true
		case <-ctx.Done():
			r.Error = "timeout"
			return false
		}
	}
	return false
}

// tailscale reads `tailscale status --json` of the edge: backend state, health warnings,
// and how the exit node is reached (direct UDP, or relayed through DERP).
func (p *Pathcheck) tailscale(ctx context.Context) *PathTailscale {
	ctx, cancel := context.WithTimeout(ctx, pathTSTimeout)
	defer cancel()
	out, err := p.exec(ctx, p.Tailscale)
	if err != nil {
		return &PathTailscale{Error: "status unavailable"}
	}
	var st struct {
		BackendState string
		Health       []string
		Peer         map[string]struct {
			ExitNode bool
			CurAddr  string
			Relay    string
			Active   bool
		}
	}
	if json.Unmarshal(out, &st) != nil {
		return &PathTailscale{Error: "status unreadable"}
	}
	r := &PathTailscale{State: st.BackendState, Health: len(st.Health), ExitPath: "none"}
	for _, peer := range st.Peer {
		if !peer.ExitNode {
			continue
		}
		switch {
		case peer.CurAddr != "":
			r.ExitPath = "direct"
		case peer.Relay != "" && peer.Active:
			r.ExitPath = "relay:" + peer.Relay
		default:
			r.ExitPath = "idle"
		}
	}
	return r
}

func (p *Pathcheck) exec(ctx context.Context, argv []string) ([]byte, error) {
	if p.Exec != nil {
		return p.Exec(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed argv from config
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, n: pathMaxTSOutput}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if len(b) > l.n {
		return 0, errTooLarge
	}
	l.n -= len(b)
	return l.w.Write(b)
}

type httpStatusError int

func (e httpStatusError) Error() string { return "HTTP " + strconv.Itoa(int(e)) }

var (
	errNoAddress         = errors.New("no address")
	errUnexpectedAddress = errors.New("unexpected address")
	errNotDERP           = errors.New("not a DERP server")
	errNotSTUN           = errors.New("not a STUN answer")
	errNoAnswer          = errors.New("no answer")
	errTooLarge          = errors.New("output too large")
)

// errClass maps an error to a short class that is safe to show and store.
func errClass(err error) string {
	var hs httpStatusError
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.As(err, &hs):
		return hs.Error()
	case errors.Is(err, errNoAddress), errors.Is(err, errUnexpectedAddress),
		errors.Is(err, errNotDERP), errors.Is(err, errNotSTUN), errors.Is(err, errNoAnswer):
		return err.Error()
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "name not found"
		}
		return "lookup failed"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "closed"
	case errors.As(err, &certErr):
		return "certificate"
	case strings.Contains(err.Error(), "remote error"):
		return "tls alert"
	}
	return "failed"
}
