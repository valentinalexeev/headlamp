package nodes

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"example.com/node-control/internal/core"
	ocisign "example.com/node-control/internal/nodes/oci"
)

const ociTimeout = 20 * time.Second

var ociRegionRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type oci struct {
	env core.Env
	hc  *http.Client
	// baseURL replaces https://iaas.<region>.oraclecloud.com (tests).
	baseURL string
	now     func() time.Time
	ids     idCache // name -> instance id for idCacheTTL
}

// NewOCI returns the OCI Compute provider, talking to the REST API with
// signed requests. The target's block is {"oci": {"region", "compartment_id",
// "instance_name", "instance_id"}}, all optional: the instance is found by
// name in the compartment (the id is cached for 15 s) (default Target.Name) unless
// instance_id pins one. region falls back to OCI_REGION and compartment_id to
// OCI_COMPARTMENT_ID. Credentials: OCI_USER_OCID, OCI_TENANCY_OCID,
// OCI_FINGERPRINT and OCI_PRIVATE_KEY (PEM text, PKCS#1 or PKCS#8).
func NewOCI(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &oci{env: env, hc: hc, now: time.Now}
}

type ociCreds struct {
	keyID   string
	key     *rsa.PrivateKey
	secrets []string
}

func (o *oci) require(name string) (string, error) {
	v := o.env.Getenv(name)
	if v == "" {
		return "", notConfigured(name + " is not set")
	}
	return v, nil
}

// parseOCIKey accepts PKCS#1 and PKCS#8 PEM text, with real or literal "\n".
func parseOCIKey(text string) (*rsa.PrivateKey, error) {
	if !strings.Contains(text, "\n") {
		text = strings.ReplaceAll(text, `\n`, "\n")
	}
	blk, _ := pem.Decode([]byte(text))
	if blk == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unsupported key encoding")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA key")
	}
	return rk, nil
}

func (o *oci) creds() (*ociCreds, error) {
	user, err := o.require("OCI_USER_OCID")
	if err != nil {
		return nil, err
	}
	tenancy, err := o.require("OCI_TENANCY_OCID")
	if err != nil {
		return nil, err
	}
	fp, err := o.require("OCI_FINGERPRINT")
	if err != nil {
		return nil, err
	}
	pemText, err := o.require("OCI_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	key, err := parseOCIKey(pemText)
	if err != nil {
		// Never echo the key material.
		return nil, notConfigured("OCI_PRIVATE_KEY is not a usable RSA private key: " + err.Error())
	}
	secrets := []string{pemText, fp}
	for _, line := range strings.Split(strings.ReplaceAll(pemText, `\n`, "\n"), "\n") {
		if line = strings.TrimSpace(line); len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			secrets = append(secrets, line)
		}
	}
	return &ociCreds{keyID: tenancy + "/" + user + "/" + fp, key: key, secrets: secrets}, nil
}

func (o *oci) block(n Target) (region, compartment, name, id string) {
	b := n.Block
	region, _ = b["region"].(string)
	compartment, _ = b["compartment_id"].(string)
	name, _ = b["instance_name"].(string)
	id, _ = b["instance_id"].(string)
	if region == "" {
		region = o.env.Getenv("OCI_REGION")
	}
	if compartment == "" {
		compartment = o.env.Getenv("OCI_COMPARTMENT_ID")
	}
	if name == "" {
		name = n.Name
	}
	return
}

func (o *oci) base(region string) string {
	if o.baseURL != "" {
		return o.baseURL
	}
	return "https://iaas." + region + ".oraclecloud.com"
}

// call performs a signed request; non-2xx becomes an APIError.
func (o *oci) call(ctx context.Context, c *ociCreds, region, method, path string, query url.Values) ([]byte, error) {
	secrets := c.secrets
	u := o.base(region) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	ctx, cancel := context.WithTimeout(ctx, ociTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	if err := ocisign.Sign(req, c.keyID, c.key, o.now()); err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	// Whatever an upstream echoes of our signature must not reach errors.
	auth := req.Header.Get("Authorization")
	secrets = append(append([]string{}, secrets...), auth)
	if i := strings.Index(auth, `signature="`); i >= 0 {
		sig := auth[i+len(`signature="`):]
		if j := strings.Index(sig, `"`); j > 0 {
			secrets = append(secrets, sig[:j])
		}
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, &APIError{Message: scrub(err.Error(), secrets)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &APIError{Status: resp.StatusCode, Message: scrub(err.Error(), secrets)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("%s %s: %d %s", method, path, resp.StatusCode, scrub(string(b), secrets))}
	}
	return b, nil
}

// resolve returns credentials, region and instance id for the target.
func (o *oci) resolve(ctx context.Context, n Target) (*ociCreds, string, string, error) {
	c, err := o.creds()
	if err != nil {
		return nil, "", "", err
	}
	region, compartment, name, id := o.block(n)
	if region == "" {
		return nil, "", "", notConfigured(`set "oci": {"region": ...} (or the OCI_REGION env var)`)
	}
	if !ociRegionRE.MatchString(region) {
		return nil, "", "", notConfigured("OCI region is not a valid region identifier")
	}
	if id != "" {
		return c, region, id, nil
	}
	if name == "" {
		return nil, "", "", notConfigured(`node needs a name, or "oci": {"instance_name": ...} or {"instance_id": ...}`)
	}
	if compartment == "" {
		return nil, "", "", notConfigured(`set "oci": {"compartment_id": ...} (or the OCI_COMPARTMENT_ID env var), or pin "instance_id"`)
	}
	key := o.cacheKey(n)
	if cached, ok := o.ids.get(key, o.now()); ok {
		return c, region, cached, nil
	}
	b, err := o.call(ctx, c, region, http.MethodGet, "/20160918/instances",
		url.Values{"compartmentId": {compartment}, "displayName": {name}})
	if err != nil {
		return nil, "", "", err
	}
	var list []struct {
		ID    string `json:"id"`
		State string `json:"lifecycleState"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, "", "", &APIError{Message: "listing instances: bad response"}
	}
	var live []string
	for _, i := range list {
		if i.State != "TERMINATING" && i.State != "TERMINATED" {
			live = append(live, i.ID)
		}
	}
	switch len(live) {
	case 0:
		return nil, "", "", &APIError{Message: fmt.Sprintf("no instance named '%s' in the compartment", scrub(name, nil))}
	case 1:
		o.ids.put(key, live[0], o.now())
		return c, region, live[0], nil
	}
	return nil, "", "", &APIError{Message: fmt.Sprintf("%d live instances named '%s'; refusing to guess", len(live), scrub(name, nil))}
}

// cacheKey identifies a name lookup; the pinned-id case never uses it.
func (o *oci) cacheKey(n Target) string {
	region, compartment, name, _ := o.block(n)
	return region + "|" + compartment + "|" + name
}

func (o *oci) get(ctx context.Context, c *ociCreds, region, id string) (state, shape string, err error) {
	b, err := o.call(ctx, c, region, http.MethodGet, "/20160918/instances/"+url.PathEscape(id), nil)
	if err != nil {
		return "", "", err
	}
	var doc struct {
		State string `json:"lifecycleState"`
		Shape string `json:"shape"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.State == "" {
		return "", "", &APIError{Message: "reading instance: bad response"}
	}
	return doc.State, doc.Shape, nil
}

func (o *oci) Status(ctx context.Context, n Target) (Status, error) {
	c, region, id, err := o.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	state, shape, err := o.get(ctx, c, region, id)
	if err != nil {
		o.ids.forgetOnNotFound(o.cacheKey(n), err)
		return nil, err
	}
	return Status{"status": state, "shape": shape}, nil
}

func (o *oci) power(ctx context.Context, n Target, want, transient, action string) (string, string, error) {
	c, region, id, err := o.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	prev, _, err := o.get(ctx, c, region, id)
	if err != nil {
		o.ids.forgetOnNotFound(o.cacheKey(n), err)
		return "", "", err
	}
	if prev == want || prev == transient {
		return prev, prev, nil
	}
	if _, err := o.call(ctx, c, region, http.MethodPost, "/20160918/instances/"+url.PathEscape(id), url.Values{"action": {action}}); err != nil {
		o.ids.forgetOnNotFound(o.cacheKey(n), err)
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a RUNNING or already STARTING instance is left alone.
func (o *oci) Start(ctx context.Context, n Target) (string, string, error) {
	return o.power(ctx, n, "RUNNING", "STARTING", "START")
}

// Stop is idempotent: a STOPPED or already STOPPING instance is left alone.
func (o *oci) Stop(ctx context.Context, n Target) (string, string, error) {
	return o.power(ctx, n, "STOPPED", "STOPPING", "STOP")
}
