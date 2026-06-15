// Package trust implements environment trust levels and the approval workflow for
// AI-initiated actions. Every AI action is proposed through ProposeAction, which — based
// on the target environment's trust level and autonomous boundaries — either auto-executes
// (autonomous + within boundaries) or records a pending approval for a human. Direct human
// actions (e.g. the dashboard deploy button) bypass this and use the deploy service
// directly; trust levels apply only to AI-initiated actions.
package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ashborntechnologies-web/OpsPilot/pkg/models"
	"github.com/ashborntechnologies-web/OpsPilot/pkg/ws"
	"github.com/google/uuid"
)

// Deployer is the subset of the deploy service the trust executor needs. Injected to
// avoid a trust↔deploy import cycle (deploy.Service satisfies it).
type Deployer interface {
	TriggerDeploy(ctx context.Context, projectID uuid.UUID, envName string) (string, error)
	TriggerRollback(ctx context.Context, projectID uuid.UUID, envName string) (string, error)
	ScaleService(ctx context.Context, projectID uuid.UUID, replicas int) (string, error)
	ProposeResourceChange(ctx context.Context, projectID, userID uuid.UUID, cpu, memory string) (string, error)
	ApplyPendingMutation(ctx context.Context, projectID, userID uuid.UUID) (string, error)
}

// SlackActionNotifier posts a pending-approval action to Slack (satisfied by *slack.Service).
type SlackActionNotifier interface {
	PostActionProposal(ctx context.Context, orgID uuid.UUID, action models.AIAction, reviewURL string) error
}

// IncidentPoster appends an entry to an incident's war-room timeline (satisfied by
// *incidents.Service).
type IncidentPoster interface {
	PostActionEntry(ctx context.Context, incidentID uuid.UUID, content string)
}

type Service struct {
	db          *models.DB
	deployer    Deployer
	hub         *ws.Hub
	slack       SlackActionNotifier
	incidents   IncidentPoster
	frontendURL string
}

func NewService(db *models.DB, deployer Deployer, hub *ws.Hub, frontendURL string) *Service {
	return &Service{db: db, deployer: deployer, hub: hub, frontendURL: trimRight(frontendURL)}
}

func (s *Service) SetSlack(n SlackActionNotifier) { s.slack = n }
func (s *Service) SetIncidents(p IncidentPoster)  { s.incidents = p }

func trimRight(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

// ActionProposal describes an AI-initiated action to evaluate against the target
// environment's trust level.
type ActionProposal struct {
	OrgID            uuid.UUID
	ProjectID        uuid.UUID
	EnvName          string // target environment name (default "production")
	EnvironmentID    *uuid.UUID
	IncidentID       *uuid.UUID
	ProposedByType   string // ai | human
	ProposedByUserID *uuid.UUID
	ActionType       string
	Parameters       map[string]any
	ConfidenceScore  *float64
	Rationale        string
}

// ProposeAction evaluates a proposed AI action against the environment's trust level and
// boundaries, records it, and either auto-executes (autonomous + in-bounds) or registers
// it for human approval (broadcasting + Slack).
func (s *Service) ProposeAction(ctx context.Context, p ActionProposal) (*models.AIAction, error) {
	if p.Parameters == nil {
		p.Parameters = map[string]any{}
	}
	if p.EnvName == "" {
		p.EnvName = "production"
	}
	if p.OrgID == uuid.Nil {
		_ = s.db.Pool.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, p.ProjectID).Scan(&p.OrgID)
	}

	envID, envName, trustLevel, boundaries := s.resolveEnv(ctx, p)
	if envName != "" {
		p.Parameters["env_name"] = envName // so ExecuteAction knows the target
	}

	approvalRequired := requiresApproval(trustLevel, boundaries, p.ActionType, p.Parameters)

	paramsJSON, _ := json.Marshal(p.Parameters)
	var a models.AIAction
	err := s.db.Pool.QueryRow(ctx, `
		INSERT INTO ai_actions
		    (org_id, project_id, environment_id, incident_id, proposed_by_type, proposed_by_user_id,
		     action_type, parameters, confidence_score, rationale, status, approval_required)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending_approval',$11)
		RETURNING id, status, proposed_at, created_at`,
		p.OrgID, p.ProjectID, envID, p.IncidentID, p.ProposedByType, p.ProposedByUserID,
		p.ActionType, paramsJSON, p.ConfidenceScore, p.Rationale, approvalRequired,
	).Scan(&a.ID, &a.Status, &a.ProposedAt, &a.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("trust: record action: %w", err)
	}
	a.OrgID, a.ProjectID, a.EnvironmentID, a.IncidentID = p.OrgID, p.ProjectID, envID, p.IncidentID
	a.ProposedByType, a.ProposedByUserID = p.ProposedByType, p.ProposedByUserID
	a.ActionType, a.Parameters, a.ConfidenceScore, a.Rationale = p.ActionType, p.Parameters, p.ConfidenceScore, p.Rationale
	a.ApprovalRequired = approvalRequired

	if !approvalRequired {
		// Autonomous + within boundaries — execute immediately, no human needed.
		_ = s.ExecuteAction(ctx, a.ID, nil)
		// Re-read terminal status for the return value.
		_ = s.db.Pool.QueryRow(ctx, `SELECT status FROM ai_actions WHERE id = $1`, a.ID).Scan(&a.Status)
		return &a, nil
	}

	// Needs approval — notify the team.
	s.broadcast(p.ProjectID, "action_proposed", a)
	if s.slack != nil {
		reviewURL := s.frontendURL + "/projects/" + p.ProjectID.String()
		if err := s.slack.PostActionProposal(ctx, p.OrgID, a, reviewURL); err != nil {
			slog.Warn("trust: slack action proposal failed", "component", "trust", "action", a.ID, "error", err)
		}
	}
	return &a, nil
}

// requiresApproval applies the trust policy. suggest/supervised always require approval.
// autonomous auto-approves only actions explicitly permitted by the boundaries; deploys
// (and terminal commands) always require approval — there is no can_deploy boundary.
func requiresApproval(trustLevel string, b *models.AutonomousBoundaries, actionType string, params map[string]any) bool {
	if trustLevel != models.TrustAutonomous || b == nil {
		return true
	}
	switch actionType {
	case models.ActionRollback:
		return !b.CanRollback
	case models.ActionScale:
		if !b.CanScale {
			return true
		}
		r, ok := paramInt(params, "replicas")
		if !ok {
			return true
		}
		return r < b.MinReplicas || r > b.MaxReplicas
	case models.ActionChangeResources:
		return !b.CanChangeResources
	default:
		// deploy, terminal_command, anything else.
		return true
	}
}

// ExecuteAction runs an approved/auto-approved action and records the outcome. executorID
// is the approving user (nil for autonomous execution).
func (s *Service) ExecuteAction(ctx context.Context, actionID uuid.UUID, executorID *uuid.UUID) error {
	var (
		actionType string
		projectID  uuid.UUID
		incidentID *uuid.UUID
		paramsRaw  []byte
	)
	if err := s.db.Pool.QueryRow(ctx,
		`SELECT action_type, project_id, incident_id, parameters FROM ai_actions WHERE id = $1`, actionID,
	).Scan(&actionType, &projectID, &incidentID, &paramsRaw); err != nil {
		return err
	}
	params := map[string]any{}
	_ = json.Unmarshal(paramsRaw, &params)
	envName, _ := params["env_name"].(string)
	if envName == "" {
		envName = "production"
	}
	uid := uuid.Nil
	if executorID != nil {
		uid = *executorID
	}

	var msg string
	var execErr error
	switch actionType {
	case models.ActionDeploy:
		msg, execErr = s.deployer.TriggerDeploy(ctx, projectID, envName)
	case models.ActionRollback:
		msg, execErr = s.deployer.TriggerRollback(ctx, projectID, envName)
	case models.ActionScale:
		if r, ok := paramInt(params, "replicas"); ok {
			msg, execErr = s.deployer.ScaleService(ctx, projectID, r)
		} else {
			execErr = fmt.Errorf("scale action missing replicas")
		}
	case models.ActionChangeResources:
		cpu, _ := params["cpu"].(string)
		mem, _ := params["memory"].(string)
		if _, err := s.deployer.ProposeResourceChange(ctx, projectID, uid, cpu, mem); err != nil {
			execErr = err
		} else {
			msg, execErr = s.deployer.ApplyPendingMutation(ctx, projectID, uid)
		}
	case models.ActionTerminalCommand:
		execErr = fmt.Errorf("terminal commands cannot be auto-executed")
	default:
		execErr = fmt.Errorf("unknown action type %q", actionType)
	}

	status := models.ActionStatusExecuted
	result := map[string]any{"message": msg}
	if execErr != nil {
		status = models.ActionStatusFailed
		result = map[string]any{"error": execErr.Error()}
	}
	resultJSON, _ := json.Marshal(result)
	_, _ = s.db.Pool.Exec(ctx,
		`UPDATE ai_actions SET status = $1, executed_at = NOW(), result = $2 WHERE id = $3`,
		status, resultJSON, actionID)

	if incidentID != nil && s.incidents != nil {
		verb := "executed"
		detail := msg
		if execErr != nil {
			verb = "failed to execute"
			detail = execErr.Error()
		}
		s.incidents.PostActionEntry(ctx, *incidentID, fmt.Sprintf("AI %s a %s action: %s", verb, actionType, detail))
	}
	s.broadcast(projectID, "action_updated", map[string]any{"action_id": actionID.String(), "status": status})
	return execErr
}

// ApproveAction validates the user is engineer+ in the action's org, marks it approved,
// and executes it.
func (s *Service) ApproveAction(ctx context.Context, actionID, userID uuid.UUID) error {
	orgID, status, err := s.actionOrgStatus(ctx, actionID)
	if err != nil {
		return err
	}
	if status != models.ActionStatusPending {
		return fmt.Errorf("action is not pending approval")
	}
	if err := s.requireEngineer(ctx, userID, orgID); err != nil {
		return err
	}
	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE ai_actions SET status = 'approved', approved_by = $1, decided_at = NOW() WHERE id = $2`,
		userID, actionID); err != nil {
		return err
	}
	return s.ExecuteAction(ctx, actionID, &userID)
}

// RejectAction validates engineer+ and marks the action rejected.
func (s *Service) RejectAction(ctx context.Context, actionID, userID uuid.UUID) error {
	orgID, status, err := s.actionOrgStatus(ctx, actionID)
	if err != nil {
		return err
	}
	if status != models.ActionStatusPending {
		return fmt.Errorf("action is not pending approval")
	}
	if err := s.requireEngineer(ctx, userID, orgID); err != nil {
		return err
	}
	var incidentID *uuid.UUID
	var actionType string
	_ = s.db.Pool.QueryRow(ctx, `SELECT incident_id, action_type FROM ai_actions WHERE id = $1`, actionID).Scan(&incidentID, &actionType)

	if _, err := s.db.Pool.Exec(ctx,
		`UPDATE ai_actions SET status = 'rejected', approved_by = $1, decided_at = NOW() WHERE id = $2`,
		userID, actionID); err != nil {
		return err
	}
	if incidentID != nil && s.incidents != nil {
		s.incidents.PostActionEntry(ctx, *incidentID, fmt.Sprintf("A proposed %s action was rejected.", actionType))
	}
	s.broadcast(uuid.Nil, "action_updated", map[string]any{"action_id": actionID.String(), "status": models.ActionStatusRejected})
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (s *Service) actionOrgStatus(ctx context.Context, actionID uuid.UUID) (uuid.UUID, string, error) {
	var orgID uuid.UUID
	var status string
	err := s.db.Pool.QueryRow(ctx, `SELECT org_id, status FROM ai_actions WHERE id = $1`, actionID).Scan(&orgID, &status)
	return orgID, status, err
}

func (s *Service) requireEngineer(ctx context.Context, userID, orgID uuid.UUID) error {
	role, err := s.db.UserOrgRole(ctx, userID, orgID)
	if err != nil {
		return fmt.Errorf("not a member of this workspace")
	}
	if models.RoleRank(role) < models.RoleRank(models.RoleEngineer) {
		return fmt.Errorf("approving actions requires the engineer or admin role")
	}
	return nil
}

// resolveEnv finds the target environment (by ID, else by project+name, else any ready
// non-preview env) and returns its id, name, trust level, and parsed boundaries.
func (s *Service) resolveEnv(ctx context.Context, p ActionProposal) (*uuid.UUID, string, string, *models.AutonomousBoundaries) {
	var (
		id         uuid.UUID
		name       string
		trustLevel string
		boundsRaw  []byte
	)
	var err error
	if p.EnvironmentID != nil {
		err = s.db.Pool.QueryRow(ctx,
			`SELECT id, name, trust_level, autonomous_boundaries FROM environments WHERE id = $1`, *p.EnvironmentID,
		).Scan(&id, &name, &trustLevel, &boundsRaw)
	} else {
		err = s.db.Pool.QueryRow(ctx,
			`SELECT id, name, trust_level, autonomous_boundaries FROM environments
			 WHERE project_id = $1 AND name = $2 AND is_preview = false LIMIT 1`, p.ProjectID, p.EnvName,
		).Scan(&id, &name, &trustLevel, &boundsRaw)
		if err != nil {
			err = s.db.Pool.QueryRow(ctx,
				`SELECT id, name, trust_level, autonomous_boundaries FROM environments
				 WHERE project_id = $1 AND is_preview = false AND stack_status = 'ready'
				 ORDER BY updated_at DESC LIMIT 1`, p.ProjectID,
			).Scan(&id, &name, &trustLevel, &boundsRaw)
		}
	}
	if err != nil {
		// No environment resolved — treat as suggest (safest: always require approval).
		return nil, p.EnvName, models.TrustSuggest, nil
	}
	var bounds *models.AutonomousBoundaries
	if len(boundsRaw) > 0 {
		var b models.AutonomousBoundaries
		if json.Unmarshal(boundsRaw, &b) == nil {
			bounds = &b
		}
	}
	return &id, name, trustLevel, bounds
}

func (s *Service) broadcast(projectID uuid.UUID, msgType string, payload any) {
	if s.hub == nil || projectID == uuid.Nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.hub.Broadcast(projectID.String(), ws.Message{Type: msgType, Payload: string(b)})
}

// paramInt reads an int parameter that may arrive as a JSON number or string.
func paramInt(params map[string]any, key string) (int, bool) {
	switch v := params[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int32:
		return int(v), true
	}
	return 0, false
}
