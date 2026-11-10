package funnel

import (
	"testing"
	"time"
)

func TestIngressStatus(t *testing.T) {
	m := attentionModule(t)
	if _, ok := m.Ingress(); ok {
		t.Fatal("no report yet must give false")
	}
	report(t, m, attentionNow.Add(-time.Minute), 2, "boom")
	s, ok := m.Ingress()
	if !ok || s.Version != "" || s.Stale || s.Errors != 1 {
		t.Fatalf("%+v %v", s, ok)
	}
	body := `{"build_version":"0.16.0","applied_version":"v1","errors":[]}`
	if err := m.st.SaveState("100.64.0.9", []byte(body), attentionNow.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s, _ = m.Ingress(); s.Version != "0.16.0" || !s.Stale || s.Errors != 0 {
		t.Fatalf("%+v", s)
	}
	for _, v := range []string{"<b>", "a b", string(make([]byte, 65))} {
		if plainVersion(v) != "" {
			t.Errorf("%q kept", v)
		}
	}
}
