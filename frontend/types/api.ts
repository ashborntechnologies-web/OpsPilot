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

export interface Project {
  id: string;
  user_id: string;
  name: string;
  repo_url: string;
  repo_owner: string;
  repo_name: string;
  framework: "fastapi" | "flask" | "python" | "nodejs" | "nextjs";
  branch: string | null;
  start_command: string | null;
  account_id: string | null;
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
  name: "staging" | "production";
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
  created_at: string;
  updated_at: string;
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
    | "error";
  payload: string;
}

export interface ConversationMessage {
  id: string;
  role: "user" | "assistant";
  message: string;
  intent?: string;
  created_at: string;
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
