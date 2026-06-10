package terminal

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	awssvc "github.com/ashborntechnologies-web/OpsPilot/internal/aws"
	"github.com/ashborntechnologies-web/OpsPilot/internal/auth"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/middleware"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Service handles in-browser ECS Exec terminal sessions.
// The WebSocket endpoint authenticates via a first-message token (same pattern as
// the chat hub) then proxies stdin/stdout through the SSM datachannel.
type Service struct {
	db      *models.DB
	awsSvc  awssvc.AWSProvider
	authSvc *auth.Service
}

func NewService(db *models.DB, awsSvc awssvc.AWSProvider, authSvc *auth.Service) *Service {
	return &Service{db: db, awsSvc: awsSvc, authSvc: authSvc}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true }, // auth via first message
}

// HandleTerminal upgrades the connection to WebSocket and proxies an ECS Exec shell.
//
// Protocol (browser → backend):
//
//	First message: {"type":"auth","token":"<clerk_jwt>"}
//	Subsequent:    raw bytes (stdin) OR {"type":"resize","cols":80,"rows":24}
//
// Protocol (backend → browser):
//
//	raw bytes (stdout/stderr from the container)
func (s *Service) HandleTerminal(c *gin.Context) {
	projectID, err := uuid.Parse(c.Param("projectId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}
	envID, err := uuid.Parse(c.Param("envId"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid environment id"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// ── Auth via first message ────────────────────────────────────────────────
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var authMsg struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &authMsg) != nil || authMsg.Type != "auth" || authMsg.Token == "" {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"first message must be {type:auth,token:...}"}`))
		return
	}

	userID, err := middleware.ResolveToken(c.Request.Context(), s.db, s.authSvc, authMsg.Token)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"unauthorized"}`))
		return
	}

	owned, err := s.db.UserOwnsProject(c.Request.Context(), userID, projectID)
	if err != nil || !owned {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"forbidden"}`))
		return
	}

	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth_ok"}`))
	// ─────────────────────────────────────────────────────────────────────────

	// Load environment and resolve cluster.
	env, err := s.loadEnv(c.Request.Context(), envID, projectID)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"environment not found"}`))
		return
	}
	if env.ECSServiceName == nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"environment not deployed — deploy first"}`))
		return
	}
	// Note: ECSClusterName is populated by loadEnv's COALESCE — it holds the platform
	// stack's cluster for two-tier environments, or the legacy per-env field. Nil here
	// means neither source has a cluster, i.e. broken provisioning state.
	if env.ECSClusterName == nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"could not resolve the ECS cluster for this environment — try re-provisioning"}`))
		return
	}

	clients, err := s.awsSvc.AssumeRoleForEnvironment(c.Request.Context(), env)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"failed to connect to AWS"}`))
		return
	}

	clusterName := *env.ECSClusterName
	taskARN, err := s.awsSvc.GetRunningTask(c.Request.Context(), clients, clusterName, *env.ECSServiceName)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"`+err.Error()+`"}`))
		return
	}

	streamURL, tokenValue, err := s.awsSvc.StartExecSession(c.Request.Context(), clients, clusterName, taskARN, "/bin/sh")
	if err != nil {
		// Try /bin/bash as fallback.
		streamURL, tokenValue, err = s.awsSvc.StartExecSession(c.Request.Context(), clients, clusterName, taskARN, "/bin/bash")
		if err != nil {
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"failed to start exec session: `+err.Error()+`"}`))
			return
		}
	}

	// Dial the SSM datachannel.
	ssmConn, _, err := websocket.DefaultDialer.Dial(streamURL, nil)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":"failed to connect to SSM datachannel"}`))
		return
	}
	defer ssmConn.Close()

	// Authenticate the datachannel with the token.
	openMsg, err := buildOpenMessage(tokenValue)
	if err != nil || ssmConn.WriteMessage(websocket.BinaryMessage, openMsg) != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SSM → browser: forward output_stream_data payloads.
	go func() {
		defer cancel()
		ackSeq := int64(1000) // outbound sequence counter for ACKs
		for {
			_, data, err := ssmConn.ReadMessage()
			if err != nil {
				return
			}
			msg, err := deserialize(data)
			if err != nil {
				log.Printf("[terminal] deserialize error: %v", err)
				continue
			}

			switch msg.messageType {
			case msgTypeOutput:
				if err := conn.WriteMessage(websocket.BinaryMessage, msg.payload); err != nil {
					return
				}
				if ack, err := buildAckMessage(ackSeq, msg); err == nil {
					ssmConn.WriteMessage(websocket.BinaryMessage, ack)
					ackSeq++
				}

			case msgTypeClosed:
				conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"closed"}`))
				return
			}
		}
	}()

	// Browser → SSM: forward input as input_stream_data or resize messages.
	seqNum := int64(1)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Try to parse as a control message (resize).
		var ctrl struct {
			Type string `json:"type"`
			Cols uint32 `json:"cols"`
			Rows uint32 `json:"rows"`
		}
		if json.Unmarshal(raw, &ctrl) == nil && ctrl.Type == "resize" {
			if frame, err := buildResizeMessage(seqNum, ctrl.Cols, ctrl.Rows); err == nil {
				ssmConn.WriteMessage(websocket.BinaryMessage, frame)
				seqNum++
			}
			continue
		}

		// Plain stdin bytes.
		if frame, err := buildInputMessage(seqNum, raw); err == nil {
			if err := ssmConn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
			seqNum++
		}
	}
}

func (s *Service) loadEnv(ctx context.Context, envID, projectID uuid.UUID) (*models.Environment, error) {
	var env models.Environment
	// The ECS cluster lives on the shared platform stack in the two-tier model;
	// COALESCE falls back to the legacy per-environment field for old environments.
	err := s.db.Pool.QueryRow(ctx,
		`SELECT e.id, e.project_id, e.name, e.aws_region, e.account_id, e.platform_stack_id,
		        e.cloudformation_stack_id, e.stack_status, e.alb_dns,
		        e.ecr_repo_uri,
		        COALESCE(ps.ecs_cluster_name, e.ecs_cluster_name) AS ecs_cluster_name,
		        e.ecs_service_name,
		        e.codebuild_project_name, e.task_execution_role_arn, e.log_group_name,
		        e.alb_target_group_arn, e.alb_listener_rule_arn, e.ecs_security_group_id, e.vpc_subnets,
		        e.created_at, e.updated_at
		 FROM environments e
		 LEFT JOIN platform_stacks ps ON ps.id = e.platform_stack_id
		 WHERE e.id = $1 AND e.project_id = $2`, envID, projectID,
	).Scan(
		&env.ID, &env.ProjectID, &env.Name, &env.AWSRegion,
		&env.AccountID, &env.PlatformStackID,
		&env.CloudFormationStackID, &env.StackStatus, &env.ALBDNS,
		&env.ECRRepoURI, &env.ECSClusterName, &env.ECSServiceName,
		&env.CodeBuildProjectName, &env.TaskExecutionRoleARN, &env.LogGroupName,
		&env.ALBTargetGroupARN, &env.ALBListenerRuleARN, &env.ECSSecurityGroupID, &env.VPCSubnets,
		&env.CreatedAt, &env.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &env, nil
}
