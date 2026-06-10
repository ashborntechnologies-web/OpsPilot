package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/ashborntechnologies-web/OpsPilot/internal/awstags"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/google/uuid"
)

// BootstrapTemplate is the small stack users deploy once per AWS account.
// It creates a single scoped IAM role that ConvDeploy assumes to manage infra.
// Parameters: ConvDeployAccountId (our platform's AWS account ID).
const BootstrapTemplate = `AWSTemplateFormatVersion: '2010-09-09'
Description: >
  ConvDeploy bootstrap - creates the platform access role that lets ConvDeploy
  manage deployment infrastructure in this AWS account.

Parameters:
  ConvDeployAccountId:
    Type: String
    Description: The AWS account ID of the ConvDeploy platform (provided in the UI).

Resources:
  ConvDeployRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: ConvDeployPlatformRole
      Description: Assumed by ConvDeploy to manage ECS, ECR, CodeBuild and networking.
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal:
              AWS: !Sub 'arn:aws:iam::${ConvDeployAccountId}:root'
            Action: sts:AssumeRole
            Condition:
              StringEquals:
                sts:ExternalId: CONVDEPLOY_EXTERNAL_ID
      Policies:
        - PolicyName: ConvDeployPermissions
          PolicyDocument:
            Version: '2012-10-17'
            Statement:
              - Sid: CloudFormation
                Effect: Allow
                Action: cloudformation:*
                Resource: !Sub 'arn:aws:cloudformation:*:${AWS::AccountId}:stack/cd-*'
              - Sid: ECR
                Effect: Allow
                Action:
                  - ecr:CreateRepository
                  - ecr:DeleteRepository
                  - ecr:DescribeRepositories
                  - ecr:GetAuthorizationToken
                  - ecr:BatchGetImage
                  - ecr:PutImage
                  - ecr:InitiateLayerUpload
                  - ecr:UploadLayerPart
                  - ecr:CompleteLayerUpload
                  - ecr:BatchCheckLayerAvailability
                  - ecr:PutLifecyclePolicy
                  - ecr:TagResource
                  - ecr:ListTagsForResource
                  - ecr:ListImages
                  - ecr:BatchDeleteImage
                Resource: '*'
              - Sid: ECS
                Effect: Allow
                Action:
                  - ecs:CreateCluster
                  - ecs:DeleteCluster
                  - ecs:CreateService
                  - ecs:UpdateService
                  - ecs:DeleteService
                  - ecs:RegisterTaskDefinition
                  - ecs:DeregisterTaskDefinition
                  - ecs:DescribeServices
                  - ecs:DescribeClusters
                  - ecs:DescribeTaskDefinition
                  - ecs:ListTasks
                  - ecs:DescribeTasks
                  - ecs:TagResource
                  - ecs:ExecuteCommand
                Resource: '*'
              - Sid: SSMMessages
                Effect: Allow
                Action:
                  - ssmmessages:CreateControlChannel
                  - ssmmessages:CreateDataChannel
                  - ssmmessages:OpenControlChannel
                  - ssmmessages:OpenDataChannel
                Resource: '*'
              - Sid: CodeBuild
                Effect: Allow
                Action:
                  - codebuild:CreateProject
                  - codebuild:DeleteProject
                  - codebuild:StartBuild
                  - codebuild:BatchGetBuilds
                  - codebuild:BatchGetProjects
                  - codebuild:ListBuildsForProject
                  - codebuild:UpdateProject
                  - codebuild:TagResource
                Resource: '*'
              - Sid: CloudWatchLogs
                Effect: Allow
                Action:
                  - logs:CreateLogGroup
                  - logs:DeleteLogGroup
                  - logs:DescribeLogGroups
                  - logs:DescribeLogStreams
                  - logs:GetLogEvents
                  - logs:FilterLogEvents
                  - logs:PutRetentionPolicy
                  - logs:TagLogGroup
                  - logs:TagResource
                  - logs:ListTagsLogGroup
                Resource: '*'
              - Sid: ACMRead
                Effect: Allow
                Action:
                  - acm:DescribeCertificate
                  - acm:ListCertificates
                Resource: '*'
              - Sid: ElasticLoadBalancing
                Effect: Allow
                Action:
                  - elasticloadbalancing:CreateLoadBalancer
                  - elasticloadbalancing:DeleteLoadBalancer
                  - elasticloadbalancing:CreateTargetGroup
                  - elasticloadbalancing:DeleteTargetGroup
                  - elasticloadbalancing:ModifyTargetGroupAttributes
                  - elasticloadbalancing:CreateListener
                  - elasticloadbalancing:DeleteListener
                  - elasticloadbalancing:ModifyListener
                  - elasticloadbalancing:CreateRule
                  - elasticloadbalancing:DeleteRule
                  - elasticloadbalancing:ModifyRule
                  - elasticloadbalancing:ModifyLoadBalancerAttributes
                  - elasticloadbalancing:RegisterTargets
                  - elasticloadbalancing:DeregisterTargets
                  - elasticloadbalancing:SetSecurityGroups
                  - elasticloadbalancing:SetSubnets
                  - elasticloadbalancing:AddTags
                  - elasticloadbalancing:Describe*
                Resource: '*'
              - Sid: EC2Networking
                Effect: Allow
                Action:
                  # Writes — kept specific so the platform role can only provision networking
                  - ec2:CreateVpc
                  - ec2:DeleteVpc
                  - ec2:CreateSubnet
                  - ec2:DeleteSubnet
                  - ec2:CreateInternetGateway
                  - ec2:DeleteInternetGateway
                  - ec2:AttachInternetGateway
                  - ec2:DetachInternetGateway
                  - ec2:CreateRouteTable
                  - ec2:DeleteRouteTable
                  - ec2:CreateRoute
                  - ec2:DeleteRoute
                  - ec2:AssociateRouteTable
                  - ec2:DisassociateRouteTable
                  - ec2:ModifyVpcAttribute
                  - ec2:ModifySubnetAttribute
                  - ec2:CreateSecurityGroup
                  - ec2:DeleteSecurityGroup
                  - ec2:AuthorizeSecurityGroupIngress
                  - ec2:AuthorizeSecurityGroupEgress
                  - ec2:RevokeSecurityGroupIngress
                  - ec2:RevokeSecurityGroupEgress
                  - ec2:CreateTags
                  - ec2:DeleteTags
                  # Reads — wildcard because ALB, ECS, and CloudFormation all chain
                  # internal EC2 describe calls (DescribeAccountAttributes, DescribeNetworkInterfaces,
                  # DescribeAddresses, etc.) that we can't enumerate in advance.
                  - ec2:Describe*
                Resource: '*'
              - Sid: IAMForInfra
                Effect: Allow
                Action:
                  - iam:CreateRole
                  - iam:DeleteRole
                  - iam:GetRole
                  - iam:AttachRolePolicy
                  - iam:DetachRolePolicy
                  - iam:PutRolePolicy
                  - iam:DeleteRolePolicy
                  - iam:GetRolePolicy
                  - iam:ListRolePolicies
                  - iam:ListAttachedRolePolicies
                  - iam:PassRole
                  - iam:TagRole
                Resource: !Sub 'arn:aws:iam::${AWS::AccountId}:role/cdrole-*'
              - Sid: ServiceLinkedRoles
                Effect: Allow
                Action:
                  - iam:CreateServiceLinkedRole
                  - iam:GetServiceLinkedRoleDeletionStatus
                Resource: !Sub 'arn:aws:iam::${AWS::AccountId}:role/aws-service-role/*'
              - Sid: STS
                Effect: Allow
                Action: sts:GetCallerIdentity
                Resource: '*'
              - Sid: SSMParameters
                Effect: Allow
                Action:
                  - ssm:PutParameter
                  - ssm:GetParameter
                  - ssm:DeleteParameter
                Resource: !Sub 'arn:aws:ssm:*:${AWS::AccountId}:parameter/convdeploy/*'
              - Sid: CostExplorer
                Effect: Allow
                Action:
                  - ce:GetCostAndUsage
                  - ce:GetCostForecast
                Resource: '*'

Outputs:
  RoleArn:
    Value: !GetAtt ConvDeployRole.Arn
    Description: Paste this ARN into ConvDeploy to complete AWS connection.
  ExternalId:
    Value: CONVDEPLOY_EXTERNAL_ID
    Description: External ID is already configured - no action needed.
`

// PlatformTemplate provisions shared infrastructure for a single AWS account × region.
// Deployed once; all project environments in the same account+region reuse it.
// ECS services, Target Groups, and listener rules are created by the deploy workflow via SDK.
const PlatformTemplate = `AWSTemplateFormatVersion: '2010-09-09'
Description: ConvDeploy shared platform stack - VPC, ECS cluster, and ALB shared across all projects.

Parameters:

  # Optional ACM certificate for HTTPS. When provided, the ALB serves HTTPS on 443
  # (where all routing rules attach) and port 80 redirects to it. The certificate
  # must cover the custom domain the user points at the ALB — ACM cannot issue
  # certificates for *.amazonaws.com names.
  CertificateArn:
    Type: String
    Default: ''
    Description: Optional ACM certificate ARN to enable HTTPS on the shared ALB.

Conditions:

  HasCertificate: !Not [!Equals [!Ref CertificateArn, '']]

Resources:

  # ── Networking ──────────────────────────────────────────────────────────────

  VPC:
    Type: AWS::EC2::VPC
    Properties:
      CidrBlock: 10.0.0.0/16
      EnableDnsHostnames: true
      EnableDnsSupport: true
      Tags:
        - Key: Name
          Value: !Ref AWS::StackName
        - Key: convdeploy:managed
          Value: 'true'

  SubnetA:
    Type: AWS::EC2::Subnet
    Properties:
      VpcId: !Ref VPC
      CidrBlock: 10.0.1.0/24
      AvailabilityZone: !Select [0, !GetAZs '']
      MapPublicIpOnLaunch: true
      Tags:
        - Key: Name
          Value: !Sub '${AWS::StackName}-a'

  SubnetB:
    Type: AWS::EC2::Subnet
    Properties:
      VpcId: !Ref VPC
      CidrBlock: 10.0.2.0/24
      AvailabilityZone: !Select [1, !GetAZs '']
      MapPublicIpOnLaunch: true
      Tags:
        - Key: Name
          Value: !Sub '${AWS::StackName}-b'

  InternetGateway:
    Type: AWS::EC2::InternetGateway

  VPCGatewayAttachment:
    Type: AWS::EC2::VPCGatewayAttachment
    Properties:
      VpcId: !Ref VPC
      InternetGatewayId: !Ref InternetGateway

  RouteTable:
    Type: AWS::EC2::RouteTable
    Properties:
      VpcId: !Ref VPC

  DefaultRoute:
    Type: AWS::EC2::Route
    DependsOn: VPCGatewayAttachment
    Properties:
      RouteTableId: !Ref RouteTable
      DestinationCidrBlock: 0.0.0.0/0
      GatewayId: !Ref InternetGateway

  SubnetARouteTableAssociation:
    Type: AWS::EC2::SubnetRouteTableAssociation
    Properties:
      SubnetId: !Ref SubnetA
      RouteTableId: !Ref RouteTable

  SubnetBRouteTableAssociation:
    Type: AWS::EC2::SubnetRouteTableAssociation
    Properties:
      SubnetId: !Ref SubnetB
      RouteTableId: !Ref RouteTable

  # ── Security Groups ──────────────────────────────────────────────────────────

  ALBSecurityGroup:
    Type: AWS::EC2::SecurityGroup
    Properties:
      GroupDescription: ConvDeploy shared ALB - allow HTTP/HTTPS from internet
      VpcId: !Ref VPC
      SecurityGroupIngress:
        - IpProtocol: tcp
          FromPort: 80
          ToPort: 80
          CidrIp: 0.0.0.0/0
        - IpProtocol: tcp
          FromPort: 443
          ToPort: 443
          CidrIp: 0.0.0.0/0
      SecurityGroupEgress:
        - IpProtocol: -1
          CidrIp: 0.0.0.0/0

  ECSSecurityGroup:
    Type: AWS::EC2::SecurityGroup
    Properties:
      GroupDescription: ConvDeploy ECS tasks - allow all TCP from shared ALB
      VpcId: !Ref VPC
      SecurityGroupIngress:
        - IpProtocol: tcp
          FromPort: 1
          ToPort: 65535
          SourceSecurityGroupId: !Ref ALBSecurityGroup
      SecurityGroupEgress:
        - IpProtocol: -1
          CidrIp: 0.0.0.0/0

  # ── ECS Cluster ──────────────────────────────────────────────────────────────

  ECSCluster:
    Type: AWS::ECS::Cluster
    Properties:
      ClusterName: !Ref AWS::StackName
      Tags:
        - Key: convdeploy:managed
          Value: 'true'

  # ── Shared ALB ───────────────────────────────────────────────────────────────
  # One ALB per account×region shared across all projects.
  # Projects attach via listener rules (host-based) created by the deploy workflow via SDK.

  LoadBalancer:
    Type: AWS::ElasticLoadBalancingV2::LoadBalancer
    Properties:
      Name: !Ref AWS::StackName
      Subnets:
        - !Ref SubnetA
        - !Ref SubnetB
      SecurityGroups:
        - !Ref ALBSecurityGroup
      Scheme: internet-facing
      Tags:
        - Key: convdeploy:managed
          Value: 'true'

  ALBListener:
    Type: AWS::ElasticLoadBalancingV2::Listener
    Properties:
      LoadBalancerArn: !Ref LoadBalancer
      Port: 80
      Protocol: HTTP
      DefaultActions:
        - !If
          - HasCertificate
          - Type: redirect
            RedirectConfig:
              Protocol: HTTPS
              Port: '443'
              StatusCode: HTTP_301
          - Type: fixed-response
            FixedResponseConfig:
              StatusCode: '404'
              ContentType: text/plain
              MessageBody: 'no route matched'

  # HTTPS listener — only created when a certificate is provided. Routing rules
  # attach here (instead of the HTTP listener) so all app traffic is TLS.
  ALBHttpsListener:
    Type: AWS::ElasticLoadBalancingV2::Listener
    Condition: HasCertificate
    Properties:
      LoadBalancerArn: !Ref LoadBalancer
      Port: 443
      Protocol: HTTPS
      Certificates:
        - CertificateArn: !Ref CertificateArn
      SslPolicy: ELBSecurityPolicy-TLS13-1-2-2021-06
      DefaultActions:
        - Type: fixed-response
          FixedResponseConfig:
            StatusCode: '404'
            ContentType: text/plain
            MessageBody: 'no route matched'

Outputs:

  ECSClusterName:
    Value: !Ref ECSCluster

  ALBArn:
    Value: !Ref LoadBalancer

  ALBDns:
    Value: !GetAtt LoadBalancer.DNSName

  ALBListenerArn:
    Value: !Ref ALBListener

  ALBHttpsListenerArn:
    Condition: HasCertificate
    Value: !Ref ALBHttpsListener

  ALBSecurityGroupId:
    Value: !Ref ALBSecurityGroup

  ECSSecurityGroupId:
    Value: !Ref ECSSecurityGroup

  SubnetA:
    Value: !Ref SubnetA

  SubnetB:
    Value: !Ref SubnetB
`

// ProjectTemplate provisions per-project resources: ECR, IAM roles, CodeBuild, and log group.
// Networking (VPC, cluster, ALB) comes from the shared platform stack.
// Target Groups, listener rules, and ECS services are created by the deploy workflow via SDK.
const ProjectTemplate = `AWSTemplateFormatVersion: '2010-09-09'
Description: ConvDeploy per-project resources - ECR, IAM roles, CodeBuild, and log group.

Parameters:
  ProjectId:
    Type: String
    Description: ConvDeploy internal project UUID.
  ProjectName:
    Type: String
    Description: Human-readable project name (used for tagging).
  EnvironmentName:
    Type: String
    AllowedValues: [staging, production]

Resources:

  # ── ECR ─────────────────────────────────────────────────────────────────────

  ECRRepository:
    Type: AWS::ECR::Repository
    Properties:
      RepositoryName: !Sub 'convdeploy-${ProjectId}'
      ImageScanningConfiguration:
        ScanOnPush: false
      LifecyclePolicy:
        LifecyclePolicyText: >
          {"rules":[{"rulePriority":1,"description":"Keep last 10 images",
          "selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":10},
          "action":{"type":"expire"}}]}

  # ── CloudWatch Logs ──────────────────────────────────────────────────────────

  LogGroup:
    Type: AWS::Logs::LogGroup
    Properties:
      LogGroupName: !Sub '/ecs/convdeploy-${ProjectId}-${EnvironmentName}'
      RetentionInDays: 30

  # ── IAM ─────────────────────────────────────────────────────────────────────

  TaskExecutionRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: !Sub 'cdrole-exec-${AWS::StackName}'
      Description: ECS task execution and task role - ECR pull, CloudWatch logs, ECS Exec.
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal:
              Service: ecs-tasks.amazonaws.com
            Action: sts:AssumeRole
      ManagedPolicyArns:
        - arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy
      Policies:
        - PolicyName: ECSExecPolicy
          PolicyDocument:
            Version: '2012-10-17'
            Statement:
              - Effect: Allow
                Action:
                  - ssmmessages:CreateControlChannel
                  - ssmmessages:CreateDataChannel
                  - ssmmessages:OpenControlChannel
                  - ssmmessages:OpenDataChannel
                Resource: '*'

  CodeBuildServiceRole:
    Type: AWS::IAM::Role
    Properties:
      RoleName: !Sub 'cdrole-cb-${AWS::StackName}'
      Description: CodeBuild service role - push images to ECR and write build logs.
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal:
              Service: codebuild.amazonaws.com
            Action: sts:AssumeRole
      Policies:
        - PolicyName: CodeBuildPolicy
          PolicyDocument:
            Version: '2012-10-17'
            Statement:
              - Effect: Allow
                Action:
                  - logs:CreateLogGroup
                  - logs:CreateLogStream
                  - logs:PutLogEvents
                Resource: '*'
              - Effect: Allow
                Action:
                  - ecr:GetAuthorizationToken
                  - ecr:BatchCheckLayerAvailability
                  - ecr:GetDownloadUrlForLayer
                  - ecr:BatchGetImage
                  - ecr:InitiateLayerUpload
                  - ecr:UploadLayerPart
                  - ecr:CompleteLayerUpload
                  - ecr:PutImage
                Resource: '*'
              # Resolve the SecureString GitHub token injected as a PARAMETER_STORE env var.
              - Effect: Allow
                Action:
                  - ssm:GetParameters
                Resource: !Sub 'arn:aws:ssm:*:${AWS::AccountId}:parameter/convdeploy/*'
              - Effect: Allow
                Action:
                  - kms:Decrypt
                Resource: '*'
                Condition:
                  StringEquals:
                    kms:ViaService: !Sub 'ssm.${AWS::Region}.amazonaws.com'

  # ── CodeBuild ────────────────────────────────────────────────────────────────

  CodeBuildProject:
    Type: AWS::CodeBuild::Project
    Properties:
      Name: !Ref AWS::StackName
      Description: !Sub 'ConvDeploy build project for ${ProjectName} (${EnvironmentName})'
      ServiceRole: !GetAtt CodeBuildServiceRole.Arn
      Artifacts:
        Type: NO_ARTIFACTS
      Environment:
        Type: LINUX_CONTAINER
        ComputeType: BUILD_GENERAL1_SMALL
        Image: aws/codebuild/standard:7.0
        PrivilegedMode: true
      Source:
        Type: NO_SOURCE
        BuildSpec: |
          version: 0.2
          phases:
            build:
              commands:
                - echo "Buildspec is always overridden at runtime by ConvDeploy"
      LogsConfig:
        CloudWatchLogs:
          Status: ENABLED
          GroupName: !Ref LogGroup
          StreamName: codebuild

Outputs:

  ECRRepositoryUri:
    Value: !Sub '${AWS::AccountId}.dkr.ecr.${AWS::Region}.amazonaws.com/convdeploy-${ProjectId}'

  CodeBuildProjectName:
    Value: !Ref CodeBuildProject

  TaskExecutionRoleArn:
    Value: !GetAtt TaskExecutionRole.Arn

  LogGroupName:
    Value: !Ref LogGroup
`

// ---- Stack operations -------------------------------------------------------

// DeployPlatformStack creates the shared-infrastructure CloudFormation stack (VPC, ECS cluster,
// ALB) for an account × region. Safe to call when the stack already exists — returns the
// existing stack ID in that case.
func (s *Service) DeployPlatformStack(ctx context.Context, clients *ClientBundle, accountID, region string) (string, error) {
	stackName := PlatformStackName()

	// An ACM certificate on the AWS account record enables the HTTPS listener.
	// Only applied at stack creation — existing stacks are not updated.
	certARN := ""
	if id, err := uuid.Parse(accountID); err == nil {
		s.db.Pool.QueryRow(ctx,
			`SELECT COALESCE(certificate_arn, '') FROM aws_accounts WHERE id = $1`, id,
		).Scan(&certARN)
	}

	input := &cloudformation.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(PlatformTemplate),
		Capabilities: []cftypes.Capability{cftypes.CapabilityCapabilityNamedIam},
		Parameters: []cftypes.Parameter{
			{ParameterKey: aws.String("CertificateArn"), ParameterValue: aws.String(certARN)},
		},
		// Stack-level tags propagate to every resource the stack creates.
		Tags: append(
			awstags.ToCloudFormation(awstags.BuildResourceTags("", "", s.platformAccountID)),
			cftypes.Tag{Key: aws.String("convdeploy:managed"), Value: aws.String("true")},
			cftypes.Tag{Key: aws.String("convdeploy:account-id"), Value: aws.String(accountID)},
		),
	}

	out, err := clients.CloudFormation.CreateStack(ctx, input)
	if err != nil {
		if strings.Contains(err.Error(), "AlreadyExistsException") {
			return stackName, nil
		}
		return "", fmt.Errorf("failed to create platform stack: %w", err)
	}

	return aws.ToString(out.StackId), nil
}

// WaitForPlatformStackAndPopulate polls until the platform stack reaches a terminal state,
// then writes the outputs into the platform_stacks record.
func (s *Service) WaitForPlatformStackAndPopulate(
	ctx context.Context,
	clients *ClientBundle,
	ps *models.PlatformStack,
	stackID string,
	onProgress func(string),
) error {
	for {
		out, err := clients.CloudFormation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(stackID),
		})
		if err != nil {
			return fmt.Errorf("failed to describe platform stack: %w", err)
		}
		if len(out.Stacks) == 0 {
			return fmt.Errorf("platform stack %q not found", stackID)
		}

		stack := out.Stacks[0]
		status := string(stack.StackStatus)

		switch stack.StackStatus {
		case cftypes.StackStatusCreateComplete, cftypes.StackStatusUpdateComplete:
			onProgress("Platform infrastructure ready.")
			return s.populatePlatformStackFromOutputs(ctx, ps, stack.Outputs)

		case cftypes.StackStatusCreateFailed,
			cftypes.StackStatusRollbackComplete,
			cftypes.StackStatusRollbackFailed,
			cftypes.StackStatusUpdateRollbackComplete,
			cftypes.StackStatusUpdateRollbackFailed:
			return fmt.Errorf("platform stack failed (%s): %s", status, aws.ToString(stack.StackStatusReason))
		}

		onProgress(fmt.Sprintf("Platform stack: %s...", status))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *Service) populatePlatformStackFromOutputs(ctx context.Context, ps *models.PlatformStack, outputs []cftypes.Output) error {
	vals := make(map[string]string, len(outputs))
	for _, o := range outputs {
		vals[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}

	required := []string{"ECSClusterName", "ALBArn", "ALBDns", "ALBListenerArn", "ALBSecurityGroupId", "ECSSecurityGroupId", "SubnetA", "SubnetB"}
	for _, k := range required {
		if _, ok := vals[k]; !ok {
			return fmt.Errorf("platform stack output %q missing", k)
		}
	}

	subnetIDs := vals["SubnetA"] + "," + vals["SubnetB"]

	// With a certificate, the stack emits ALBHttpsListenerArn — all routing rules
	// attach to the 443 listener (port 80 only redirects), so store IT as the
	// listener every consumer (deploys, previews) targets.
	listenerARN := vals["ALBListenerArn"]
	httpsEnabled := false
	if https, ok := vals["ALBHttpsListenerArn"]; ok && https != "" {
		listenerARN = https
		httpsEnabled = true
	}

	_, err := s.db.Pool.Exec(ctx, `
		UPDATE platform_stacks SET
			ecs_cluster_name      = $1,
			alb_arn               = $2,
			alb_dns               = $3,
			alb_listener_arn      = $4,
			alb_security_group_id = $5,
			ecs_security_group_id = $6,
			subnet_ids            = $7,
			stack_status          = $8,
			https_enabled         = $9,
			updated_at            = NOW()
		WHERE id = $10`,
		vals["ECSClusterName"],
		vals["ALBArn"],
		vals["ALBDns"],
		listenerARN,
		vals["ALBSecurityGroupId"],
		vals["ECSSecurityGroupId"],
		subnetIDs,
		models.StackStatusReady,
		httpsEnabled,
		ps.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to save platform stack outputs: %w", err)
	}

	// Refresh in-memory struct so callers can use it immediately.
	ps.ECSClusterName = aws.String(vals["ECSClusterName"])
	ps.ALBArn = aws.String(vals["ALBArn"])
	ps.ALBDNS = aws.String(vals["ALBDns"])
	ps.ALBListenerArn = aws.String(listenerARN)
	ps.ALBSecurityGroupID = aws.String(vals["ALBSecurityGroupId"])
	ps.ECSSecurityGroupID = aws.String(vals["ECSSecurityGroupId"])
	ps.SubnetIDs = aws.String(subnetIDs)
	ps.StackStatus = models.StackStatusReady
	ps.HTTPSEnabled = httpsEnabled

	return nil
}

// DeployProjectStack creates the per-project CloudFormation stack (ECR, IAM roles, CodeBuild,
// log group). Safe to call when the stack already exists.
func (s *Service) DeployProjectStack(ctx context.Context, clients *ClientBundle, env *models.Environment, project *models.Project) (string, error) {
	stackName := ProjectStackName(project.ID.String(), env.Name)

	input := &cloudformation.CreateStackInput{
		StackName:    aws.String(stackName),
		TemplateBody: aws.String(ProjectTemplate),
		Parameters: []cftypes.Parameter{
			{ParameterKey: aws.String("ProjectId"), ParameterValue: aws.String(project.ID.String())},
			{ParameterKey: aws.String("ProjectName"), ParameterValue: aws.String(project.Name)},
			{ParameterKey: aws.String("EnvironmentName"), ParameterValue: aws.String(env.Name)},
		},
		Capabilities: []cftypes.Capability{cftypes.CapabilityCapabilityNamedIam},
		// Stack-level tags propagate to every resource the stack creates (ECR repo,
		// CodeBuild project, IAM roles, log group).
		Tags: append(
			awstags.ToCloudFormation(awstags.BuildResourceTags(project.ID.String(), env.Name, s.platformAccountID)),
			cftypes.Tag{Key: aws.String("convdeploy:project-id"), Value: aws.String(project.ID.String())},
			cftypes.Tag{Key: aws.String("convdeploy:environment"), Value: aws.String(env.Name)},
		),
	}

	out, err := clients.CloudFormation.CreateStack(ctx, input)
	if err != nil {
		if strings.Contains(err.Error(), "AlreadyExistsException") {
			return stackName, nil
		}
		return "", fmt.Errorf("failed to create project stack: %w", err)
	}

	return aws.ToString(out.StackId), nil
}

// WaitForProjectStackAndPopulate polls until the project stack completes and saves outputs
// to the environments table.
func (s *Service) WaitForProjectStackAndPopulate(
	ctx context.Context,
	clients *ClientBundle,
	env *models.Environment,
	project *models.Project,
	stackID string,
	ps *models.PlatformStack,
	onProgress func(string),
) error {
	for {
		out, err := clients.CloudFormation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(stackID),
		})
		if err != nil {
			return fmt.Errorf("failed to describe project stack: %w", err)
		}
		if len(out.Stacks) == 0 {
			return fmt.Errorf("project stack %q not found", stackID)
		}

		stack := out.Stacks[0]
		status := string(stack.StackStatus)

		switch stack.StackStatus {
		case cftypes.StackStatusCreateComplete, cftypes.StackStatusUpdateComplete:
			onProgress("Project resources provisioned successfully.")
			return s.populateEnvironmentFromProjectOutputs(ctx, env, stack.Outputs, project, ps)

		case cftypes.StackStatusCreateFailed,
			cftypes.StackStatusRollbackComplete,
			cftypes.StackStatusRollbackFailed,
			cftypes.StackStatusUpdateRollbackComplete,
			cftypes.StackStatusUpdateRollbackFailed:
			return fmt.Errorf("project stack failed (%s): %s", status, aws.ToString(stack.StackStatusReason))
		}

		onProgress(fmt.Sprintf("Provisioning project resources: %s...", status))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *Service) populateEnvironmentFromProjectOutputs(
	ctx context.Context,
	env *models.Environment,
	outputs []cftypes.Output,
	project *models.Project,
	ps *models.PlatformStack,
) error {
	vals := make(map[string]string, len(outputs))
	for _, o := range outputs {
		vals[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}

	required := []string{"ECRRepositoryUri", "CodeBuildProjectName", "TaskExecutionRoleArn", "LogGroupName"}
	for _, k := range required {
		if _, ok := vals[k]; !ok {
			return fmt.Errorf("project stack output %q missing", k)
		}
	}

	serviceName := ECSServiceName(project.ID.String(), env.Name)

	_, err := s.db.Pool.Exec(ctx, `
		UPDATE environments SET
			ecr_repo_uri            = $1,
			ecs_service_name        = $2,
			codebuild_project_name  = $3,
			task_execution_role_arn = $4,
			log_group_name          = $5,
			alb_dns                 = $6,
			platform_stack_id       = $7,
			stack_status            = $8,
			updated_at              = NOW()
		WHERE id = $9`,
		vals["ECRRepositoryUri"],
		serviceName,
		vals["CodeBuildProjectName"],
		vals["TaskExecutionRoleArn"],
		vals["LogGroupName"],
		ps.ALBDNS,
		ps.ID,
		models.StackStatusReady,
		env.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to save project stack outputs: %w", err)
	}

	return nil
}

// DeleteProjectStack initiates deletion of a project CloudFormation stack and blocks until
// the stack is deleted or returns an error. It is idempotent: if the stack does not exist
// it returns nil immediately.
func (s *Service) DeleteProjectStack(ctx context.Context, clients *ClientBundle, stackID string) error {
	// Initiate deletion.
	_, err := clients.CloudFormation.DeleteStack(ctx, &cloudformation.DeleteStackInput{
		StackName: aws.String(stackID),
	})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return fmt.Errorf("failed to initiate stack deletion: %w", err)
	}

	// Poll until the stack is gone.
	for {
		out, err := clients.CloudFormation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
			StackName: aws.String(stackID),
		})
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				return nil
			}
			return fmt.Errorf("failed to describe stack during deletion: %w", err)
		}
		if len(out.Stacks) == 0 {
			return nil
		}
		switch out.Stacks[0].StackStatus {
		case cftypes.StackStatusDeleteComplete:
			return nil
		case cftypes.StackStatusDeleteFailed:
			return fmt.Errorf("stack deletion failed: %s", aws.ToString(out.Stacks[0].StackStatusReason))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// ---- Naming conventions -----------------------------------------------------

// PlatformStackName returns the CloudFormation stack name for the shared platform stack.
// One per AWS account × region (region is implicit in the CF stack's region).
func PlatformStackName() string {
	return "cd-platform"
}

// ProjectStackName returns the CloudFormation stack name for a project × environment.
func ProjectStackName(projectID, envName string) string {
	short := projectID
	if len(short) > 8 {
		short = short[:8]
	}
	shortEnv := envName
	if len(shortEnv) > 4 {
		shortEnv = shortEnv[:4]
	}
	return fmt.Sprintf("cd-%s-%s", short, shortEnv) // max 16 chars
}

// ECSServiceName returns the deterministic ECS service name for a project × environment.
func ECSServiceName(projectID, envName string) string {
	return ProjectStackName(projectID, envName)
}

// InfraStackName is kept for backward compatibility with pre-platform-stack environments.
func InfraStackName(projectID, envName string) string {
	return ProjectStackName(projectID, envName)
}
