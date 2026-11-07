package agent

import (
	"regexp"
	"strconv"
	"strings"
)

// Info is the public parameters of this bridge, served at GET /v1/info. Only public values: there is no field or
// variable for the Reality private key or any credential, because an operator approves these values in
// node-control and they must be safe to show.
type Info struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	SNI       string `json:"sni"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
	// Hy2Port is the public UDP port of the Hysteria 2 inbound (#110), when the bridge has one. The Salamander
	// password is a secret and is never here.
	Hy2Port int `json:"hy2_port,omitempty"`
}

// InfoConfigError names an invalid INGRESS_* variable, never its value.
type InfoConfigError struct{ Var string }

func (e *InfoConfigError) Error() string { return e.Var + " is invalid" }

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func dnsName(v string) bool {
	if len(v) > 253 {
		return false
	}
	for _, l := range strings.Split(v, ".") {
		if !labelRE.MatchString(l) {
			return false
		}
	}
	return true
}

func port(raw, v string) (int, error) {
	if raw == "" || strings.TrimLeft(raw, "0123456789") != "" {
		return 0, &InfoConfigError{v}
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		return 0, &InfoConfigError{v}
	}
	return n, nil
}

// LoadInfo reads the /v1/info parameters. nil without an error means the deployment does not configure them
// (the endpoint answers 503). Whatever is set is checked, even if the set is incomplete.
func LoadInfo(getenv func(string) string) (*Info, error) {
	g := func(k string) string { return getenv(k) }
	if n := g("INGRESS_NAME"); n != "" && !nameRE.MatchString(n) {
		return nil, &InfoConfigError{"INGRESS_NAME"}
	}
	for _, v := range []string{"INGRESS_HOST", "INGRESS_SNI"} {
		if x := g(v); x != "" && !dnsName(x) {
			return nil, &InfoConfigError{v}
		}
	}
	p := 443
	if raw := g("INGRESS_PORT"); raw != "" {
		var err error
		if p, err = port(raw, "INGRESS_PORT"); err != nil {
			return nil, err
		}
	}
	hy2 := 0
	if raw := g("INGRESS_HY2_PORT"); raw != "" {
		var err error
		if hy2, err = port(raw, "INGRESS_HY2_PORT"); err != nil {
			return nil, err
		}
	}
	for _, v := range []string{"INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID"} {
		if x := g(v); x != "" && strings.ContainsAny(x, " \t\r\n\v\f") {
			return nil, &InfoConfigError{v}
		}
	}
	for _, k := range []string{"INGRESS_NAME", "INGRESS_HOST", "INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID"} {
		if g(k) == "" {
			return nil, nil
		}
	}
	sni := g("INGRESS_SNI")
	if sni == "" {
		sni = g("INGRESS_HOST")
	}
	return &Info{Name: g("INGRESS_NAME"), Host: g("INGRESS_HOST"), Port: p, SNI: sni,
		PublicKey: g("INGRESS_PUBLIC_KEY"), ShortID: g("INGRESS_SHORT_ID"), Hy2Port: hy2}, nil
}
