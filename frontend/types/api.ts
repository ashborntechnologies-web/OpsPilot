export interface User {
  id: string;
  clerk_id: string;
  email: string;
  created_at: string;
  updated_at: string;
}

export interface AWSAccount {
  id: string;
  user_id: string;
  label: string;
  aws_account_id: string;
  iam_role_arn: string;
  created_at: string;
  updated_at: string;
}

export type Framework =
  | "fastapi" | "flask" | "django" | "python"
  | "nodejs" | "express" | "nestjs" | "nextjs" | "remix" | "nuxtjs" | "svelte" | "astro"
  | "react-spa" | "vite"
  | "go"
  | "rails"
  | "spring"
  | "static";

export interface Project {
  id: string;
  user_id: string;
  name: string;
  repo_url: string;
  repo_owner: string;
  repo_name: string;
  framework: Framework;
  branch: string | null;
  start_command: string | null;
  account_id: string | null;
  github_webhook_id?: number | null;
  previews_enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface PlatformStack {
  id: string;
  account_id: string;
  aws_region: string;
  cloudformation_stack_id: string | null;
  stack_status: "pending" | "provisioning" | "ready" | "failed";
  ecs_cluster_name: string | null;
  alb_arn: string | null;
  alb_dns: string | null;
  alb_listener_arn: string | null;
  alb_security_group_id: string | null;
  ecs_security_group_id: string | null;
  subnet_ids: string | null;
  created_at: string;
  updated_at: string;
}

export interface Environment {
  id: string;
  project_id: string;
  name: string; // "staging" | "production" | "pr-N" for previews
  aws_region: string;
  account_id: string | null;
  platform_stack_id: string | null;
  cloudformation_stack_id: string | null;
  stack_status: "pending" | "provisioning" | "ready" | "failed";
  alb_dns: string | null;
  ecr_repo_uri: string | null;
  ecs_cluster_name: string | null;
  ecs_service_name: string | null;
  codebuild_project_name: string | null;
  task_execution_role_arn: string | null;
  log_group_name: string | null;
  alb_target_group_arn: string | null;
  alb_listener_rule_arn: string | null;
  ecs_security_group_id: string | null;
  vpc_subnets: string | null;
  // PR preview fields
  is_preview: boolean;
  pr_number: number | null;
  pr_branch: string | null;
  pr_head_sha: string | null;
  created_at: string;
  updated_at: string;
}

export interface CostSummary {
  total_monthly_cost: number;
  by_service: Record<string, number>;
  currency: string;
  period_start: string;
  period_end: string;
}

export interface Deployment {
  id: string;
  project_id: string;
  environment_id: string;
  commit_sha: string;
  commit_message: string | null;
  image_uri: string | null;
  status: "pending" | "building" | "deploying" | "live" | "failed" | "rolled_back";
  failure_reason: string | null;
  created_at: string;
  updated_at: string;
}

export interface GithubRepo {
  name: string;
  full_name: string;
  html_url: string;
  private: boolean;
  description: string | null;
}

export interface WsMessage {
  type:
    | "auth_ok"
    | "thinking"
    | "response"
    | "deploy_progress"
    | "deploy_done"
    | "deploy_failed"
    | "provision_progress"
    | "provision_done"
    | "provision_failed"
    | "alert"
    | "alert_resolved"
    | "deploy_risk"
    | "build_log"
    | "runtime_event"
    | "error";
  payload: string;
}

export interface Alert {
  id: string;
  project_id: string;
  environment_id: string | null;
  alert_type: string;
  severity: "warn" | "error" | "info";
  title: string;
  summary: string;
  status: "open" | "resolved" | "snoozed";
  triggered_at: string;
  resolved_at: string | null;
  snoozed_until: string | null;
}

export interface RiskScore {
  score: number;
  level: "low" | "medium" | "high" | "critical";
  factors: { name: string; points: number; reason: string }[];
  explanation: string;
}

export interface UserMe {
  email: string;
  plan: "free" | "pro" | "team";
  ai_actions_this_month: number;
  ai_actions_limit: number;
  projects_count: number;
  projects_limit: number;
  notifications: {
    enabled: boolean;
    deploy_failed: boolean;
    deploy_succeeded: boolean;
    alert_fired: boolean;
  };
}

export interface ConversationMessage {
  id: string;
  role: "user" | "assistant";
  message: string;
  intent?: string;
  created_at: string;
}

export interface EnvVar {
  id: string;
  environment_id: string;
  key: string;
  value: string; // "***" for secrets in list responses
  is_secret: boolean;
  created_at: string;
  updated_at: string;
}

export interface Webhook {
  id: string;
  project_id: string;
  url: string;
  events: string[];
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface OperationalEvent {
  id: string;
  project_id: string;
  environment_id: string | null;
  deployment_id: string | null;
  event_type: string;
  severity: "info" | "warn" | "error";
  source: string;
  actor_type: "system" | "user" | "ai";
  payload: Record<string, unknown> | null;
  occurred_at: string;
}
