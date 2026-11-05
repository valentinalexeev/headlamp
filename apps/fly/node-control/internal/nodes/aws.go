package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

const awsTimeout = 20 * time.Second

// awsStatus maps EC2 instance states to the RUNNING/STOPPED vocabulary the other VM providers use.
var awsStatus = map[ec2types.InstanceStateName]string{
	ec2types.InstanceStateNameRunning:  "RUNNING",
	ec2types.InstanceStateNameStopped:  "STOPPED",
	ec2types.InstanceStateNamePending:  "STARTING",
	ec2types.InstanceStateNameStopping: "STOPPING",
}

// ec2API is the part of the EC2 client the provider uses; tests replace the endpoint, not this interface.
type ec2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}

type awsProvider struct {
	env core.Env
	hc  *http.Client
	now func() time.Time
	ids idCache // region|name -> instance id

	mu      sync.Mutex
	clients map[string]ec2API // by region
	load    func(ctx context.Context, region string) (ec2API, error)
}

// NewAWS returns the AWS EC2 provider. The target's block is {"region": ..., "instance_name": ..., "instance_id": ...},
// all optional: the instance is found by its Name tag (default Target.Name, terminated instances ignored, the id cached
// for 15 s) unless instance_id pins one; region falls back to AWS_REGION. Credentials come from the default AWS chain:
// an instance or task role, AWS_PROFILE, or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY. AWS_ENDPOINT_URL_EC2 overrides
// the endpoint (tests, other installations).
func NewAWS(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	p := &awsProvider{env: env, hc: hc, now: time.Now, clients: map[string]ec2API{}}
	p.load = p.defaultLoad
	return p
}

func (p *awsProvider) defaultLoad(ctx context.Context, region string) (ec2API, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region), awsconfig.WithHTTPClient(p.hc)}
	if p.env.Getenv("AWS_PROFILE") != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p.env.Getenv("AWS_PROFILE")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, notConfigured("AWS configuration: " + scrub(err.Error(), nil))
	}
	return ec2.NewFromConfig(cfg, func(o *ec2.Options) {
		if u := p.env.Getenv("AWS_ENDPOINT_URL_EC2"); u != "" {
			o.BaseEndpoint = aws.String(u)
		}
	}), nil
}

func (p *awsProvider) client(ctx context.Context, region string) (ec2API, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[region]; ok {
		return c, nil
	}
	c, err := p.load(ctx, region)
	if err != nil {
		return nil, err
	}
	p.clients[region] = c
	return c, nil
}

func (p *awsProvider) block(n Target) (region, name, id string) {
	region, _ = n.Block["region"].(string)
	name, _ = n.Block["instance_name"].(string)
	id, _ = n.Block["instance_id"].(string)
	if region == "" {
		region = p.env.Getenv("AWS_REGION")
	}
	if name == "" {
		name = n.Name
	}
	return
}

// apiError turns an SDK error into an APIError: HTTP status when there is one, the AWS error code and message,
// bounded and scrubbed. An unknown instance id is a 404, so the id cache drops it.
func apiError(op string, err error) error {
	var ae smithy.APIError
	status := 0
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	if errors.As(err, &ae) {
		if ae.ErrorCode() == "InvalidInstanceID.NotFound" || ae.ErrorCode() == "InvalidInstanceID.Malformed" {
			status = http.StatusNotFound
		}
		return &APIError{Status: status, Message: scrub(fmt.Sprintf("%s: %s: %s", op, ae.ErrorCode(), ae.ErrorMessage()), nil)}
	}
	return &APIError{Status: status, Message: scrub(op+": "+err.Error(), nil)}
}

// resolve returns the region and instance id for the target.
func (p *awsProvider) resolve(ctx context.Context, n Target) (string, string, error) {
	region, name, id := p.block(n)
	if region == "" {
		return "", "", notConfigured(`set "aws": {"region": ...} (or the AWS_REGION env var)`)
	}
	if id != "" {
		return region, id, nil
	}
	if name == "" {
		return "", "", notConfigured(`node needs a name, or "aws": {"instance_name": ...} or {"instance_id": ...}`)
	}
	key := region + "|" + name
	if cached, ok := p.ids.get(key, p.now()); ok {
		return region, cached, nil
	}
	c, err := p.client(ctx, region)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:Name"), Values: []string{name}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	if err != nil {
		return "", "", apiError("listing instances", err)
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			ids = append(ids, aws.ToString(i.InstanceId))
		}
	}
	switch len(ids) {
	case 0:
		return "", "", &APIError{Message: fmt.Sprintf("no instance with Name tag '%s' in %s", scrub(name, nil), scrub(region, nil))}
	case 1:
		p.ids.put(key, ids[0], p.now())
		return region, ids[0], nil
	}
	return "", "", &APIError{Message: fmt.Sprintf("%d instances with Name tag '%s'; refusing to guess", len(ids), scrub(name, nil))}
}

func (p *awsProvider) cacheKey(n Target) string {
	region, name, _ := p.block(n)
	return region + "|" + name
}

type awsInstance struct {
	state, shape string
}

func (p *awsProvider) describe(ctx context.Context, region, id string) (awsInstance, error) {
	c, err := p.client(ctx, region)
	if err != nil {
		return awsInstance{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return awsInstance{}, apiError("reading instance", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id && i.State != nil {
				return awsInstance{state: string(i.State.Name), shape: string(i.InstanceType)}, nil
			}
		}
	}
	return awsInstance{}, &APIError{Status: http.StatusNotFound, Message: "reading instance: not found"}
}

func awsNorm(raw string) string {
	if v, ok := awsStatus[ec2types.InstanceStateName(raw)]; ok {
		return v
	}
	return strings.ToUpper(strings.ReplaceAll(raw, "-", "_"))
}

func (p *awsProvider) Status(ctx context.Context, n Target) (Status, error) {
	region, id, err := p.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	inst, err := p.describe(ctx, region, id)
	if err != nil {
		p.ids.forgetOnNotFound(p.cacheKey(n), err)
		return nil, err
	}
	st := Status{"status": awsNorm(inst.state)}
	if inst.shape != "" {
		st["shape"] = inst.shape
	}
	return st, nil
}

func (p *awsProvider) power(ctx context.Context, n Target, want, transient string, act func(context.Context, ec2API, string) error) (string, string, error) {
	region, id, err := p.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	inst, err := p.describe(ctx, region, id)
	if err != nil {
		p.ids.forgetOnNotFound(p.cacheKey(n), err)
		return "", "", err
	}
	prev := awsNorm(inst.state)
	if prev == want {
		return prev, prev, nil
	}
	c, err := p.client(ctx, region)
	if err != nil {
		return "", "", err
	}
	actx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	if err := act(actx, c, id); err != nil {
		p.ids.forgetOnNotFound(p.cacheKey(n), err)
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a running instance is left alone.
func (p *awsProvider) Start(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, "RUNNING", "STARTING", func(ctx context.Context, c ec2API, id string) error {
		if _, err := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{id}}); err != nil {
			return apiError("starting instance", err)
		}
		return nil
	})
}

// Stop is idempotent: a stopped instance is left alone.
func (p *awsProvider) Stop(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, "STOPPED", "STOPPING", func(ctx context.Context, c ec2API, id string) error {
		if _, err := c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}}); err != nil {
			return apiError("stopping instance", err)
		}
		return nil
	})
}
