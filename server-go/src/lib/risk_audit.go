// Port of server/src/lib/risk-audit.ts.
//
// RiskLevel / classifyLogRisk() drive the audit view (risk level, approval and
// verification requirements, batch sensitivity) and redactAuditText() scrubs
// e-mail addresses, IPs and credential-shaped tokens out of audit content.
//
// Note on the db registration hook below: server-go/src/lib already imports
// server-go/src/db, so db cannot import lib (import cycle). The db layer exposes
// RegisterLogRiskClassifier / RegisterLogAuditRedactor and this package wires
// the canonical implementations in during init(). See db/logs.go.
package lib

import (
	"regexp"
	"strings"

	"github.com/rain-kl/payincus/server-go/src/db"
)

func init() {
	db.RegisterLogRiskClassifier(func(module, action, content string) db.LogRisk {
		risk := ClassifyLogRisk(RiskAuditLogInput{Module: module, Action: action, Content: content})
		return db.LogRisk{
			RiskLevel:            string(risk.RiskLevel),
			RiskTitle:            risk.RiskTitle,
			ApprovalRequired:     risk.ApprovalRequired,
			VerificationRequired: risk.VerificationRequired,
			BatchSensitive:       risk.BatchSensitive,
		}
	})
	db.RegisterLogAuditRedactor(RedactAuditText)
}

// RiskLevel enumerates the audit risk levels.
type RiskLevel string

const (
	RiskLevelLow      RiskLevel = "low"
	RiskLevelMedium   RiskLevel = "medium"
	RiskLevelHigh     RiskLevel = "high"
	RiskLevelCritical RiskLevel = "critical"
)

// RiskOperationDefinition mirrors the TypeScript RiskOperationDefinition.
type RiskOperationDefinition struct {
	Action               string    `json:"action"`
	Module               string    `json:"module,omitempty"`
	Level                RiskLevel `json:"level"`
	Title                string    `json:"title"`
	Description          string    `json:"description"`
	ApprovalRequired     bool      `json:"approvalRequired"`
	VerificationRequired bool      `json:"verificationRequired"`
	BatchSensitive       bool      `json:"batchSensitive"`
}

// RiskAuditLogInput mirrors the TypeScript RiskAuditLogInput.
type RiskAuditLogInput struct {
	Module  string `json:"module"`
	Action  string `json:"action"`
	Content string `json:"content"`
}

// LogRisk mirrors classifyLogRisk()'s return shape.
type LogRisk struct {
	RiskLevel            RiskLevel `json:"riskLevel"`
	RiskTitle            string    `json:"riskTitle"`
	ApprovalRequired     bool      `json:"approvalRequired"`
	VerificationRequired bool      `json:"verificationRequired"`
	BatchSensitive       bool      `json:"batchSensitive"`
}

// RiskOperationDefinitions mirrors RISK_OPERATION_DEFINITIONS.
var RiskOperationDefinitions = []RiskOperationDefinition{
	{
		Action: "system.update.start", Module: "system", Level: RiskLevelCritical,
		Title: "系统在线更新", Description: "会替换生产运行版本，必须保留 OTA 任务日志和回滚证据。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "system.update.rollback", Module: "system", Level: RiskLevelCritical,
		Title: "系统回滚", Description: "会切换生产 current 指向，必须确认目标版本和数据库兼容性。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "system.config_update", Module: "system", Level: RiskLevelHigh,
		Title: "系统配置变更", Description: "可能影响注册、支付、邮件、风控或公开站点配置。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "payment_provider.create", Module: "billing", Level: RiskLevelCritical,
		Title: "新增支付通道", Description: "会影响真实收款链路，配置摘要必须脱敏记录。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "payment_provider.update", Module: "billing", Level: RiskLevelCritical,
		Title: "修改支付通道", Description: "会影响真实收款链路，密钥和回调配置不得进入日志或导出。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "payment_provider.delete", Module: "billing", Level: RiskLevelCritical,
		Title: "删除支付通道", Description: "可能导致充值不可用，必须保留操作人和结果。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "payment_provider.status", Module: "billing", Level: RiskLevelHigh,
		Title: "支付通道启停", Description: "会影响用户充值入口和回调可用性。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "balance.adjustment.request", Module: "billing", Level: RiskLevelHigh,
		Title: "余额调账申请", Description: "影响用户资金余额，必须进入审批流。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "balance.adjustment.review", Module: "billing", Level: RiskLevelCritical,
		Title: "余额调账审批", Description: "审批通过后可能写入余额流水，必须和审批单互相追溯。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "instance.batch_delete", Module: "instance", Level: RiskLevelCritical,
		Title: "批量删除实例", Description: "会删除用户资源，必须二次确认并记录影响范围。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: true,
	},
	{
		Action: "instance.delete", Module: "instance", Level: RiskLevelHigh,
		Title: "删除实例", Description: "会释放或删除用户资源，需要记录实例和执行结果。",
		ApprovalRequired: false, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "host.delete", Module: "host", Level: RiskLevelCritical,
		Title: "删除节点", Description: "会影响资源交付和现有实例管理。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "host.update", Module: "host", Level: RiskLevelHigh,
		Title: "修改节点", Description: "可能影响 Agent、Incus 和资源调度配置。",
		ApprovalRequired: false, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "package.update", Module: "package", Level: RiskLevelHigh,
		Title: "修改套餐", Description: "可能影响价格、资源配额和售卖策略。",
		ApprovalRequired: true, VerificationRequired: false, BatchSensitive: false,
	},
	{
		Action: "package.delete", Module: "package", Level: RiskLevelHigh,
		Title: "删除套餐", Description: "会影响用户下单和续费入口。",
		ApprovalRequired: true, VerificationRequired: false, BatchSensitive: false,
	},
	{
		Action: "user.role.update", Module: "user", Level: RiskLevelCritical,
		Title: "管理员权限变更", Description: "会影响后台权限边界，必须记录变更前后摘要。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
	{
		Action: "user.delete", Module: "user", Level: RiskLevelCritical,
		Title: "删除用户", Description: "可能影响账号、余额、实例和审计追溯。",
		ApprovalRequired: true, VerificationRequired: true, BatchSensitive: false,
	},
}

var riskLevelRank = map[RiskLevel]int{
	RiskLevelLow:      1,
	RiskLevelMedium:   2,
	RiskLevelHigh:     3,
	RiskLevelCritical: 4,
}

var moduleDefaultRisk = map[string]RiskLevel{
	"system":   RiskLevelMedium,
	"billing":  RiskLevelMedium,
	"host":     RiskLevelMedium,
	"package":  RiskLevelMedium,
	"user":     RiskLevelMedium,
	"instance": RiskLevelLow,
	"security": RiskLevelLow,
	"auth":     RiskLevelLow,
}

var logFailedPattern = regexp.MustCompile(`(?i)failed|失败|error|异常`)

func getOperationDefinition(log RiskAuditLogInput) *RiskOperationDefinition {
	for i := range RiskOperationDefinitions {
		definition := &RiskOperationDefinitions[i]
		if definition.Action == log.Action && (definition.Module == "" || definition.Module == log.Module) {
			return definition
		}
	}
	return nil
}

// ClassifyLogRisk mirrors classifyLogRisk().
func ClassifyLogRisk(log RiskAuditLogInput) LogRisk {
	if definition := getOperationDefinition(log); definition != nil {
		return LogRisk{
			RiskLevel:            definition.Level,
			RiskTitle:            definition.Title,
			ApprovalRequired:     definition.ApprovalRequired,
			VerificationRequired: definition.VerificationRequired,
			BatchSensitive:       definition.BatchSensitive,
		}
	}

	defaultLevel := RiskLevelLow
	if level, ok := moduleDefaultRisk[log.Module]; ok {
		defaultLevel = level
	}
	isFailed := logFailedPattern.MatchString(log.Content)
	riskLevel := defaultLevel
	if isFailed && riskLevelRank[defaultLevel] < riskLevelRank[RiskLevelMedium] {
		riskLevel = RiskLevelMedium
	}

	riskTitle := "需关注操作"
	if riskLevel == RiskLevelLow {
		riskTitle = "普通操作"
	}

	return LogRisk{
		RiskLevel:            riskLevel,
		RiskTitle:            riskTitle,
		ApprovalRequired:     false,
		VerificationRequired: false,
		BatchSensitive:       false,
	}
}

// GetRiskOperationDefinitions mirrors getRiskOperationDefinitions().
func GetRiskOperationDefinitions() []RiskOperationDefinition {
	return RiskOperationDefinitions
}

// ==================== redaction ====================

var (
	maskAuditEmailPattern = regexp.MustCompile(`(?i)\b([A-Z0-9._%+-])([A-Z0-9._%+-]*)(@[A-Z0-9.-]+\.[A-Z]{2,})\b`)
	maskAuditIpv4Pattern  = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	maskAuditIpv6Pattern  = regexp.MustCompile(`(?i)\b(?:[a-f0-9]{1,4}:){2,7}[a-f0-9]{1,4}\b`)
)

func maskAuditEmail(value string) string {
	return maskAuditEmailPattern.ReplaceAllStringFunc(value, func(match string) string {
		groups := maskAuditEmailPattern.FindStringSubmatch(match)
		if len(groups) < 4 {
			return match
		}
		first, rest, domain := groups[1], groups[2], groups[3]
		hidden := "*"
		if runes := []rune(rest); len(runes) > 1 {
			count := len(runes)
			if count > 6 {
				count = 6
			}
			hidden = strings.Repeat("*", count)
		}
		return first + hidden + domain
	})
}

func maskAuditIpv4(value string) string {
	return maskAuditIpv4Pattern.ReplaceAllStringFunc(value, func(match string) string {
		groups := maskAuditIpv4Pattern.FindStringSubmatch(match)
		if len(groups) < 4 {
			return match
		}
		return groups[1] + "." + groups[2] + "." + groups[3] + ".*"
	})
}

func maskAuditIpv6(value string) string {
	return maskAuditIpv6Pattern.ReplaceAllString(value, "[IPv6已脱敏]")
}

// RedactAuditText mirrors redactAuditText(); empty input yields an empty string.
func RedactAuditText(value string) string {
	if value == "" {
		return ""
	}
	return maskAuditIpv6(maskAuditIpv4(maskAuditEmail(SanitizeTokensInString(value))))
}

// IsRiskLevelAtLeast mirrors isRiskLevelAtLeast().
func IsRiskLevelAtLeast(level RiskLevel, threshold RiskLevel) bool {
	return riskLevelRank[level] >= riskLevelRank[threshold]
}
