package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
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

// asgAPI is the part of the Auto Scaling client the provider uses for group-managed nodes (docs/adr/0001).
type asgAPI interface {
	DescribeAutoScalingGroups(context.Context, *autoscaling.DescribeAutoScalingGroupsInput, ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
	UpdateAutoScalingGroup(context.Context, *autoscaling.UpdateAutoScalingGroupInput, ...func(*autoscaling.Options)) (*autoscaling.UpdateAutoScalingGroupOutput, error)
}

type awsClients struct {
	ec2 ec2API
	asg asgAPI
}

// asgTag is the tag AWS puts on every instance an Auto Scaling group launches.
const asgTag = "aws:autoscaling:groupName"

type awsProvider struct {
	env core.Env
	hc  *http.Client
	now func() time.Time
	ids idCache // region|name -> instance id

	mu      sync.Mutex
	clients map[string]awsClients // by region
	load    func(ctx context.Context, region string) (awsClients, error)
}

// NewAWS returns the AWS EC2 provider. The target's block is {"region", "instance_name", "instance_id", "asg"}, all
// optional: the instance is found by its Name tag (default Target.Name, terminated instances ignored, the id cached for
// 15 s) unless instance_id pins one; region falls back to AWS_REGION. An instance launched by an Auto Scaling group
// (or a block naming "asg") is started and stopped by scaling the group between 1 and 0, because the group would
// replace a stopped instance (docs/adr/0001). Credentials come from the default AWS chain: an instance or task role,
// AWS_PROFILE, or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY. AWS_ENDPOINT_URL_EC2 and AWS_ENDPOINT_URL_AUTO_SCALING
// override the endpoints (tests, other installations).
func NewAWS(env core.Env, hc *http.Client) Provider {
	if hc == nil {
		hc = http.DefaultClient
	}
	p := &awsProvider{env: env, hc: hc, now: time.Now, clients: map[string]awsClients{}}
	p.load = p.defaultLoad
	return p
}

func (p *awsProvider) defaultLoad(ctx context.Context, region string) (awsClients, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region), awsconfig.WithHTTPClient(p.hc)}
	if p.env.Getenv("AWS_PROFILE") != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p.env.Getenv("AWS_PROFILE")))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return awsClients{}, notConfigured("AWS configuration: " + scrub(err.Error(), nil))
	}
	return awsClients{
		ec2: ec2.NewFromConfig(cfg, func(o *ec2.Options) {
			if u := p.env.Getenv("AWS_ENDPOINT_URL_EC2"); u != "" {
				o.BaseEndpoint = aws.String(u)
			}
		}),
		asg: autoscaling.NewFromConfig(cfg, func(o *autoscaling.Options) {
			if u := p.env.Getenv("AWS_ENDPOINT_URL_AUTO_SCALING"); u != "" {
				o.BaseEndpoint = aws.String(u)
			}
		}),
	}, nil
}

func (p *awsProvider) client(ctx context.Context, region string) (awsClients, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[region]; ok {
		return c, nil
	}
	c, err := p.load(ctx, region)
	if err != nil {
		return awsClients{}, err
	}
	p.clients[region] = c
	return c, nil
}

// awsTarget is what a node block resolves to: the region, and either a plain instance or an Auto Scaling group (with
// its instance when there is one).
type awsTarget struct {
	region, instance, group string
}

func (p *awsProvider) block(n Target) (region, name, id, group string) {
	region, _ = n.Block["region"].(string)
	name, _ = n.Block["instance_name"].(string)
	id, _ = n.Block["instance_id"].(string)
	group, _ = n.Block["asg"].(string)
	if region == "" {
		region = p.env.Getenv("AWS_REGION")
	}
	if name == "" {
		name = n.Name
	}
	return
}

func (p *awsProvider) cacheKey(n Target) string {
	region, name, _, _ := p.block(n)
	return region + "|" + name
}

// apiError turns an SDK error into an APIError: HTTP status when there is one, the AWS error code and message,
// bounded and scrubbed. An unknown instance id is a 404, so the id cache drops it. AWS messages (AccessDenied in
// particular) name the caller's ARN and account id; those reach the console, so they are redacted.
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
		return &APIError{Status: status, Message: redactAWS(scrub(fmt.Sprintf("%s: %s: %s", op, ae.ErrorCode(), ae.ErrorMessage()), nil))}
	}
	return &APIError{Status: status, Message: redactAWS(scrub(op+": "+err.Error(), nil))}
}

var (
	awsARN       = regexp.MustCompile(`arn:aws[a-z-]*:[^\s"',]+`)
	awsAccountID = regexp.MustCompile(`\b\d{12}\b`)
	awsKeyID     = regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{12,}\b`)
)

// redactAWS removes ARNs, account ids and access key ids from text shown to admins.
func redactAWS(s string) string {
	s = awsARN.ReplaceAllString(s, "<arn>")
	s = awsKeyID.ReplaceAllString(s, "<access-key-id>")
	return awsAccountID.ReplaceAllString(s, "<account>")
}

type awsInstance struct {
	id, state, shape, group string
}

func toInstance(i ec2types.Instance) awsInstance {
	in := awsInstance{id: aws.ToString(i.InstanceId), shape: string(i.InstanceType)}
	if i.State != nil {
		in.state = string(i.State.Name)
	}
	for _, t := range i.Tags {
		if aws.ToString(t.Key) == asgTag {
			in.group = aws.ToString(t.Value)
		}
	}
	return in
}

// findByName lists the live instances with the Name tag. Terminated and shutting-down instances do not count, so a
// replaced or scaled-in instance is never picked.
func (p *awsProvider) findByName(ctx context.Context, c awsClients, name string) ([]awsInstance, error) {
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:Name"), Values: []string{name}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	if err != nil {
		return nil, apiError("listing instances", err)
	}
	var found []awsInstance
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			found = append(found, toInstance(i))
		}
	}
	return found, nil
}

func (p *awsProvider) describe(ctx context.Context, c awsClients, id string) (awsInstance, error) {
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if err != nil {
		return awsInstance{}, apiError("reading instance", err)
	}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if aws.ToString(i.InstanceId) == id {
				return toInstance(i), nil
			}
		}
	}
	return awsInstance{}, &APIError{Status: http.StatusNotFound, Message: "reading instance: not found"}
}

// groupExists reports whether an Auto Scaling group of that name exists.
func (p *awsProvider) groupExists(ctx context.Context, c awsClients, group string) (bool, error) {
	_, ok, err := p.describeGroup(ctx, c, group)
	return ok, err
}

// resolve decides what the target controls. Order: a pinned instance_id; a block "asg"; the instance with the Name
// tag (a group when it carries aws:autoscaling:groupName); with no live instance, the group "<name>-asg" that
// exit-node-vm/aws creates, which has no instance while it is stopped.
func (p *awsProvider) resolve(ctx context.Context, n Target) (awsTarget, error) {
	region, name, id, group := p.block(n)
	if region == "" {
		return awsTarget{}, notConfigured(`set "aws": {"region": ...} (or the AWS_REGION env var)`)
	}
	if group != "" {
		return awsTarget{region: region, group: group}, nil
	}
	if id != "" {
		return awsTarget{region: region, instance: id}, nil
	}
	if name == "" {
		return awsTarget{}, notConfigured(`node needs a name, or "aws": {"instance_name": ...}, {"instance_id": ...} or {"asg": ...}`)
	}
	key := region + "|" + name
	if cached, ok := p.ids.get(key, p.now()); ok {
		if g, isGroup := strings.CutPrefix(cached, "asg:"); isGroup {
			return awsTarget{region: region, group: g}, nil
		}
		return awsTarget{region: region, instance: cached}, nil
	}
	c, err := p.client(ctx, region)
	if err != nil {
		return awsTarget{}, err
	}
	found, err := p.findByName(ctx, c, name)
	if err != nil {
		return awsTarget{}, err
	}
	switch len(found) {
	case 1:
		if found[0].group != "" {
			p.ids.put(key, "asg:"+found[0].group, p.now())
			return awsTarget{region: region, group: found[0].group}, nil
		}
		p.ids.put(key, found[0].id, p.now())
		return awsTarget{region: region, instance: found[0].id}, nil
	case 0:
		guess := name + "-asg"
		ok, err := p.groupExists(ctx, c, guess)
		if err != nil {
			return awsTarget{}, err
		}
		if ok {
			p.ids.put(key, "asg:"+guess, p.now())
			return awsTarget{region: region, group: guess}, nil
		}
		return awsTarget{}, &APIError{Message: fmt.Sprintf("no instance with Name tag '%s' and no group '%s' in %s",
			scrub(name, nil), scrub(guess, nil), scrub(region, nil))}
	}
	return awsTarget{}, &APIError{Message: fmt.Sprintf("%d instances with Name tag '%s'; refusing to guess", len(found), scrub(name, nil))}
}

type awsGroup struct {
	min, desired int32
	instances    []awsGroupInstance
}

type awsGroupInstance struct{ id, lifecycle string }

func (p *awsProvider) describeGroup(ctx context.Context, c awsClients, group string) (awsGroup, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	out, err := c.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{AutoScalingGroupNames: []string{group}})
	if err != nil {
		return awsGroup{}, false, apiError("reading auto scaling group", err)
	}
	for _, g := range out.AutoScalingGroups {
		if aws.ToString(g.AutoScalingGroupName) != group {
			continue
		}
		ag := awsGroup{min: aws.ToInt32(g.MinSize), desired: aws.ToInt32(g.DesiredCapacity)}
		for _, i := range g.Instances {
			ag.instances = append(ag.instances, awsGroupInstance{id: aws.ToString(i.InstanceId), lifecycle: string(i.LifecycleState)})
		}
		return ag, true, nil
	}
	return awsGroup{}, false, nil
}

// groupStatus maps a group to the provider vocabulary. desired 0 is the stopped state: STOPPING while an instance is
// still leaving, STOPPED when none is left. desired 1 is running once one instance is InService and its EC2 state is
// running; before that it is STARTING.
func (p *awsProvider) groupStatus(ctx context.Context, c awsClients, g awsGroup) (Status, error) {
	if g.desired == 0 {
		if len(g.instances) == 0 {
			return Status{"status": "STOPPED"}, nil
		}
		return Status{"status": "STOPPING"}, nil
	}
	for _, i := range g.instances {
		if i.lifecycle != "InService" {
			continue
		}
		inst, err := p.describe(ctx, c, i.id)
		if err != nil {
			return nil, err
		}
		st := Status{"status": awsNorm(inst.state)}
		if inst.shape != "" {
			st["shape"] = inst.shape
		}
		return st, nil
	}
	return Status{"status": "STARTING"}, nil
}

func awsNorm(raw string) string {
	if v, ok := awsStatus[ec2types.InstanceStateName(raw)]; ok {
		return v
	}
	return strings.ToUpper(strings.ReplaceAll(raw, "-", "_"))
}

func (p *awsProvider) Status(ctx context.Context, n Target) (Status, error) {
	t, err := p.resolve(ctx, n)
	if err != nil {
		return nil, err
	}
	c, err := p.client(ctx, t.region)
	if err != nil {
		return nil, err
	}
	if t.group != "" {
		g, ok, err := p.describeGroup(ctx, c, t.group)
		if err != nil {
			return nil, err
		}
		if !ok {
			p.ids.forget(p.cacheKey(n))
			return nil, &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf("no auto scaling group '%s'", scrub(t.group, nil))}
		}
		return p.groupStatus(ctx, c, g)
	}
	inst, err := p.describe(ctx, c, t.instance)
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

// scaleGroup sets MinSize and DesiredCapacity together, so the group neither replaces a scaled-in instance nor refuses
// the change because desired would fall below min. MaxSize is left alone.
func (p *awsProvider) scaleGroup(ctx context.Context, c awsClients, group string, to int32) error {
	ctx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	_, err := c.asg.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(group), MinSize: aws.Int32(to), DesiredCapacity: aws.Int32(to),
	})
	if err != nil {
		return apiError("scaling auto scaling group", err)
	}
	return nil
}

func (p *awsProvider) power(ctx context.Context, n Target, start bool) (string, string, error) {
	want, transient := "STOPPED", "STOPPING"
	if start {
		want, transient = "RUNNING", "STARTING"
	}
	t, err := p.resolve(ctx, n)
	if err != nil {
		return "", "", err
	}
	c, err := p.client(ctx, t.region)
	if err != nil {
		return "", "", err
	}
	if t.group != "" {
		g, ok, err := p.describeGroup(ctx, c, t.group)
		if err != nil {
			return "", "", err
		}
		if !ok {
			p.ids.forget(p.cacheKey(n))
			return "", "", &APIError{Status: http.StatusNotFound, Message: fmt.Sprintf("no auto scaling group '%s'", scrub(t.group, nil))}
		}
		st, err := p.groupStatus(ctx, c, g)
		if err != nil {
			return "", "", err
		}
		prev, _ := st["status"].(string)
		// Idempotent on the group's intent, not only on the instance: a group already scaled the right way is left alone.
		if start && g.desired >= 1 || !start && g.desired == 0 {
			return prev, prev, nil
		}
		to := int32(0)
		if start {
			to = 1
		}
		if err := p.scaleGroup(ctx, c, t.group, to); err != nil {
			return "", "", err
		}
		return prev, transient, nil
	}
	inst, err := p.describe(ctx, c, t.instance)
	if err != nil {
		p.ids.forgetOnNotFound(p.cacheKey(n), err)
		return "", "", err
	}
	prev := awsNorm(inst.state)
	if prev == want {
		return prev, prev, nil
	}
	actx, cancel := context.WithTimeout(ctx, awsTimeout)
	defer cancel()
	if start {
		_, err = c.ec2.StartInstances(actx, &ec2.StartInstancesInput{InstanceIds: []string{t.instance}})
	} else {
		_, err = c.ec2.StopInstances(actx, &ec2.StopInstancesInput{InstanceIds: []string{t.instance}})
	}
	if err != nil {
		err = apiError(map[bool]string{true: "starting instance", false: "stopping instance"}[start], err)
		p.ids.forgetOnNotFound(p.cacheKey(n), err)
		return "", "", err
	}
	return prev, transient, nil
}

// Start is idempotent: a running instance, or a group already scaled to 1, is left alone.
func (p *awsProvider) Start(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, true)
}

// Stop is idempotent: a stopped instance, or a group already scaled to 0, is left alone.
func (p *awsProvider) Stop(ctx context.Context, n Target) (string, string, error) {
	return p.power(ctx, n, false)
}
