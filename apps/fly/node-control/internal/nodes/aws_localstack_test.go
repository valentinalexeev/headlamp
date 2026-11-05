package nodes

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

// TestAWSAgainstLocalStack drives the real provider against a LocalStack EC2 endpoint (a full emulator, not a fake
// written for this test). It runs only when AWS_LOCALSTACK_URL is set, e.g. http://localhost:4566.
func TestAWSAgainstLocalStack(t *testing.T) {
	url := os.Getenv("AWS_LOCALSTACK_URL")
	if url == "" {
		t.Skip("AWS_LOCALSTACK_URL not set")
	}
	// LocalStack accepts any static credentials; these placeholder values are not secrets.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		t.Setenv(k, "localstack")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	cli := ec2.NewFromConfig(cfg, func(o *ec2.Options) { o.BaseEndpoint = aws.String(url) })
	out, err := cli.RunInstances(ctx, &ec2.RunInstancesInput{ImageId: aws.String("ami-df5de72bdb3b"), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		InstanceType: ec2types.InstanceTypeT3Micro,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance,
			Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("ls-exit-a")}}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := aws.ToString(out.Instances[0].InstanceId)
	t.Cleanup(func() { _, _ = cli.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}}) })

	p := NewAWS(core.MapEnv(map[string]string{"AWS_REGION": "us-east-1", "AWS_ENDPOINT_URL_EC2": url}), http.DefaultClient)
	n := Target{Name: "ls-exit-a"}
	wait := func(want string) {
		t.Helper()
		for i := 0; i < 40; i++ {
			if st, err := p.Status(ctx, n); err == nil && st["status"] == want {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		st, err := p.Status(ctx, n)
		t.Fatalf("never reached %s: %v %v", want, st, err)
	}
	wait("RUNNING")
	if st, _ := p.Status(ctx, n); st["shape"] != "t3.micro" {
		t.Errorf("shape: %v", st)
	}
	if _, cur, err := p.Stop(ctx, n); err != nil || (cur != "STOPPING" && cur != "STOPPED") {
		t.Fatalf("stop: %q %v", cur, err)
	}
	wait("STOPPED")
	if prev, cur, err := p.Stop(ctx, n); err != nil || prev != "STOPPED" || cur != "STOPPED" {
		t.Fatalf("stop again: %q %q %v", prev, cur, err)
	}
	if _, cur, err := p.Start(ctx, n); err != nil || (cur != "STARTING" && cur != "RUNNING") {
		t.Fatalf("start: %q %v", cur, err)
	}
	wait("RUNNING")
	t.Logf("instance %s: RUNNING -> STOPPED -> RUNNING through the provider", id)
}
