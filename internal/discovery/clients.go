package discovery

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// ScanClients holds the per-region AWS SDK clients the discovery scanners use. It is
// built from an assumed-role config (aws.Service.AssumeRoleConfigForAccount) — these
// services are not part of the shared aws.ClientBundle since only discovery needs them.
type ScanClients struct {
	Region      string
	ECS         *ecs.Client
	ELB         *elasticloadbalancingv2.Client
	RDS         *rds.Client
	ElastiCache *elasticache.Client
	Lambda      *lambda.Client
	S3          *s3.Client
	SQS         *sqs.Client
}

// NewScanClients builds a ScanClients from an assumed-role config for one region.
func NewScanClients(cfg aws.Config, region string) *ScanClients {
	return &ScanClients{
		Region:      region,
		ECS:         ecs.NewFromConfig(cfg),
		ELB:         elasticloadbalancingv2.NewFromConfig(cfg),
		RDS:         rds.NewFromConfig(cfg),
		ElastiCache: elasticache.NewFromConfig(cfg),
		Lambda:      lambda.NewFromConfig(cfg),
		S3:          s3.NewFromConfig(cfg),
		SQS:         sqs.NewFromConfig(cfg),
	}
}
