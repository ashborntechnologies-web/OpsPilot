// Package discovery scans connected AWS accounts for existing infrastructure so users
// can onboard environments OpsPilot did not create. Each AWS service has an independent
// scanner; ScanAccount runs them in parallel and upserts the results into
// discovered_resources (idempotent on re-scan). Resources tagged ManagedBy=OpsPilot are
// flagged is_managed. project_id stays NULL until a user assigns a resource to a project.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/awstags"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"

	"github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/elasticache"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
)

type Service struct {
	db       *models.DB
	awsSvc   *awssvc.Service
	enqueuer ScanEnqueuer // optional; set via SetEnqueuer for the scan-trigger endpoint
}

func NewService(db *models.DB, awsSvc *awssvc.Service) *Service {
	return &Service{db: db, awsSvc: awsSvc}
}

// scanned is a service-agnostic resource row produced by a scanner before it is tied to
// an account/org and persisted.
type scanned struct {
	Type     string
	ID       string // AWS ARN or native ID
	Name     string
	Region   string
	Metadata map[string]any
	Tags     map[string]any
}

func (r scanned) managed() bool {
	v, ok := r.Tags[awstags.KeyManagedBy]
	return ok && v == awstags.ManagedByValue
}

// ScanAccountByID is the entry point used by the queue handler. It resolves the regions
// the account is used in, assumes the account's role per region, and runs ScanAccount.
// Errors in one region do not stop the others; the account's last_scanned_at is updated
// at the end.
func (s *Service) ScanAccountByID(ctx context.Context, accountID uuid.UUID) error {
	var orgID uuid.UUID
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT org_id FROM aws_accounts WHERE id = $1`, accountID,
	).Scan(&orgID); err != nil {
		return fmt.Errorf("discovery: load account %s: %w", accountID, err)
	}

	regions, err := s.awsSvc.AccountRegions(ctx, accountID)
	if err != nil {
		return fmt.Errorf("discovery: regions for %s: %w", accountID, err)
	}

	for _, region := range regions {
		cfg, err := s.awsSvc.AssumeRoleConfigForAccount(ctx, accountID, region)
		if err != nil {
			slog.Error("discovery: assume role failed", "component", "discovery",
				"account", accountID, "region", region, "error", err)
			continue
		}
		s.ScanAccount(ctx, NewScanClients(cfg, region), accountID, orgID)
	}

	s.awsSvc.MarkAccountScanned(ctx, accountID)
	return nil
}

// ScanAllAccounts enqueues a scan job for every connected AWS account. Invoked by the
// daily scheduler so each account is refreshed without one giant job.
func (s *Service) ScanAllAccounts(ctx context.Context) error {
	if s.enqueuer == nil {
		return nil
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT id FROM aws_accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if _, err := s.enqueuer.EnqueueScan(id.String()); err != nil {
			slog.Warn("discovery: failed to enqueue scheduled scan", "component", "discovery", "account", id, "error", err)
		}
	}
	slog.Info("discovery: enqueued scheduled scans", "component", "discovery", "accounts", len(ids))
	return nil
}

// ScanAccount runs every scanner in parallel for one region and upserts the results.
// Each scanner is isolated: a failure (missing IAM permission, throttling) is logged and
// skipped so the others still complete.
func (s *Service) ScanAccount(ctx context.Context, clients *ScanClients, accountID, orgID uuid.UUID) {
	scanners := map[string]func(context.Context, *ScanClients) ([]scanned, error){
		"ecs":         s.ScanECSServices,
		"rds":         s.ScanRDSInstances,
		"elasticache": s.ScanElastiCache,
		"lambda":      s.ScanLambda,
		"s3":          s.ScanS3,
		"alb":         s.ScanALBs,
		"sqs":         s.ScanSQS,
	}

	var (
		mu  sync.Mutex
		all []scanned
		wg  sync.WaitGroup
	)
	for name, fn := range scanners {
		wg.Add(1)
		go func(name string, fn func(context.Context, *ScanClients) ([]scanned, error)) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("discovery: scanner panicked", "component", "discovery", "scanner", name, "panic", r)
				}
			}()
			res, err := fn(ctx, clients)
			if err != nil {
				slog.Warn("discovery: scanner failed", "component", "discovery",
					"scanner", name, "region", clients.Region, "error", err)
				return
			}
			mu.Lock()
			all = append(all, res...)
			mu.Unlock()
		}(name, fn)
	}
	wg.Wait()

	for _, r := range all {
		s.upsert(ctx, orgID, accountID, r)
	}
	slog.Info("discovery: scan complete", "component", "discovery",
		"account", accountID, "region", clients.Region, "resources", len(all))
}

// upsert writes one discovered resource, keyed by (org_id, resource_type, resource_id).
// A re-scan refreshes last_seen_at, metadata, tags, name, and is_managed without
// disturbing project_id (the user's assignment) or first_seen_at.
func (s *Service) upsert(ctx context.Context, orgID, accountID uuid.UUID, r scanned) {
	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO discovered_resources
		    (org_id, aws_account_id, resource_type, resource_id, resource_name, region, metadata, tags, is_managed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (org_id, resource_type, resource_id) DO UPDATE SET
		    resource_name = EXCLUDED.resource_name,
		    region        = EXCLUDED.region,
		    metadata      = EXCLUDED.metadata,
		    tags          = EXCLUDED.tags,
		    is_managed    = EXCLUDED.is_managed,
		    last_seen_at  = NOW()`,
		orgID, accountID, r.Type, r.ID, r.Name, r.Region, r.Metadata, r.Tags, r.managed())
	if err != nil {
		slog.Warn("discovery: upsert failed", "component", "discovery",
			"resource_type", r.Type, "resource_id", r.ID, "error", err)
	}
}

// ─── Scanners ─────────────────────────────────────────────────────────────────
// Each returns the resources it found; an error aborts only that scanner.

// ScanECSServices discovers ECS clusters and the services within them, recording the
// task definition's log group (used by the monitor for discovered services).
func (s *Service) ScanECSServices(ctx context.Context, c *ScanClients) ([]scanned, error) {
	clusters, err := c.ECS.ListClusters(ctx, &ecs.ListClustersInput{})
	if err != nil {
		return nil, err
	}
	var out []scanned
	for _, clusterARN := range clusters.ClusterArns {
		out = append(out, scanned{
			Type: models.ResourceECSCluster, ID: clusterARN, Name: arnTail(clusterARN),
			Region: c.Region, Metadata: map[string]any{"cluster_arn": clusterARN}, Tags: map[string]any{},
		})

		var svcARNs []string
		paginator := ecs.NewListServicesPaginator(c.ECS, &ecs.ListServicesInput{Cluster: aws.String(clusterARN)})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(ctx)
			if err != nil {
				break
			}
			svcARNs = append(svcARNs, page.ServiceArns...)
		}
		// DescribeServices accepts at most 10 services per call.
		for i := 0; i < len(svcARNs); i += 10 {
			end := i + 10
			if end > len(svcARNs) {
				end = len(svcARNs)
			}
			desc, err := c.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
				Cluster:  aws.String(clusterARN),
				Services: svcARNs[i:end],
				Include:  []ecstypes.ServiceField{ecstypes.ServiceFieldTags},
			})
			if err != nil {
				continue
			}
			for _, svc := range desc.Services {
				meta := map[string]any{
					"cluster_arn":   clusterARN,
					"cluster_name":  arnTail(clusterARN),
					"service_name":  aws.ToString(svc.ServiceName),
					"status":        aws.ToString(svc.Status),
					"running_count": svc.RunningCount,
					"desired_count": svc.DesiredCount,
				}
				if lg := s.ecsLogGroup(ctx, c, aws.ToString(svc.TaskDefinition)); lg != "" {
					meta["log_group_name"] = lg
				}
				out = append(out, scanned{
					Type: models.ResourceECSService, ID: aws.ToString(svc.ServiceArn),
					Name: aws.ToString(svc.ServiceName), Region: c.Region,
					Metadata: meta, Tags: ecsTags(svc.Tags),
				})
			}
		}
	}
	return out, nil
}

// ecsLogGroup best-effort extracts the awslogs group from a task definition's first
// container, so the log scanner can monitor discovered services.
func (s *Service) ecsLogGroup(ctx context.Context, c *ScanClients, taskDef string) string {
	if taskDef == "" {
		return ""
	}
	td, err := c.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(taskDef)})
	if err != nil || td.TaskDefinition == nil {
		return ""
	}
	for _, cd := range td.TaskDefinition.ContainerDefinitions {
		if cd.LogConfiguration != nil {
			if g, ok := cd.LogConfiguration.Options["awslogs-group"]; ok {
				return g
			}
		}
	}
	return ""
}

func (s *Service) ScanRDSInstances(ctx context.Context, c *ScanClients) ([]scanned, error) {
	var out []scanned
	paginator := rds.NewDescribeDBInstancesPaginator(c.RDS, &rds.DescribeDBInstancesInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, db := range page.DBInstances {
			meta := map[string]any{
				"engine":         aws.ToString(db.Engine),
				"status":         aws.ToString(db.DBInstanceStatus),
				"instance_class": aws.ToString(db.DBInstanceClass),
				"multi_az":       aws.ToBool(db.MultiAZ),
			}
			if db.Endpoint != nil {
				meta["endpoint"] = aws.ToString(db.Endpoint.Address)
			}
			tags := map[string]any{}
			for _, t := range db.TagList {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
			out = append(out, scanned{
				Type: models.ResourceRDSInstance, ID: aws.ToString(db.DBInstanceArn),
				Name: aws.ToString(db.DBInstanceIdentifier), Region: c.Region, Metadata: meta, Tags: tags,
			})
		}
	}
	return out, nil
}

func (s *Service) ScanElastiCache(ctx context.Context, c *ScanClients) ([]scanned, error) {
	var out []scanned
	paginator := elasticache.NewDescribeCacheClustersPaginator(c.ElastiCache, &elasticache.DescribeCacheClustersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, cc := range page.CacheClusters {
			arn := aws.ToString(cc.ARN)
			meta := map[string]any{
				"engine":    aws.ToString(cc.Engine),
				"status":    aws.ToString(cc.CacheClusterStatus),
				"node_type": aws.ToString(cc.CacheNodeType),
			}
			tags := map[string]any{}
			if arn != "" {
				if tl, err := c.ElastiCache.ListTagsForResource(ctx, &elasticache.ListTagsForResourceInput{ResourceName: aws.String(arn)}); err == nil {
					for _, t := range tl.TagList {
						tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
					}
				}
			}
			out = append(out, scanned{
				Type: models.ResourceElastiCache, ID: arn, Name: aws.ToString(cc.CacheClusterId),
				Region: c.Region, Metadata: meta, Tags: tags,
			})
		}
	}
	return out, nil
}

func (s *Service) ScanLambda(ctx context.Context, c *ScanClients) ([]scanned, error) {
	var out []scanned
	paginator := lambda.NewListFunctionsPaginator(c.Lambda, &lambda.ListFunctionsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, fn := range page.Functions {
			arn := aws.ToString(fn.FunctionArn)
			meta := map[string]any{
				"runtime":     string(fn.Runtime),
				"handler":     aws.ToString(fn.Handler),
				"memory_size": aws.ToInt32(fn.MemorySize),
				"timeout":     aws.ToInt32(fn.Timeout),
			}
			tags := map[string]any{}
			if arn != "" {
				if tg, err := c.Lambda.ListTags(ctx, &lambda.ListTagsInput{Resource: aws.String(arn)}); err == nil {
					for k, v := range tg.Tags {
						tags[k] = v
					}
				}
			}
			out = append(out, scanned{
				Type: models.ResourceLambda, ID: arn, Name: aws.ToString(fn.FunctionName),
				Region: c.Region, Metadata: meta, Tags: tags,
			})
		}
	}
	return out, nil
}

// ScanS3 lists buckets (a global API) and resolves each bucket's real region via
// GetBucketLocation; the idempotent upsert means re-running per region is harmless.
func (s *Service) ScanS3(ctx context.Context, c *ScanClients) ([]scanned, error) {
	list, err := c.S3.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	var out []scanned
	for _, b := range list.Buckets {
		name := aws.ToString(b.Name)
		region := "us-east-1"
		if loc, err := c.S3.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(name)}); err == nil {
			if string(loc.LocationConstraint) != "" {
				region = string(loc.LocationConstraint)
			}
		}
		tags := map[string]any{}
		if tg, err := c.S3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(name)}); err == nil {
			for _, t := range tg.TagSet {
				tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
			}
		}
		out = append(out, scanned{
			Type: models.ResourceS3Bucket, ID: "arn:aws:s3:::" + name, Name: name,
			Region: region, Metadata: map[string]any{"bucket": name}, Tags: tags,
		})
	}
	return out, nil
}

// ScanALBs discovers application load balancers (ELBv2 type=application).
func (s *Service) ScanALBs(ctx context.Context, c *ScanClients) ([]scanned, error) {
	var out []scanned
	paginator := elasticloadbalancingv2.NewDescribeLoadBalancersPaginator(c.ELB, &elasticloadbalancingv2.DescribeLoadBalancersInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, lb := range page.LoadBalancers {
			if lb.Type != elbtypes.LoadBalancerTypeEnumApplication {
				continue
			}
			arn := aws.ToString(lb.LoadBalancerArn)
			meta := map[string]any{
				"dns_name": aws.ToString(lb.DNSName),
				"scheme":   string(lb.Scheme),
			}
			if lb.State != nil {
				meta["state"] = string(lb.State.Code)
			}
			tags := map[string]any{}
			if arn != "" {
				if td, err := c.ELB.DescribeTags(ctx, &elasticloadbalancingv2.DescribeTagsInput{ResourceArns: []string{arn}}); err == nil {
					for _, desc := range td.TagDescriptions {
						for _, t := range desc.Tags {
							tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
						}
					}
				}
			}
			out = append(out, scanned{
				Type: models.ResourceALB, ID: arn, Name: aws.ToString(lb.LoadBalancerName),
				Region: c.Region, Metadata: meta, Tags: tags,
			})
		}
	}
	return out, nil
}

func (s *Service) ScanSQS(ctx context.Context, c *ScanClients) ([]scanned, error) {
	list, err := c.SQS.ListQueues(ctx, &sqs.ListQueuesInput{})
	if err != nil {
		return nil, err
	}
	var out []scanned
	for _, qURL := range list.QueueUrls {
		arn := qURL
		if attr, err := c.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(qURL), AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
		}); err == nil {
			if a, ok := attr.Attributes["QueueArn"]; ok && a != "" {
				arn = a
			}
		}
		tags := map[string]any{}
		if tg, err := c.SQS.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(qURL)}); err == nil {
			for k, v := range tg.Tags {
				tags[k] = v
			}
		}
		out = append(out, scanned{
			Type: models.ResourceSQSQueue, ID: arn, Name: queueName(qURL),
			Region: c.Region, Metadata: map[string]any{"queue_url": qURL}, Tags: tags,
		})
	}
	return out, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func ecsTags(tags []ecstypes.Tag) map[string]any {
	m := map[string]any{}
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

// arnTail returns the final path segment of an ARN (e.g. the cluster/resource name).
func arnTail(arn string) string {
	if i := strings.LastIndexAny(arn, "/:"); i >= 0 && i < len(arn)-1 {
		return arn[i+1:]
	}
	return arn
}

func queueName(qURL string) string {
	if i := strings.LastIndex(qURL, "/"); i >= 0 && i < len(qURL)-1 {
		return qURL[i+1:]
	}
	return qURL
}
