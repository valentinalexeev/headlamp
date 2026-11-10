package enroll

import (
	"strings"
	"testing"
)

// The agent and xray versions in /v1/info (#195) are read for the fleet view but are not in the fingerprint: an
// upgrade must not need a new approval. Control characters are removed.
func TestInfoVersionNotInFingerprint(t *testing.T) {
	body := `{"name":"b","host":"b.example.com","port":443,"sni":"b.example.com","public_key":"K","short_id":"0a"}`
	old, err := parseInfo("b", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if old.version != "" || old.xray != "" {
		t.Fatalf("%+v", old)
	}
	nw, err := parseInfo("b", []byte(strings.TrimSuffix(body, "}")+`,"version":"0.16.0+abc1234\u001b[31m","xray":"25.10.15"}`))
	if err != nil {
		t.Fatal(err)
	}
	if nw.fp != old.fp {
		t.Fatal("the version changed the fingerprint")
	}
	if strings.ContainsRune(nw.version, 0x1b) || !strings.HasPrefix(nw.version, "0.16.0+abc1234") || nw.xray != "25.10.15" {
		t.Fatalf("%q %q", nw.version, nw.xray)
	}
	if _, err := parseInfo("b", []byte(strings.TrimSuffix(body, "}")+`,"version":5}`)); err != nil {
		t.Fatal("a version that is not a string must be ignored, not refused")
	}
}
