package lib_test

import (
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestRiskOperationDefinitionsParity(t *testing.T) {
	definitions := lib.GetRiskOperationDefinitions()
	if len(definitions) != 17 {
		t.Fatalf("expected 17 risk operation definitions, got %d", len(definitions))
	}
}

func TestClassifyLogRiskMatchesDefinition(t *testing.T) {
	// Known operation definition: system.update.start is critical.
	risk := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "system", Action: "system.update.start"})
	if risk.RiskLevel != lib.RiskLevelCritical {
		t.Errorf("risk level = %q, want critical", risk.RiskLevel)
	}
	if risk.RiskTitle != "系统在线更新" {
		t.Errorf("risk title = %q, want 系统在线更新", risk.RiskTitle)
	}
	if !risk.ApprovalRequired || !risk.VerificationRequired || risk.BatchSensitive {
		t.Errorf("flags = %+v, want approval+verification true, batch false", risk)
	}

	// instance.delete: high, no approval, verification required.
	deletion := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "instance", Action: "instance.delete"})
	if deletion.RiskLevel != lib.RiskLevelHigh || deletion.ApprovalRequired || !deletion.VerificationRequired {
		t.Errorf("instance.delete = %+v, want high/no-approval/verification", deletion)
	}
}

func TestClassifyLogRiskFallsBackToModuleDefault(t *testing.T) {
	// billing defaults to medium even without a matching definition.
	medium := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "billing", Action: "billing.custom"})
	if medium.RiskLevel != lib.RiskLevelMedium || medium.RiskTitle != "需关注操作" {
		t.Errorf("billing default = %+v, want medium/需关注操作", medium)
	}

	// instance defaults to low for a non-failing message.
	low := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "instance", Action: "instance.custom", Content: "done"})
	if low.RiskLevel != lib.RiskLevelLow || low.RiskTitle != "普通操作" {
		t.Errorf("instance default = %+v, want low/普通操作", low)
	}
}

func TestClassifyLogRiskEscalatesFailures(t *testing.T) {
	// A low-default module escalates to medium when the content signals failure.
	escalated := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "instance", Action: "instance.custom", Content: "error occurred"})
	if escalated.RiskLevel != lib.RiskLevelMedium || escalated.RiskTitle != "需关注操作" {
		t.Errorf("failure escalation = %+v, want medium/需关注操作", escalated)
	}

	// A medium-default module never escalates beyond medium.
	stays := lib.ClassifyLogRisk(lib.RiskAuditLogInput{Module: "billing", Action: "billing.custom", Content: "失败"})
	if stays.RiskLevel != lib.RiskLevelMedium {
		t.Errorf("medium default = %+v, want medium", stays)
	}
}

func TestIsRiskLevelAtLeast(t *testing.T) {
	if !lib.IsRiskLevelAtLeast(lib.RiskLevelCritical, lib.RiskLevelHigh) {
		t.Error("critical should be at least high")
	}
	if lib.IsRiskLevelAtLeast(lib.RiskLevelLow, lib.RiskLevelMedium) {
		t.Error("low should not be at least medium")
	}
	if !lib.IsRiskLevelAtLeast(lib.RiskLevelMedium, lib.RiskLevelMedium) {
		t.Error("medium should be at least medium")
	}
}

func TestRedactAuditText(t *testing.T) {
	if got := lib.RedactAuditText(""); got != "" {
		t.Errorf("empty input = %q, want empty", got)
	}
	if got := lib.RedactAuditText("user@example.com"); got != "u***@example.com" {
		t.Errorf("email = %q, want u***@example.com", got)
	}
	if got := lib.RedactAuditText("ip 192.168.1.10 here"); got != "ip 192.168.1.* here" {
		t.Errorf("ipv4 = %q, want ip 192.168.1.* here", got)
	}
	if got := lib.RedactAuditText("2001:db8:85a3:8d3:1319:8a2e:370:7348"); got != "[IPv6已脱敏]" {
		t.Errorf("ipv6 = %q, want [IPv6已脱敏]", got)
	}
	if got := lib.RedactAuditText("?token=secret"); got != "?token=[REDACTED]" {
		t.Errorf("token = %q, want ?token=[REDACTED]", got)
	}
}
