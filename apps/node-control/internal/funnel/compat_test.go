package funnel

import (
	"strings"
	"testing"
)

// #300: under COMPAT_STRICT, GET /ingress/v1/config answers 409 when the last state report names a funnel API
// outside the matrix. No report yet, a report without "api" (older than #300), a compatible one, and warn-only
// mode all get the configuration as before.
func TestStrictRefusesAnIncompatibleIngress(t *testing.T) {
	for _, c := range []struct {
		name   string
		strict bool
		report string // "" : no report
		want   int
	}{
		{"warn only", false, `{"api":9,"errors":[]}`, 200},
		{"strict, incompatible", true, `{"api":9,"errors":[]}`, 409},
		{"strict, compatible", true, `{"api":1,"errors":[]}`, 200},
		{"strict, older than #300", true, `{"errors":[]}`, 200},
		{"strict, no report yet", true, "", 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newIngressRigStrict(t, c.strict)
			if c.report != "" {
				if err := rig.st.SaveState("100.64.0.9", []byte(c.report), rig.now); err != nil {
					t.Fatal(err)
				}
			}
			w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, nil)
			if w.Code != c.want {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if c.want == 409 && (!strings.Contains(w.Body.String(), "incompatible") ||
				!strings.Contains(rig.log.String(), "COMPAT_STRICT")) {
				t.Errorf("body %s log %s", w.Body.String(), rig.log.String())
			}
		})
	}
	// the status the fleet reads carries the reported api
	m := attentionModule(t)
	report(t, m, attentionNow, 0)
	if s, _ := m.Ingress(); s.API != 0 {
		t.Errorf("no api: %d", s.API)
	}
	if err := m.st.SaveState("100.64.0.9", []byte(`{"api":2,"errors":[]}`), attentionNow); err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Ingress(); s.API != 2 {
		t.Errorf("api %d", s.API)
	}
}
