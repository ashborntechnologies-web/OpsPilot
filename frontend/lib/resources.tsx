import {
  Container, Server, Database, MemoryStick, Zap, Archive, Network, Globe, Inbox, Cpu,
  type LucideIcon,
} from "lucide-react";
import type { ResourceType, DiscoveredResource } from "@/types/api";

export const RESOURCE_LABELS: Record<ResourceType, string> = {
  ecs_service: "ECS Service",
  ecs_cluster: "ECS Cluster",
  rds_instance: "RDS Instance",
  elasticache_cluster: "ElastiCache",
  lambda_function: "Lambda",
  s3_bucket: "S3 Bucket",
  alb: "Load Balancer",
  cloudfront_distribution: "CloudFront",
  sqs_queue: "SQS Queue",
  ec2_instance: "EC2 Instance",
};

export const RESOURCE_ICONS: Record<ResourceType, LucideIcon> = {
  ecs_service: Container,
  ecs_cluster: Server,
  rds_instance: Database,
  elasticache_cluster: MemoryStick,
  lambda_function: Zap,
  s3_bucket: Archive,
  alb: Network,
  cloudfront_distribution: Globe,
  sqs_queue: Inbox,
  ec2_instance: Cpu,
};

// The filterable resource types shown in the inventory dropdown.
export const RESOURCE_TYPES: ResourceType[] = [
  "ecs_service", "ecs_cluster", "rds_instance", "elasticache_cluster",
  "lambda_function", "s3_bucket", "alb", "sqs_queue", "cloudfront_distribution", "ec2_instance",
];

export function resourceLabel(t: string): string {
  return RESOURCE_LABELS[t as ResourceType] ?? t;
}

// resourceStatus pulls a human-readable status out of a resource's metadata,
// falling back to managed/discovered.
export function resourceStatus(r: DiscoveredResource): string {
  const m = r.metadata ?? {};
  const status = m["status"] ?? m["state"];
  if (typeof status === "string" && status) return status;
  return r.is_managed ? "managed" : "discovered";
}
