// Package awstags centralizes the resource tags OpsPilot applies to everything
// it creates in customer AWS accounts. Consistent tagging identifies OpsPilot-managed
// resources for cleanup, cost attribution per project/environment, and support.
package awstags

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

// Tag keys applied to every OpsPilot-managed resource.
const (
	KeyManagedBy   = "ManagedBy"
	KeyProjectID   = "OpsPilotProjectID"
	KeyEnvironment = "OpsPilotEnvironment"
	KeyAccountID   = "OpsPilotAccountID"

	ManagedByValue = "OpsPilot"
)

// Tag is a service-agnostic key/value pair; convert with the To* helpers below.
type Tag struct {
	Key   string
	Value string
}

// BuildResourceTags returns the standard tag set for a resource. Empty values
// are omitted (e.g. platform-level resources that aren't tied to one project).
func BuildResourceTags(projectID, envName, platformAccountID string) []Tag {
	tags := []Tag{{Key: KeyManagedBy, Value: ManagedByValue}}
	if projectID != "" {
		tags = append(tags, Tag{Key: KeyProjectID, Value: projectID})
	}
	if envName != "" {
		tags = append(tags, Tag{Key: KeyEnvironment, Value: envName})
	}
	if platformAccountID != "" {
		tags = append(tags, Tag{Key: KeyAccountID, Value: platformAccountID})
	}
	return tags
}

// ToCloudFormation converts to CloudFormation stack tags (these propagate to
// every resource the stack creates).
func ToCloudFormation(tags []Tag) []cftypes.Tag {
	out := make([]cftypes.Tag, len(tags))
	for i, t := range tags {
		out[i] = cftypes.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)}
	}
	return out
}

// ToECS converts to ECS resource tags (services, task definitions).
func ToECS(tags []Tag) []ecstypes.Tag {
	out := make([]ecstypes.Tag, len(tags))
	for i, t := range tags {
		out[i] = ecstypes.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)}
	}
	return out
}

// ToELB converts to Elastic Load Balancing v2 tags (target groups, rules).
func ToELB(tags []Tag) []elbtypes.Tag {
	out := make([]elbtypes.Tag, len(tags))
	for i, t := range tags {
		out[i] = elbtypes.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)}
	}
	return out
}
