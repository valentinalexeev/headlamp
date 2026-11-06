package nodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// fakeEC2 answers the EC2 query API (form-encoded POST, XML answers) for DescribeInstances, StartInstances and
// StopInstances, enough for the provider. Requests must be SigV4-signed.
type fakeEC2 struct {
	mu        sync.Mutex
	instances []fakeInstance
	actions   []string // "Action id"
	filters   []string // tag:Name values seen
	failCode  string   // when set, every call answers this AWS error code with 400
	unsigned  int
	groups    map[string]*fakeGroup // Auto Scaling groups by name
	updates   []string              // "group min desired"
}

type fakeGroup struct {
	min, desired int
	instances    map[string]string // id -> lifecycle state
}

type fakeInstance struct{ id, name, state, kind, group string }

func (f *fakeEC2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		f.unsigned++
		http.Error(w, "unsigned", http.StatusForbidden)
		return
	}
	_ = r.ParseForm()
	q := r.PostForm
	if f.failCode != "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>it failed</Message></Error></Errors><RequestID>r</RequestID></Response>`, f.failCode)
		return
	}
	switch q.Get("Action") {
	case "DescribeAutoScalingGroups":
		var b strings.Builder
		b.WriteString(`<DescribeAutoScalingGroupsResponse><DescribeAutoScalingGroupsResult><AutoScalingGroups>`)
		for _, name := range values(q, "AutoScalingGroupNames.member") {
			g, ok := f.groups[name]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, `<member><AutoScalingGroupName>%s</AutoScalingGroupName><MinSize>%d</MinSize><MaxSize>1</MaxSize><DesiredCapacity>%d</DesiredCapacity><Instances>`,
				name, g.min, g.desired)
			for id, lc := range g.instances {
				fmt.Fprintf(&b, `<member><InstanceId>%s</InstanceId><LifecycleState>%s</LifecycleState></member>`, id, lc)
			}
			b.WriteString(`</Instances></member>`)
		}
		b.WriteString(`</AutoScalingGroups></DescribeAutoScalingGroupsResult></DescribeAutoScalingGroupsResponse>`)
		fmt.Fprint(w, b.String())
	case "UpdateAutoScalingGroup":
		name := q.Get("AutoScalingGroupName")
		f.updates = append(f.updates, name+" "+q.Get("MinSize")+" "+q.Get("DesiredCapacity"))
		if g, ok := f.groups[name]; ok {
			fmt.Sscan(q.Get("MinSize"), &g.min)
			fmt.Sscan(q.Get("DesiredCapacity"), &g.desired)
		}
		fmt.Fprint(w, `<UpdateAutoScalingGroupResponse><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></UpdateAutoScalingGroupResponse>`)
	case "DescribeInstances":
		ids := values(q, "InstanceId")
		var name string
		states := map[string]bool{}
		for i := 1; q.Get(fmt.Sprintf("Filter.%d.Name", i)) != ""; i++ {
			switch q.Get(fmt.Sprintf("Filter.%d.Name", i)) {
			case "tag:Name":
				name = q.Get(fmt.Sprintf("Filter.%d.Value.1", i))
				f.filters = append(f.filters, name)
			case "instance-state-name":
				for _, s := range values(q, fmt.Sprintf("Filter.%d.Value", i)) {
					states[s] = true
				}
			}
		}
		var b strings.Builder
		b.WriteString(`<DescribeInstancesResponse><reservationSet><item><instancesSet>`)
		found := false
		for _, in := range f.instances {
			if len(ids) > 0 && !contains(ids, in.id) || name != "" && in.name != name || len(states) > 0 && !states[in.state] {
				continue
			}
			found = true
			tags := ""
			if in.group != "" {
				tags = fmt.Sprintf(`<tagSet><item><key>aws:autoscaling:groupName</key><value>%s</value></item></tagSet>`, in.group)
			}
			fmt.Fprintf(&b, `<item><instanceId>%s</instanceId><instanceType>%s</instanceType><instanceState><name>%s</name></instanceState>%s</item>`,
				in.id, in.kind, in.state, tags)
		}
		b.WriteString(`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		if len(ids) > 0 && !found {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`)
			return
		}
		fmt.Fprint(w, b.String())
	case "StartInstances", "StopInstances":
		id := q.Get("InstanceId.1")
		f.actions = append(f.actions, q.Get("Action")+" "+id)
		fmt.Fprintf(w, `<%sResponse><instancesSet/></%sResponse>`, q.Get("Action"), q.Get("Action"))
	default:
		http.Error(w, "unexpected action "+q.Get("Action"), http.StatusBadRequest)
	}
}

func values(q url.Values, prefix string) []string {
	var out []string
	for i := 1; q.Get(fmt.Sprintf("%s.%d", prefix, i)) != ""; i++ {
		out = append(out, q.Get(fmt.Sprintf("%s.%d", prefix, i)))
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func newAWSTest(t *testing.T, f *fakeEC2, extra map[string]string) Provider {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	env := map[string]string{
		"AWS_REGION": "xx-test-1", "AWS_ENDPOINT_URL_EC2": srv.URL, "AWS_ENDPOINT_URL_AUTO_SCALING": srv.URL,
		"AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY": "secret-key-value",
	}
	for k, v := range extra {
		env[k] = v
	}
	for k, v := range env { // the SDK's default chain reads credentials from the process environment
		t.Setenv(k, v)
	}
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	return NewAWS(core.MapEnv(env), srv.Client())
}

func TestAWSFindsByNameTagAndReportsStatus(t *testing.T) {
	f := &fakeEC2{instances: []fakeInstance{
		{"i-0old", "exit-a", "terminated", "t4g.nano", ""},
		{"i-0abc", "exit-a", "stopped", "t4g.nano", ""},
		{"i-0other", "exit-b", "running", "t4g.nano", ""},
	}}
	p := newAWSTest(t, f, nil)
	st, err := p.Status(context.Background(), Target{Name: "exit-a"})
	if err != nil {
		t.Fatal(err)
	}
	if st["status"] != "STOPPED" || st["shape"] != "t4g.nano" {
		t.Fatalf("status = %v", st)
	}
	// the id is cached: a second call does not list by name again
	if _, err := p.Status(context.Background(), Target{Name: "exit-a"}); err != nil {
		t.Fatal(err)
	}
	if len(f.filters) != 1 || f.filters[0] != "exit-a" {
		t.Fatalf("name lookups = %v", f.filters)
	}
	if f.unsigned != 0 {
		t.Fatal("unsigned request")
	}
}

func TestAWSStartStopAreIdempotent(t *testing.T) {
	f := &fakeEC2{instances: []fakeInstance{{"i-0abc", "exit-a", "stopped", "t4g.nano", ""}}}
	p := newAWSTest(t, f, nil)
	n := Target{Name: "exit-a"}
	prev, cur, err := p.Stop(context.Background(), n)
	if err != nil || prev != "STOPPED" || cur != "STOPPED" || len(f.actions) != 0 {
		t.Fatalf("stop on stopped: %q %q %v %v", prev, cur, err, f.actions)
	}
	prev, cur, err = p.Start(context.Background(), n)
	if err != nil || prev != "STOPPED" || cur != "STARTING" {
		t.Fatalf("start: %q %q %v", prev, cur, err)
	}
	f.instances[0].state = "running"
	if prev, cur, err = p.Start(context.Background(), n); err != nil || prev != "RUNNING" || cur != "RUNNING" {
		t.Fatalf("start on running: %q %q %v", prev, cur, err)
	}
	if prev, cur, err = p.Stop(context.Background(), n); err != nil || prev != "RUNNING" || cur != "STOPPING" {
		t.Fatalf("stop: %q %q %v", prev, cur, err)
	}
	if got := strings.Join(f.actions, ","); got != "StartInstances i-0abc,StopInstances i-0abc" {
		t.Fatalf("actions = %s", got)
	}
}

func TestAWSBlockPinsInstanceAndRegion(t *testing.T) {
	f := &fakeEC2{instances: []fakeInstance{{"i-0pin", "whatever", "running", "t3.micro", ""}}}
	p := newAWSTest(t, f, map[string]string{"AWS_REGION": ""})
	n := Target{Name: "exit-a", Block: map[string]any{"region": "xx-test-2", "instance_id": "i-0pin"}}
	st, err := p.Status(context.Background(), n)
	if err != nil || st["status"] != "RUNNING" {
		t.Fatalf("pinned: %v %v", st, err)
	}
	if len(f.filters) != 0 {
		t.Fatal("a pinned id must not list by name")
	}
}

func TestAWSErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("no region", func(t *testing.T) {
		p := newAWSTest(t, &fakeEC2{}, map[string]string{"AWS_REGION": ""})
		if _, err := p.Status(ctx, Target{Name: "x"}); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("want not configured, got %v", err)
		}
	})
	t.Run("no match", func(t *testing.T) {
		p := newAWSTest(t, &fakeEC2{}, nil)
		if _, err := p.Status(ctx, Target{Name: "x"}); err == nil || !strings.Contains(err.Error(), "no instance") || !strings.Contains(err.Error(), "x-asg") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("two matches", func(t *testing.T) {
		p := newAWSTest(t, &fakeEC2{instances: []fakeInstance{{"i-1", "x", "running", "", ""}, {"i-2", "x", "stopped", "", ""}}}, nil)
		if _, err := p.Status(ctx, Target{Name: "x"}); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("access denied names the code, never the secret", func(t *testing.T) {
		p := newAWSTest(t, &fakeEC2{failCode: "UnauthorizedOperation"}, nil)
		_, err := p.Status(ctx, Target{Name: "x"})
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != 400 || !strings.Contains(err.Error(), "UnauthorizedOperation") {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "secret-key-value") || strings.Contains(err.Error(), "AKIDEXAMPLE") {
			t.Fatalf("credentials in error: %v", err)
		}
	})
	t.Run("a vanished instance drops the cached id", func(t *testing.T) {
		f := &fakeEC2{instances: []fakeInstance{{"i-0abc", "x", "running", "", ""}}}
		p := newAWSTest(t, f, nil)
		if _, err := p.Status(ctx, Target{Name: "x"}); err != nil {
			t.Fatal(err)
		}
		f.instances = []fakeInstance{{"i-0new", "x", "running", "", ""}}
		if _, err := p.Status(ctx, Target{Name: "x"}); err == nil {
			t.Fatal("want an error for the vanished id")
		}
		if st, err := p.Status(ctx, Target{Name: "x"}); err != nil || st["status"] != "RUNNING" || len(f.filters) != 2 {
			t.Fatalf("after re-lookup: %v %v %v", st, err, f.filters)
		}
	})
}

// The exit-node-vm/aws module runs each node in a one-instance Auto Scaling group. Stopping its instance would only make
// the group replace it, so the provider scales the group to 0 and back to 1 (docs/adr/0001).
func TestAWSGroupManagedNodeScalesTheGroup(t *testing.T) {
	ctx := context.Background()
	f := &fakeEC2{
		instances: []fakeInstance{{"i-0asg", "exit-a", "running", "t4g.nano", "exit-a-asg"}},
		groups:    map[string]*fakeGroup{"exit-a-asg": {min: 1, desired: 1, instances: map[string]string{"i-0asg": "InService"}}},
	}
	p := newAWSTest(t, f, nil)
	n := Target{Name: "exit-a"}

	if st, err := p.Status(ctx, n); err != nil || st["status"] != "RUNNING" || st["shape"] != "t4g.nano" {
		t.Fatalf("status: %v %v", st, err)
	}
	if prev, cur, err := p.Start(ctx, n); err != nil || prev != "RUNNING" || cur != "RUNNING" || len(f.updates) != 0 {
		t.Fatalf("start on running group: %q %q %v %v", prev, cur, err, f.updates)
	}
	if prev, cur, err := p.Stop(ctx, n); err != nil || prev != "RUNNING" || cur != "STOPPING" {
		t.Fatalf("stop: %q %q %v", prev, cur, err)
	}
	if len(f.actions) != 0 {
		t.Fatalf("a group-managed instance must not be stopped directly: %v", f.actions)
	}
	// While the instance leaves, the node is STOPPING; once it is gone (and Name no longer matches), STOPPED.
	if st, _ := p.Status(ctx, n); st["status"] != "STOPPING" {
		t.Fatalf("while leaving: %v", st)
	}
	f.groups["exit-a-asg"].instances = nil
	f.instances = []fakeInstance{{"i-0asg", "exit-a", "terminated", "t4g.nano", "exit-a-asg"}}
	if st, _ := p.Status(ctx, n); st["status"] != "STOPPED" {
		t.Fatalf("after scale-in: %v", st)
	}
	if prev, cur, err := p.Stop(ctx, n); err != nil || prev != "STOPPED" || cur != "STOPPED" {
		t.Fatalf("stop on stopped group: %q %q %v", prev, cur, err)
	}
	if prev, cur, err := p.Start(ctx, n); err != nil || prev != "STOPPED" || cur != "STARTING" {
		t.Fatalf("start: %q %q %v", prev, cur, err)
	}
	if st, _ := p.Status(ctx, n); st["status"] != "STARTING" {
		t.Fatalf("no instance yet: %v", st)
	}
	if got := strings.Join(f.updates, ","); got != "exit-a-asg 0 0,exit-a-asg 1 1" {
		t.Fatalf("updates = %s", got)
	}
}

// After the cache expires a stopped group node has no instance with its Name tag; the provider still finds the group
// by the module's name, <name>-asg, so it can be started again.
func TestAWSStoppedGroupFoundByModuleName(t *testing.T) {
	f := &fakeEC2{groups: map[string]*fakeGroup{"exit-a-asg": {min: 0, desired: 0}}}
	p := newAWSTest(t, f, nil)
	n := Target{Name: "exit-a"}
	if st, err := p.Status(context.Background(), n); err != nil || st["status"] != "STOPPED" {
		t.Fatalf("status: %v %v", st, err)
	}
	if _, cur, err := p.Start(context.Background(), n); err != nil || cur != "STARTING" {
		t.Fatalf("start: %q %v", cur, err)
	}
}

func TestAWSBlockNamesTheGroup(t *testing.T) {
	f := &fakeEC2{groups: map[string]*fakeGroup{"custom": {min: 1, desired: 1, instances: map[string]string{"i-9": "Pending"}}}}
	p := newAWSTest(t, f, nil)
	n := Target{Name: "exit-a", Block: map[string]any{"asg": "custom"}}
	if st, err := p.Status(context.Background(), n); err != nil || st["status"] != "STARTING" {
		t.Fatalf("status: %v %v", st, err)
	}
	if len(f.filters) != 0 {
		t.Fatal("a named group must not list instances by name")
	}
}

func TestAWSErrorsRedactIdentities(t *testing.T) {
	keyID := "AKIA" + strings.Repeat("X", 16) // built at runtime: a literal key id would trip the commit hook
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>User: arn:aws:sts::123456789012:assumed-role/node-control/i-0abc is not authorized to perform: ec2:DescribeInstances (account 123456789012, key %s)</Message></Error></Errors></Response>`, keyID)
	}))
	defer srv.Close()

	p := newAWSTest(t, &fakeEC2{}, map[string]string{"AWS_ENDPOINT_URL_EC2": srv.URL})
	_, err := p.Status(context.Background(), Target{Name: "x"})
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, leak := range []string{"123456789012", "arn:aws", keyID, "node-control/i-0abc"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaks %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "UnauthorizedOperation") || !strings.Contains(msg, "ec2:DescribeInstances") {
		t.Errorf("error lost its useful part: %s", msg)
	}
}
