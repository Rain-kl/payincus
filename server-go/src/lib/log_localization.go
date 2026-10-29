// Port of server/src/lib/log-localization.ts.
//
// normalizeLogContentForStorage() rewrites human log strings to Chinese before
// they are persisted, so the audit list and CSV export show a single language.
//
// Note on the db registration hook below: server-go/src/lib already imports
// server-go/src/db (token_invalidation.go / operation_verification.go), so the
// db package cannot import lib without creating an import cycle. The db layer
// therefore exposes RegisterLogNormalizer and the canonical implementation here
// wires itself in during init(). See db/logs.go for the other side.
package lib

import (
	"fmt"
	"regexp"

	"github.com/rain-kl/payincus/server-go/src/db"
)

func init() {
	db.RegisterLogNormalizer(NormalizeLogContentForStorage)
}

// ==================== translateLogDetails ====================

type logReplacement struct {
	re          *regexp.Regexp
	replacement string
}

var logDetailReplacements = []logReplacement{
	{regexp.MustCompile(`\bMemory:`), "内存:"},
	{regexp.MustCompile(`\bDisk:`), "磁盘:"},
	{regexp.MustCompile(`\burl:`), "地址:"},
	{regexp.MustCompile(`\blocation:`), "地区:"},
	{regexp.MustCompile(`\bcountry:`), "国家:"},
	{regexp.MustCompile(`\bstorage:`), "存储:"},
	{regexp.MustCompile(`\binstanceType:`), "实例类型:"},
	{regexp.MustCompile(`\bnetwork:`), "网络:"},
	{regexp.MustCompile(`\bhosts:`), "节点:"},
	{regexp.MustCompile(`\bhost:`), "节点:"},
	{regexp.MustCompile(`\barch:`), "架构:"},
	{regexp.MustCompile(`\bminor changes\b`), "少量变更"},
	{regexp.MustCompile(`\bunknown\b`), "未知"},
	{regexp.MustCompile(`\bN/A\b`), "无"},
}

func translateLogDetails(details string) string {
	for _, r := range logDetailReplacements {
		details = r.re.ReplaceAllString(details, r.replacement)
	}
	return details
}

// ==================== translateLogError ====================

var logErrorReplacements = []logReplacement{
	{regexp.MustCompile(`Failed creating instance record:`), "创建实例记录失败："},
	{regexp.MustCompile(`创建实例记录失败：\s*`), "创建实例记录失败："},
	{regexp.MustCompile(`Failed to create instance:\s*`), "创建实例失败："},
	{regexp.MustCompile(`Instance type "virtual-machine" is not supported on this server:\s*`), "当前服务器不支持虚拟机实例类型："},
	{regexp.MustCompile(`KVM support is missing \(no /dev/kvm\)`), "缺少 KVM 支持（没有 /dev/kvm）"},
	{regexp.MustCompile(`Failed to run:`), "执行失败："},
	{regexp.MustCompile(`(?i)not authorized`), "未授权"},
	{regexp.MustCompile(`(?i)request returned error:`), "请求返回错误："},
}

func translateLogError(message string) string {
	for _, r := range logErrorReplacements {
		message = r.re.ReplaceAllString(message, r.replacement)
	}
	return message
}

// ==================== normalizeLogContentForStorage ====================

var logExactContent = map[string]string{
	"password":                           "修改密码",
	"Started 2FA setup":                  "开始设置双因素认证",
	"Enabled two-factor authentication":  "启用双因素认证",
	"Disabled two-factor authentication": "禁用双因素认证",
	"Regenerated 2FA recovery codes":     "重新生成双因素认证恢复码",
	"2FA disabled due to password reset": "因重置密码禁用双因素认证",
}

var (
	logBatchDeleteRe  = regexp.MustCompile(`^Batch deleted instance "([^"]+)" on host "([^"]+)"(?: \(refund: ¥([^)]+)\))?$`)
	logImagePolicyRe  = regexp.MustCompile(`^Updated image policy for host "([^"]+)" with (\d+) image\(s\)$`)
	logUpdateStartRe  = regexp.MustCompile(`^Started system update from (\S+) to (\S+)$`)
	logUpdateRollRe   = regexp.MustCompile(`^Started rollback for system update task #(\d+)$`)
	logUpdateDenyRe   = regexp.MustCompile(`^Denied system update access for (.+)$`)
	logCreateInstRe   = regexp.MustCompile(`^Failed to create instance "([^"]+)": (.+)$`)
	logCreatePlanRe   = regexp.MustCompile(`^Created plan "([^"]+)" for package "([^"]+)"$`)
	logUpdatePlanRe   = regexp.MustCompile(`^Updated plan "([^"]+)" for package "([^"]+)"$`)
	logDeletePlanRe   = regexp.MustCompile(`^Deleted plan "([^"]+)" from package "([^"]+)"$`)
	logSyncQuotaRe    = regexp.MustCompile(`^Synced quota to (\d+) instance\(s\) for plan "([^"]+)"$`)
	logCreatePkgRe    = regexp.MustCompile(`^Created package "([^"]+)" \[(.+)\]$`)
	logUpdatePkgRe    = regexp.MustCompile(`^Updated package "([^"]+)" \[(.+)\]$`)
	logDeletePkgRe    = regexp.MustCompile(`^Deleted package "([^"]+)" \[(.+)\]$`)
	logReleaseQuotaRe = regexp.MustCompile(`^Released quota for package "([^"]+)": CPU \+([^,]+), Memory \+([^ ]+) on (\d+) host\(s\)$`)
	logCreateHostRe   = regexp.MustCompile(`^Created host "([^"]+)" \[(.+)\]$`)
	logUpdateHostRe   = regexp.MustCompile(`^Updated host "([^"]+)" \[(.+)\]$`)
	logVerifyHostRe   = regexp.MustCompile(`^Verified and connected host "([^"]+)" \[(.+)\]$`)
	logCreatePoolRe   = regexp.MustCompile(`^Created storage pool "([^"]+)" \(([^)]+)\) on host "([^"]+)"$`)
	logQueueTaskRe    = regexp.MustCompile(`^Queued (start|stop|restart) task for instance "([^"]+)"$`)
	logQueueImageRe   = regexp.MustCompile(`^Queued (rebuild|recreate) task for instance "([^"]+)" with image (.+)$`)
	logSuspendRe      = regexp.MustCompile(`^Suspended instance "([^"]+)"(?: \(reason: (.+)\))?$`)
	logUnsuspendRe    = regexp.MustCompile(`^Unsuspended instance "([^"]+)"$`)
	logAddPortRe      = regexp.MustCompile(`^Added port mapping for instance "([^"]+)" \[(.+)\]$`)
	logDeletePortRe   = regexp.MustCompile(`^Deleted port mapping for instance "([^"]+)" \[(.+)\]$`)
	logPortFailRe     = regexp.MustCompile(`^Failed to (add|delete) port mapping: (.+)$`)
)

var (
	queuedTaskActionMap  = map[string]string{"start": "启动", "stop": "停止", "restart": "重启"}
	queuedImageActionMap = map[string]string{"rebuild": "重装", "recreate": "重建"}
)

// NormalizeLogContentForStorage mirrors normalizeLogContentForStorage().
func NormalizeLogContentForStorage(content string) string {
	if value, ok := logExactContent[content]; ok {
		return value
	}

	if m := logBatchDeleteRe.FindStringSubmatch(content); m != nil {
		suffix := ""
		if m[3] != "" {
			suffix = fmt.Sprintf("（退款：¥%s）", m[3])
		}
		return fmt.Sprintf("批量删除节点 \"%s\" 上的实例 \"%s\"%s", m[2], m[1], suffix)
	}

	if m := logImagePolicyRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("更新节点 \"%s\" 的镜像策略，共 %s 个镜像", m[1], m[2])
	}

	if m := logUpdateStartRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("开始系统更新：%s -> %s", m[1], m[2])
	}

	if m := logUpdateRollRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("开始回滚系统更新任务 #%s", m[1])
	}

	if m := logUpdateDenyRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("拒绝用户 %s 执行系统更新", m[1])
	}

	if m := logCreateInstRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("创建实例 \"%s\" 失败：%s", m[1], translateLogError(m[2]))
	}

	if m := logCreatePlanRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("为套餐 \"%s\" 创建方案 \"%s\"", m[2], m[1])
	}

	if m := logUpdatePlanRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("更新套餐 \"%s\" 的方案 \"%s\"", m[2], m[1])
	}

	if m := logDeletePlanRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("从套餐 \"%s\" 删除方案 \"%s\"", m[2], m[1])
	}

	if m := logSyncQuotaRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("已将方案 \"%s\" 的配额同步到 %s 个实例", m[2], m[1])
	}

	if m := logCreatePkgRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("创建套餐 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logUpdatePkgRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("更新套餐 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logDeletePkgRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("删除套餐 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logReleaseQuotaRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("为套餐 \"%s\" 释放配额：CPU +%s，内存 +%s，作用节点 %s 个", m[1], m[2], m[3], m[4])
	}

	if m := logCreateHostRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("创建节点 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logUpdateHostRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("更新节点 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logVerifyHostRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("验证并连接节点 \"%s\" [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logCreatePoolRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("在节点 \"%s\" 创建存储池 \"%s\"（%s）", m[3], m[1], m[2])
	}

	if m := logQueueTaskRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("已为实例 \"%s\" 创建%s任务", m[2], queuedTaskActionMap[m[1]])
	}

	if m := logQueueImageRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("已为实例 \"%s\" 创建%s任务，镜像 %s", m[2], queuedImageActionMap[m[1]], m[3])
	}

	if m := logSuspendRe.FindStringSubmatch(content); m != nil {
		reason := ""
		if m[2] != "" {
			reason = fmt.Sprintf("（原因：%s）", m[2])
		}
		return fmt.Sprintf("暂停实例 \"%s\"%s", m[1], reason)
	}

	if m := logUnsuspendRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("恢复实例 \"%s\"", m[1])
	}

	if m := logAddPortRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("为实例 \"%s\" 添加端口映射 [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logDeletePortRe.FindStringSubmatch(content); m != nil {
		return fmt.Sprintf("删除实例 \"%s\" 的端口映射 [%s]", m[1], translateLogDetails(m[2]))
	}

	if m := logPortFailRe.FindStringSubmatch(content); m != nil {
		action := "删除"
		if m[1] == "add" {
			action = "添加"
		}
		return fmt.Sprintf("%s端口映射失败：%s", action, translateLogError(m[2]))
	}

	return translateLogError(content)
}
