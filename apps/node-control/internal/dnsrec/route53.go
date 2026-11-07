package dnsrec

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Route53API is the part of the Route 53 client that Route53 uses.
type Route53API interface {
	ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, opt ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error)
	ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, opt ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error)
}

// Route53 writes the record in one hosted zone, with the standard AWS credentials (the instance role on AWS).
type Route53 struct {
	Zone   string // hosted zone id
	Client Route53API
}

func (r *Route53) Name() string { return "route53" }

// Route 53 stores "*" in a name as the escape \052.
func r53Name(n string) string { return strings.ReplaceAll(strings.TrimSuffix(n, ".")+".", "*", `\052`) }

func (r *Route53) Get(ctx context.Context, name, typ string) (string, bool, error) {
	out, err := r.Client.ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(r.Zone), StartRecordName: aws.String(r53Name(name)),
		StartRecordType: types.RRType(typ), MaxItems: aws.Int32(1),
	})
	if err != nil {
		return "", false, fmt.Errorf("route53: %w", err)
	}
	for _, s := range out.ResourceRecordSets {
		if strings.EqualFold(aws.ToString(s.Name), r53Name(name)) && string(s.Type) == typ && len(s.ResourceRecords) > 0 {
			return aws.ToString(s.ResourceRecords[0].Value), true, nil
		}
	}
	return "", false, nil
}

// Put is an UPSERT, so it creates or replaces in one call.
func (r *Route53) Put(ctx context.Context, rec Record) error {
	_, err := r.Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(r.Zone),
		ChangeBatch: &types.ChangeBatch{
			Comment: aws.String("headlamp funnel (#127)"),
			Changes: []types.Change{{Action: types.ChangeActionUpsert, ResourceRecordSet: &types.ResourceRecordSet{
				Name: aws.String(r53Name(rec.Name)), Type: types.RRType(rec.Type), TTL: aws.Int64(int64(rec.TTL)),
				ResourceRecords: []types.ResourceRecord{{Value: aws.String(rec.Target)}},
			}}},
		},
	})
	if err != nil {
		return fmt.Errorf("route53: %w", err)
	}
	return nil
}
