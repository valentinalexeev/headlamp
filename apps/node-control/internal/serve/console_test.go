package serve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// #366: the console name resolves to node-control's own addresses, gets a certificate from the
// serve CA that chains to it, and no service can take it.
func TestConsoleNameCertAndDNS(t *testing.T) {
	r := newRig(t, nil)
	name := "console." + zone
	if err := r.mod.SetConsole("console.elsewhere.example", nil); err == nil {
		t.Fatal("a name outside SERVE_ZONE must be refused")
	}
	if err := r.mod.SetConsole(name, []netip.Addr{netip.MustParseAddr("100.64.0.99")}); err != nil {
		t.Fatal(err)
	}
	r.mod.refreshDNS(context.Background())
	if _, ips := r.lookup(name); len(ips) != 1 || ips[0] != "100.64.0.99" {
		t.Fatalf("console name answers %v", ips)
	}

	cert, err := r.mod.ConsoleCert(&tls.ClientHelloInfo{ServerName: name})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(r.mod.ca.CertPEM())
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
		t.Fatalf("the console certificate must chain to the serve CA for %s: %v", name, err)
	}
	if time.Until(leaf.NotAfter) > consoleLeafTTL+time.Hour {
		t.Fatalf("leaf lasts too long: %v", leaf.NotAfter)
	}
	again, _ := r.mod.ConsoleCert(&tls.ClientHelloInfo{ServerName: name})
	if again != cert {
		t.Fatal("the leaf is reused until half its life")
	}
	if _, err := r.mod.ConsoleCert(&tls.ClientHelloInfo{ServerName: "grafana." + zone}); err == nil {
		t.Fatal("only the console name gets a certificate")
	}

	if code, _ := r.do(http.MethodPost, "/serve/services", ipOwner, map[string]string{"name": "console", "namespace": "mon"}); code != http.StatusConflict {
		t.Fatalf("a service must not take the console name: %d", code)
	}
	for _, bad := range []string{"console.ops." + zone, "*." + zone, zone} {
		if err := r.mod.SetConsole(bad, nil); err == nil {
			t.Fatalf("%q: the console name must be one label under the zone", bad)
		}
	}
	if code, _ := r.do(http.MethodPost, "/serve/services", ipOwner, map[string]string{"name": "grafana", "namespace": "mon"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("another name is fine: %d", code)
	}

	_ = r.mod.SetConsole("", nil)
	if _, err := r.mod.ConsoleCert(nil); err == nil {
		t.Fatal("off means no certificate")
	}
}
