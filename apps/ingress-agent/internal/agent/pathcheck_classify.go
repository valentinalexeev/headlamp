package agent

import "strings"

// Path check classes, in order of severity. Classify gives the first that applies as the
// class of a run, and all that apply as its findings.
const (
	PathEdgeUplinkBad = "edge-uplink-bad" // the reference host fails too: the edge's own uplink
	PathDNSFailed     = "dns-failed"      // the central name does not resolve
	PathDNSTampered   = "dns-tampered"    // it resolves, but not to an expected address
	PathIPBlocked     = "ip-blocked"      // no TCP connection to the central address
	PathSNIBlocked    = "sni-blocked"     // TLS fails with the real name, works without it
	PathTLSBlocked    = "tls-blocked"     // TLS fails with and without the name
	PathAppBlocked    = "app-blocked"     // TLS works, the headscale health request does not
	PathDERPBlocked   = "derp-blocked"    // TLS works, the DERP upgrade does not
	PathTailnetDown   = "tailnet-down"    // the edge's tailscaled is not running
	PathStalled       = "stalled"         // the transfer started and then stopped
	PathTransferFail  = "transfer-failed" // the transfer failed in another way
	PathThrottled     = "throttled"       // central is much slower than the reference
	PathUDPBlocked    = "udp-blocked"     // no STUN answer while TCP works
	PathDERPOnly      = "derp-only"       // the exit node is reached only through DERP
)

// throttleRatio: a central transfer slower than 1/throttleRatio of the reference transfer, run
// right after it from the same edge, counts as throttled.
const throttleRatio = 4

var pathSeverity = []string{PathEdgeUplinkBad, PathDNSFailed, PathDNSTampered, PathIPBlocked,
	PathSNIBlocked, PathTLSBlocked, PathAppBlocked, PathDERPBlocked, PathTailnetDown,
	PathStalled, PathTransferFail, PathThrottled, PathUDPBlocked, PathDERPOnly}

func failed(p *PathProbe) bool { return p != nil && !p.OK }

// Classify gives the class of a run and every finding, from the probe results.
func Classify(r *PathResult) (string, []string) {
	found := map[string]bool{}
	centralDown := failed(r.DNS) && r.DNS.Error != errUnexpectedAddress.Error() ||
		failed(r.TCP)
	if failed(r.Ref) && centralDown {
		return PathEdgeUplinkBad, []string{PathEdgeUplinkBad}
	}
	switch {
	case failed(r.DNS) && r.DNS.Error == errUnexpectedAddress.Error():
		found[PathDNSTampered] = true
	case failed(r.DNS):
		found[PathDNSFailed] = true
	}
	switch {
	case failed(r.TCP):
		found[PathIPBlocked] = true
	case failed(r.TLS) && r.TLSNoSNI != nil && r.TLSNoSNI.OK:
		found[PathSNIBlocked] = true
	case failed(r.TLS):
		found[PathTLSBlocked] = true
	}
	found[PathAppBlocked] = failed(r.HTTP)
	found[PathDERPBlocked] = failed(r.DERP)
	found[PathUDPBlocked] = failed(r.STUN) && r.TCP != nil && r.TCP.OK
	if b := r.Blob; b != nil {
		found[PathStalled] = b.Stalled
		found[PathTransferFail] = !b.OK && !b.Stalled
		ref := r.RefBlob
		found[PathThrottled] = b.OK && ref != nil && ref.OK && b.Kbps*throttleRatio < ref.Kbps
	}
	if ts := r.Tailscale; ts != nil && ts.Error == "" {
		found[PathTailnetDown] = ts.State != "Running"
		found[PathDERPOnly] = strings.HasPrefix(ts.ExitPath, "relay:")
	}
	var out []string
	for _, c := range pathSeverity {
		if found[c] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return pathClassOK, nil
	}
	return out[0], out
}
