// Package notify delivers email notifications (alert fired, deploy succeeded or
// failed) over SMTP with STARTTLS. When SMTP is not configured, the service
// degrades to a no-op that logs the would-be email so the rest of the platform
// never has to nil-check.
package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// EmailService sends transactional notification emails. Construct with
// NewEmailService; a partially configured service becomes a logging no-op.
type EmailService struct {
	smtpHost    string
	smtpPort    string
	smtpUser    string
	smtpPass    string
	fromAddress string
	enabled     bool
}

// NewEmailService returns a working sender when all parameters are set, and a
// logging no-op otherwise.
func NewEmailService(host, port, user, pass, from string) *EmailService {
	enabled := host != "" && port != "" && user != "" && pass != "" && from != ""
	if !enabled {
		slog.Info("email notifications disabled — SMTP not fully configured", "component", "notify")
	}
	return &EmailService{
		smtpHost:    host,
		smtpPort:    port,
		smtpUser:    user,
		smtpPass:    pass,
		fromAddress: from,
		enabled:     enabled,
	}
}

// SendAlert emails an infrastructure alert notification.
func (e *EmailService) SendAlert(ctx context.Context, toEmail, projectName, envName, alertTitle, summary, alertURL string) error {
	subject := fmt.Sprintf("⚠️ OpsPilot Alert: %s — %s", alertTitle, projectName)
	body := e.renderBody(
		"#b45309", // amber-700
		alertTitle,
		fmt.Sprintf("Project <strong>%s</strong>, environment <strong>%s</strong>", html.EscapeString(projectName), html.EscapeString(envName)),
		summary,
		alertURL, "View in OpsPilot",
	)
	return e.send(ctx, toEmail, subject, body)
}

// SendDeployResult emails a deploy success/failure notification.
func (e *EmailService) SendDeployResult(ctx context.Context, toEmail, projectName, envName, status, commitSHA, url string) error {
	short := commitSHA
	if len(short) > 8 {
		short = short[:8]
	}
	var subject, color, headline string
	if status == "live" || status == "succeeded" {
		subject = fmt.Sprintf("✅ Deploy succeeded: %s/%s", projectName, envName)
		color = "#15803d" // green-700
		headline = "Deployment succeeded"
	} else {
		subject = fmt.Sprintf("❌ Deploy failed: %s/%s", projectName, envName)
		color = "#b91c1c" // red-700
		headline = "Deployment failed"
	}
	body := e.renderBody(
		color,
		headline,
		fmt.Sprintf("Project <strong>%s</strong>, environment <strong>%s</strong>, commit <code>%s</code>",
			html.EscapeString(projectName), html.EscapeString(envName), html.EscapeString(short)),
		"",
		url, "Open project",
	)
	return e.send(ctx, toEmail, subject, body)
}

// SendOrgInvite emails an invitation to join a team workspace.
func (e *EmailService) SendOrgInvite(ctx context.Context, toEmail, orgName, role, acceptURL string) error {
	subject := fmt.Sprintf("You've been invited to %s on OpsPilot", orgName)
	body := e.renderBody(
		"#4f46e5", // indigo-600
		"Workspace invitation",
		fmt.Sprintf("You've been invited to join <strong>%s</strong> as <strong>%s</strong>.",
			html.EscapeString(orgName), html.EscapeString(role)),
		"Click below to accept. This invite expires in 7 days.",
		acceptURL, "Accept invitation",
	)
	return e.send(ctx, toEmail, subject, body)
}

// SendDailySummary emails the AI morning briefing (paragraph + recommendations).
func (e *EmailService) SendDailySummary(ctx context.Context, toEmail, orgName, date, paragraph string, recommendations []string, url string) error {
	subject := fmt.Sprintf("☀️ OpsPilot daily summary — %s", orgName)
	// detail is rendered as raw HTML by renderBody (workspace line + recommendations list);
	// the plain-text paragraph goes through the escaped summary slot.
	var detail strings.Builder
	fmt.Fprintf(&detail, "Workspace <strong>%s</strong>", html.EscapeString(orgName))
	if len(recommendations) > 0 {
		detail.WriteString(`<p style="margin:12px 0 4px;font-size:13px;font-weight:600;color:#18181b;">Recommendations</p><ul style="margin:0;padding-left:18px;font-size:14px;color:#3f3f46;">`)
		for _, r := range recommendations {
			detail.WriteString("<li>" + html.EscapeString(r) + "</li>")
		}
		detail.WriteString("</ul>")
	}
	body := e.renderBody("#4f46e5", fmt.Sprintf("Daily summary — %s", date), detail.String(), paragraph, url, "View in OpsPilot")
	return e.send(ctx, toEmail, subject, body)
}

// SendMonthlyReport emails the monthly operational health report's executive summary with a
// link to the analytics dashboard. Sent to org admins.
func (e *EmailService) SendMonthlyReport(ctx context.Context, toEmail, orgName, month, summary, url string) error {
	subject := fmt.Sprintf("📊 OpsPilot monthly health report — %s (%s)", orgName, month)
	detail := fmt.Sprintf("Operational health for <strong>%s</strong> · %s", html.EscapeString(orgName), html.EscapeString(month))
	body := e.renderBody("#4f46e5", "Monthly operational health report", detail, summary, url, "View full dashboard")
	return e.send(ctx, toEmail, subject, body)
}

// renderBody produces a minimal HTML email in the OpsPilot zinc/indigo theme.
func (e *EmailService) renderBody(accentColor, headline, detail, summary, linkURL, linkLabel string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><body style="margin:0;padding:24px;background:#fafafa;font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;color:#18181b;">`)
	b.WriteString(`<div style="max-width:520px;margin:0 auto;background:#ffffff;border:1px solid #e4e4e7;border-radius:8px;padding:24px;">`)
	b.WriteString(`<p style="margin:0 0 16px;font-size:13px;font-weight:600;color:#4f46e5;">OpsPilot</p>`)
	b.WriteString(fmt.Sprintf(`<h1 style="margin:0 0 8px;font-size:18px;color:%s;">%s</h1>`, accentColor, html.EscapeString(headline)))
	b.WriteString(fmt.Sprintf(`<p style="margin:0 0 12px;font-size:14px;color:#52525b;">%s</p>`, detail))
	if summary != "" {
		b.WriteString(fmt.Sprintf(`<p style="margin:0 0 16px;font-size:14px;line-height:1.5;color:#18181b;">%s</p>`, html.EscapeString(summary)))
	}
	if linkURL != "" {
		b.WriteString(fmt.Sprintf(`<a href="%s" style="display:inline-block;padding:8px 16px;background:#4f46e5;color:#ffffff;border-radius:6px;font-size:13px;text-decoration:none;">%s</a>`,
			linkURL, html.EscapeString(linkLabel)))
	}
	b.WriteString(`<p style="margin:24px 0 0;font-size:11px;color:#a1a1aa;">You are receiving this because notifications are enabled in your OpsPilot settings.</p>`)
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// send delivers a message via SMTP with STARTTLS. No-op (logged) when disabled.
func (e *EmailService) send(ctx context.Context, to, subject, htmlBody string) error {
	if !e.enabled {
		slog.Info("email skipped (SMTP not configured)", "component", "notify", "to", to, "subject", subject)
		return nil
	}
	if to == "" {
		return fmt.Errorf("empty recipient address")
	}

	msg := strings.Join([]string{
		"From: OpsPilot <" + e.fromAddress + ">",
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"",
		htmlBody,
	}, "\r\n")

	addr := net.JoinHostPort(e.smtpHost, e.smtpPort)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}

	client, err := smtp.NewClient(conn, e.smtpHost)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: e.smtpHost}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", e.smtpUser, e.smtpPass, e.smtpHost)); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err := client.Mail(e.fromAddress); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return client.Quit()
}
