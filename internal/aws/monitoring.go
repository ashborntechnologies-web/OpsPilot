// monitoring.go contains the AWS calls used by the continuous monitoring
// subsystem (internal/monitor): ALB metric retrieval for the health poller,
// CodeBuild log streaming for live build output, and build cancellation.
package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
)

// ALBMetrics is a 2-minute window of load balancer health signals.
type ALBMetrics struct {
	Request5xxCount float64 `json:"request_5xx_count"`
	RequestCount    float64 `json:"request_count"`
	P99LatencyMs    float64 `json:"p99_latency_ms"`
}

// albDimensionFromARN converts an ALB ARN to the CloudWatch "LoadBalancer"
// dimension value (the ARN suffix after "loadbalancer/"):
// arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/name/abc → app/name/abc
func albDimensionFromARN(arn string) string {
	if i := strings.Index(arn, "loadbalancer/"); i != -1 {
		return arn[i+len("loadbalancer/"):]
	}
	return ""
}

// tgDimensionFromARN converts a target group ARN to the CloudWatch
// "TargetGroup" dimension value (the suffix starting at "targetgroup/").
func tgDimensionFromARN(arn string) string {
	if i := strings.Index(arn, "targetgroup/"); i != -1 {
		return arn[i:]
	}
	return ""
}

// GetALBMetrics returns 5xx count, request count, and p99 latency for the last
// 2 minutes. Returns zero values when the ALB ARN is empty (environment not yet
// deployed) so the poller can treat "no traffic" and "no ALB" identically.
func (s *Service) GetALBMetrics(ctx context.Context, clients *ClientBundle, albArn, targetGroupArn string) (*ALBMetrics, error) {
	m := &ALBMetrics{}
	lbDim := albDimensionFromARN(albArn)
	if lbDim == "" {
		return m, nil
	}

	dims := []cwtypes.Dimension{{Name: aws.String("LoadBalancer"), Value: aws.String(lbDim)}}
	latencyDims := dims
	if tg := tgDimensionFromARN(targetGroupArn); tg != "" {
		// Scope latency + 5xx to the environment's target group where possible.
		latencyDims = append([]cwtypes.Dimension{{Name: aws.String("TargetGroup"), Value: aws.String(tg)}}, dims...)
	}

	end := time.Now()
	start := end.Add(-2 * time.Minute)
	period := int32(120)

	out, err := clients.Metrics.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(start),
		EndTime:   aws.Time(end),
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{
				Id: aws.String("fivexx"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/ApplicationELB"),
						MetricName: aws.String("HTTPCode_Target_5XX_Count"),
						Dimensions: latencyDims,
					},
					Period: aws.Int32(period),
					Stat:   aws.String("Sum"),
				},
			},
			{
				Id: aws.String("requests"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/ApplicationELB"),
						MetricName: aws.String("RequestCount"),
						Dimensions: dims,
					},
					Period: aws.Int32(period),
					Stat:   aws.String("Sum"),
				},
			},
			{
				Id: aws.String("latency"),
				MetricStat: &cwtypes.MetricStat{
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/ApplicationELB"),
						MetricName: aws.String("TargetResponseTime"),
						Dimensions: latencyDims,
					},
					Period: aws.Int32(period),
					Stat:   aws.String("p99"),
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch ALB metrics: %w", err)
	}

	for _, r := range out.MetricDataResults {
		if len(r.Values) == 0 {
			continue
		}
		v := r.Values[0]
		switch aws.ToString(r.Id) {
		case "fivexx":
			m.Request5xxCount = v
		case "requests":
			m.RequestCount = v
		case "latency":
			m.P99LatencyMs = v * 1000 // TargetResponseTime is in seconds
		}
	}
	return m, nil
}

// StreamCodeBuildLogs reads CodeBuild log events newer than `since` and invokes
// callback per line. Returns the timestamp of the last event seen, for
// cursor-based polling. The build ID has the form "project-name:stream-uuid".
func (s *Service) StreamCodeBuildLogs(ctx context.Context, clients *ClientBundle, buildID string, since time.Time, callback func(line string)) (time.Time, error) {
	parts := strings.SplitN(buildID, ":", 2)
	if len(parts) != 2 {
		return since, fmt.Errorf("unexpected build ID format %q", buildID)
	}
	logGroup := "/aws/codebuild/" + parts[0]
	logStream := parts[1]

	startMillis := since.UnixMilli() + 1 // GetLogEvents startTime is inclusive
	out, err := clients.CloudWatch.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(logGroup),
		LogStreamName: aws.String(logStream),
		StartTime:     aws.Int64(startMillis),
		StartFromHead: aws.Bool(true),
	})
	if err != nil {
		// The stream doesn't exist for the first few seconds of a build — not an error.
		if strings.Contains(err.Error(), "ResourceNotFoundException") {
			return since, nil
		}
		return since, fmt.Errorf("failed to read build logs: %w", err)
	}

	last := since
	events := out.Events
	sort.Slice(events, func(i, j int) bool {
		return aws.ToInt64(events[i].Timestamp) < aws.ToInt64(events[j].Timestamp)
	})
	for _, e := range events {
		line := strings.TrimRight(aws.ToString(e.Message), "\n")
		if line != "" {
			callback(line)
		}
		if ts := time.UnixMilli(aws.ToInt64(e.Timestamp)); ts.After(last) {
			last = ts
		}
	}
	return last, nil
}

// StopCodeBuildJob cancels an in-flight CodeBuild build.
func (s *Service) StopCodeBuildJob(ctx context.Context, clients *ClientBundle, buildID string) error {
	_, err := clients.CodeBuild.StopBuild(ctx, &codebuild.StopBuildInput{
		Id: aws.String(buildID),
	})
	if err != nil {
		return fmt.Errorf("failed to stop build %s: %w", buildID, err)
	}
	return nil
}
