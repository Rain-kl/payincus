// Port of server/src/lib/instance-audit.ts.
package lib

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AuditProcess mirrors AuditProcess in instance-audit.ts.
type AuditProcess struct {
	PID           int32    `json:"pid"`
	PPID          *int32   `json:"ppid"`
	User          string   `json:"user"`
	Stat          string   `json:"stat"`
	CPUPercent    *float64 `json:"cpuPercent"`
	MemoryPercent *float64 `json:"memoryPercent"`
	Elapsed       string   `json:"elapsed"`
	Command       string   `json:"command"`
	Args          string   `json:"args"`
	Raw           string   `json:"raw"`
	Findings      []string `json:"findings"`
}

// AuditConnection mirrors AuditConnection in instance-audit.ts.
type AuditConnection struct {
	Protocol string  `json:"protocol"`
	State    string  `json:"state"`
	Local    string  `json:"local"`
	Peer     string  `json:"peer"`
	Process  *string `json:"process"`
	PID      *int32  `json:"pid"`
	Raw      string  `json:"raw"`
}

// AuditStartupItem mirrors AuditStartupItem in instance-audit.ts.
type AuditStartupItem struct {
	Source   string   `json:"source"`
	Command  string   `json:"command"`
	Raw      string   `json:"raw"`
	Findings []string `json:"findings"`
}

// AuditFinding mirrors AuditFinding in instance-audit.ts.
type AuditFinding struct {
	ID             string  `json:"id"`
	Severity       string  `json:"severity"`
	Category       string  `json:"category"`
	Title          string  `json:"title"`
	Detail         string  `json:"detail"`
	TargetType     string  `json:"targetType"`
	RuleID         string  `json:"ruleId,omitempty"`
	RuleName       string  `json:"ruleName,omitempty"`
	RuleSource     string  `json:"ruleSource,omitempty"`
	MatchedText    string  `json:"matchedText,omitempty"`
	Recommendation *string `json:"recommendation,omitempty"`
	PID            *int32  `json:"pid,omitempty"`
	Evidence       string  `json:"evidence"`
	Ignored        bool    `json:"ignored,omitempty"`
	IgnoreReason   *string `json:"ignoreReason,omitempty"`
}

// AuditSummary mirrors AuditSummary in instance-audit.ts.
type AuditSummary struct {
	RiskLevel        string `json:"riskLevel"`
	ProcessCount     int    `json:"processCount"`
	ConnectionCount  int    `json:"connectionCount"`
	ListeningCount   int    `json:"listeningCount"`
	StartupItemCount int    `json:"startupItemCount"`
	FindingCount     int    `json:"findingCount"`
}

// AuditRuleDefinition mirrors AuditRuleDefinition in instance-audit.ts.
type AuditRuleDefinition struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"`
	Category       string   `json:"category"`
	Name           string   `json:"name"`
	Description    *string  `json:"description,omitempty"`
	TargetTypes    []string `json:"targetTypes"`
	MatchType      string   `json:"matchType"`
	PatternText    string   `json:"patternText"`
	CaseSensitive  bool     `json:"caseSensitive"`
	Source         string   `json:"source"`
	Enabled        bool     `json:"enabled"`
	Recommendation *string  `json:"recommendation,omitempty"`
}

// AuditIgnoreDefinition mirrors AuditIgnoreDefinition in instance-audit.ts.
type AuditIgnoreDefinition struct {
	ID         int32
	RuleID     *string
	TargetType *string
	MatchText  *string
	Reason     *string
	ExpiresAt  *time.Time
}

// AuditAnalysisInput mirrors analyzeAuditData parameter object.
type AuditAnalysisInput struct {
	Processes    []AuditProcess
	Connections  []AuditConnection
	StartupItems []AuditStartupItem
	Rules        []AuditRuleDefinition
	Ignores      []AuditIgnoreDefinition
}

// AuditAnalysisResult mirrors analyzeAuditData return object.
type AuditAnalysisResult struct {
	Findings []AuditFinding
	Summary  AuditSummary
}

func auditStrPtr(s string) *string { return &s }

// BuiltinAuditRules mirrors BUILTIN_AUDIT_RULES in instance-audit.ts.
var BuiltinAuditRules = []AuditRuleDefinition{
	{
		ID:             "packet-tool-hping",
		Severity:       "high",
		Category:       "network-abuse",
		Name:           "疑似发包工具进程",
		Description:    auditStrPtr("匹配常见的发包、构造数据包或压测类工具。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(hping[3]?|nping|mz|mausezahn|mzbench|pktgen|packit|nemesis|tcpreplay|scapy|flood|synflood|udpflood|icmpflood|httpflood|slowloris|goldeneye|hulk|xerxes)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("请先确认进程归属、流量用途和客户业务背景，再决定是否手动处置。"),
	},
	{
		ID:             "mass-scan-tool",
		Severity:       "high",
		Category:       "network-abuse",
		Name:           "疑似大规模扫描工具",
		Description:    auditStrPtr("匹配常见的公网大范围扫描或高频扫描工具。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(masscan|zmap|zdns|zgrab2?|naabu|rustscan|httpx|nuclei|fscan|kscan|gogo|goscan|scanrs|nmap)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("请核实扫描是否经过授权，并结合网络连接、工单或客户说明判断。"),
	},
	{
		ID:             "flood-tool-name",
		Severity:       "high",
		Category:       "network-abuse",
		Name:           "疑似 Flood/DDoS 程序",
		Description:    auditStrPtr("匹配常见 Flood、DDoS 工具名称或命令名称。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(syn[-_]?flood|udp[-_]?flood|http[-_]?flood|slowloris|goldeneye|hulk|xerxes|torshammer|ufonet|mhddos|ddosim|layer[47]|flooder|cc[-_]?attack|stress|hammer|python.*(ddos|flood|attack|hammer)|node.*(ddos|flood|attack))\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("建议按高风险处理。手动停止前请先保留进程参数和连接证据。"),
	},
	{
		ID:             "proxy-panel",
		Severity:       "medium",
		Category:       "proxy-panel",
		Name:           "疑似代理面板程序",
		Description:    auditStrPtr("匹配常见代理面板或相关管理面板名称。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(3x-ui|x-ui|v2-ui|s-ui|h-ui|marzban|v2board|sspanel|xboard|trojan-panel|hiddify|nebula|alpine|sub-web|subconverter|bt-panel|aapanel|aa-panel)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("请确认当前节点规则是否允许此类应用，并结合客户业务说明判断。"),
	},
	{
		ID:             "proxy-core",
		Severity:       "medium",
		Category:       "proxy-panel",
		Name:           "疑似代理核心进程",
		Description:    auditStrPtr("匹配常见代理核心、转发核心或隧道类进程名称。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(xray|v2ray|sing-?box|trojan(-go|rs)?|hysteria2?|naive|brook|gost|ss-server|ss-rust|shadowsocks(-rust)?|tuic|juicity|clash(-meta)?|mihomo|reality|xtls|vless|vmess|frp[cs]?|nps|npc|chisel|ligolo(-ng)?|rathole|bore)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("处置前请核对客户用途、节点政策和实际流量，避免误伤正常服务。"),
	},
	{
		ID:             "miner",
		Severity:       "high",
		Category:       "resource-abuse",
		Name:           "疑似挖矿进程",
		Description:    auditStrPtr("匹配常见加密货币挖矿进程名称。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(xmrig|xmrig-proxy|xmr-stak|xmrigcc|cpuminer|minerd|ethminer|lolminer|nbminer|teamredminer|t-rex|gminer|bzminer|srbminer|wildrig|rigel|miniZ|SRBMiner|moneroocean)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("请结合 CPU 占用、运行时间和服务条款人工确认，违规后再执行处置。"),
	},
	{
		ID:             "bruteforce",
		Severity:       "medium",
		Category:       "credential-attack",
		Name:           "疑似爆破/撞库工具",
		Description:    auditStrPtr("匹配常见密码爆破、登录尝试或凭据攻击工具。"),
		TargetTypes:    []string{"process", "startup"},
		MatchType:      "regex",
		PatternText:    `\b(hydra|thc-hydra|medusa|ncrack|patator|crowbar|brutespray|ffuf|gobuster|dirsearch|dirb|sshpass|brute)\b`,
		CaseSensitive:  false,
		Source:         "builtin",
		Enabled:        true,
		Recommendation: auditStrPtr("请检查外连目标、登录尝试记录和客户说明，再决定是否手动处理。"),
	},
}

var (
	auditProcessLineRe   = regexp.MustCompile(`^(\d+)\s+(\d+)\s+(\S+)\s+(\S+)\s+([\d.]+)\s+([\d.]+)\s+(\S+)\s+(\S+)(?:\s+(.*))?$`)
	auditConnPidRe       = regexp.MustCompile(`\bpid=(\d+)`)
	auditStartupSourceRe = regexp.MustCompile(`^\[(.+)\]$`)
	auditMultiSpaceRe    = regexp.MustCompile(`\s+`)
	auditLineSplitRe     = regexp.MustCompile(`\r?\n`)
	auditUDPIRe          = regexp.MustCompile(`(?i)^udp`)
)

func auditNormalizeText(v string) string {
	return strings.TrimSpace(auditMultiSpaceRe.ReplaceAllString(v, " "))
}

// ParseAuditProcesses mirrors parseProcesses in instance-audit.ts.
func ParseAuditProcesses(output string, limit int) []AuditProcess {
	if limit <= 0 {
		limit = 300
	}
	rows := make([]AuditProcess, 0)
	for _, rawLine := range auditLineSplitRe.Split(output, -1) {
		raw := strings.TrimSpace(rawLine)
		if raw == "" {
			continue
		}
		m := auditProcessLineRe.FindStringSubmatch(raw)
		if len(m) < 9 {
			continue
		}
		pidVal, err := strconv.ParseInt(m[1], 10, 32)
		if err != nil {
			continue
		}
		var ppidPtr *int32
		if ppidVal, err := strconv.ParseInt(m[2], 10, 32); err == nil {
			v := int32(ppidVal)
			ppidPtr = &v
		}
		var cpuPtr, memPtr *float64
		if cpu, err := strconv.ParseFloat(m[5], 64); err == nil {
			cpuPtr = &cpu
		}
		if mem, err := strconv.ParseFloat(m[6], 64); err == nil {
			memPtr = &mem
		}
		argsRaw := m[8]
		if len(m) > 9 && m[9] != "" {
			argsRaw = m[9]
		}
		rows = append(rows, AuditProcess{
			PID:           int32(pidVal),
			PPID:          ppidPtr,
			User:          m[3],
			Stat:          m[4],
			CPUPercent:    cpuPtr,
			MemoryPercent: memPtr,
			Elapsed:       m[7],
			Command:       m[8],
			Args:          auditNormalizeText(argsRaw),
			Raw:           raw,
			Findings:      []string{},
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

// ParseAuditConnections mirrors parseConnections in instance-audit.ts.
func ParseAuditConnections(output string, limit int) []AuditConnection {
	if limit <= 0 {
		limit = 300
	}
	rows := make([]AuditConnection, 0)
	for _, rawLine := range auditLineSplitRe.Split(output, -1) {
		raw := strings.TrimSpace(rawLine)
		if raw == "" {
			continue
		}
		fields := strings.Fields(raw)
		if len(fields) < 5 {
			continue
		}
		local := fields[4]
		peer := ""
		if len(fields) > 5 {
			peer = fields[5]
		}
		var procPtr *string
		var pidPtr *int32
		if len(fields) > 6 {
			proc := strings.Join(fields[6:], " ")
			procPtr = &proc
			if m := auditConnPidRe.FindStringSubmatch(proc); len(m) == 2 {
				if v, err := strconv.ParseInt(m[1], 10, 32); err == nil {
					pv := int32(v)
					pidPtr = &pv
				}
			}
		}
		rows = append(rows, AuditConnection{
			Protocol: fields[0],
			State:    fields[1],
			Local:    local,
			Peer:     peer,
			Process:  procPtr,
			PID:      pidPtr,
			Raw:      raw,
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

// ParseAuditStartupItems mirrors parseStartupItems in instance-audit.ts.
func ParseAuditStartupItems(output string, limit int) []AuditStartupItem {
	if limit <= 0 {
		limit = 200
	}
	rows := make([]AuditStartupItem, 0)
	source := "startup"
	for _, rawLine := range auditLineSplitRe.Split(output, -1) {
		raw := strings.TrimSpace(rawLine)
		if raw == "" {
			continue
		}
		if m := auditStartupSourceRe.FindStringSubmatch(raw); len(m) == 2 {
			source = m[1]
			continue
		}
		rows = append(rows, AuditStartupItem{
			Source:   source,
			Command:  auditNormalizeText(raw),
			Raw:      raw,
			Findings: []string{},
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

func auditSeverityRank(sev string) int {
	switch sev {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	default:
		return 0
	}
}

func auditFindMatch(rule AuditRuleDefinition, text string) string {
	if !rule.Enabled {
		return ""
	}
	patternText := strings.TrimSpace(rule.PatternText)
	if patternText == "" {
		return ""
	}
	if rule.MatchType == "regex" {
		expr := patternText
		if !rule.CaseSensitive {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return ""
		}
		return re.FindString(text)
	}
	haystack := text
	needle := patternText
	if !rule.CaseSensitive {
		haystack = strings.ToLower(haystack)
		needle = strings.ToLower(needle)
	}
	if rule.MatchType == "exact" {
		if strings.TrimSpace(haystack) == strings.TrimSpace(needle) {
			return patternText
		}
		return ""
	}
	if strings.Contains(haystack, needle) {
		return patternText
	}
	return ""
}

func auditHasTarget(rule AuditRuleDefinition, target string) bool {
	for _, t := range rule.TargetTypes {
		if t == target {
			return true
		}
	}
	return false
}

func auditSliceString(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
}

func auditLocalizeSubject(targetType, subject string) string {
	switch targetType {
	case "startup":
		return "启动项 " + subject
	case "network":
		return "网络连接 " + subject
	default:
		return subject
	}
}

// AnalyzeAuditData mirrors analyzeAuditData in instance-audit.ts.
func AnalyzeAuditData(input AuditAnalysisInput) AuditAnalysisResult {
	rulesSource := input.Rules
	if len(rulesSource) == 0 {
		rulesSource = BuiltinAuditRules
	}
	rules := make([]AuditRuleDefinition, 0, len(rulesSource))
	for _, r := range rulesSource {
		if r.Enabled {
			rules = append(rules, r)
		}
	}

	findings := make([]AuditFinding, 0)
	now := time.Now().UTC()

	for idx := range input.Processes {
		proc := &input.Processes[idx]
		text := proc.Command + " " + proc.Args
		for _, rule := range rules {
			if !auditHasTarget(rule, "process") {
				continue
			}
			matched := auditFindMatch(rule, text)
			if matched == "" {
				continue
			}
			proc.Findings = append(proc.Findings, rule.Name)
			pid := proc.PID
			subject := fmt.Sprintf("PID %d (%s)", proc.PID, proc.Command)
			findings = append(findings, AuditFinding{
				ID:             fmt.Sprintf("process-%d-%s", idx, rule.ID),
				Severity:       rule.Severity,
				Category:       rule.Category,
				Title:          rule.Name,
				Detail:         auditLocalizeSubject("process", subject) + " 命中审查规则 " + rule.ID + "。",
				TargetType:     "process",
				RuleID:         rule.ID,
				RuleName:       rule.Name,
				RuleSource:     rule.Source,
				MatchedText:    matched,
				Recommendation: rule.Recommendation,
				PID:            &pid,
				Evidence:       auditSliceString(proc.Raw, 800),
			})
		}
	}

	for idx := range input.StartupItems {
		item := &input.StartupItems[idx]
		for _, rule := range rules {
			if !auditHasTarget(rule, "startup") {
				continue
			}
			matched := auditFindMatch(rule, item.Command)
			if matched == "" {
				continue
			}
			item.Findings = append(item.Findings, rule.Name)
			subject := "来源 " + item.Source
			findings = append(findings, AuditFinding{
				ID:             fmt.Sprintf("startup-%d-%s", idx, rule.ID),
				Severity:       rule.Severity,
				Category:       rule.Category,
				Title:          rule.Name,
				Detail:         auditLocalizeSubject("startup", subject) + " 命中审查规则 " + rule.ID + "。",
				TargetType:     "startup",
				RuleID:         rule.ID,
				RuleName:       rule.Name,
				RuleSource:     rule.Source,
				MatchedText:    matched,
				Recommendation: rule.Recommendation,
				Evidence:       auditSliceString(item.Raw, 800),
			})
		}
	}

	udpLines := make([]string, 0)
	listeningCount := 0
	for idx, conn := range input.Connections {
		if auditUDPIRe.MatchString(conn.Protocol) {
			if len(udpLines) < 5 {
				udpLines = append(udpLines, conn.Raw)
			}
		}
		if strings.ToUpper(conn.State) == "LISTEN" {
			listeningCount++
		}
		for _, rule := range rules {
			if !auditHasTarget(rule, "network") {
				continue
			}
			matched := auditFindMatch(rule, conn.Raw)
			if matched == "" {
				continue
			}
			subject := fmt.Sprintf("%s %s -> %s", conn.Protocol, conn.Local, conn.Peer)
			findings = append(findings, AuditFinding{
				ID:             fmt.Sprintf("network-%d-%s", idx, rule.ID),
				Severity:       rule.Severity,
				Category:       rule.Category,
				Title:          rule.Name,
				Detail:         auditLocalizeSubject("network", subject) + " 命中审查规则 " + rule.ID + "。",
				TargetType:     "network",
				RuleID:         rule.ID,
				RuleName:       rule.Name,
				RuleSource:     rule.Source,
				MatchedText:    matched,
				Recommendation: rule.Recommendation,
				PID:            conn.PID,
				Evidence:       auditSliceString(conn.Raw, 800),
			})
		}
	}

	udpCount := 0
	for _, conn := range input.Connections {
		if auditUDPIRe.MatchString(conn.Protocol) {
			udpCount++
		}
	}
	if udpCount >= 100 {
		findings = append(findings, AuditFinding{
			ID:         "network-high-udp-connection-count",
			Severity:   "medium",
			Category:   "network-anomaly",
			Title:      "UDP 连接数量偏高",
			Detail:     fmt.Sprintf("本次扫描从 ss/netstat 返回 %d 条 UDP 连接记录，请结合业务用途判断是否异常。", udpCount),
			TargetType: "network",
			Evidence:   strings.Join(udpLines, "\n"),
		})
	}

	highCPUCount := 0
	for _, proc := range input.Processes {
		if proc.CPUPercent != nil && *proc.CPUPercent >= 80 {
			pid := proc.PID
			findings = append(findings, AuditFinding{
				ID:         fmt.Sprintf("process-%d-high-cpu", proc.PID),
				Severity:   "low",
				Category:   "resource-anomaly",
				Title:      "CPU 占用偏高的进程",
				Detail:     fmt.Sprintf("PID %d 当前 CPU 占用为 %g%%。", proc.PID, *proc.CPUPercent),
				TargetType: "process",
				PID:        &pid,
				Evidence:   auditSliceString(proc.Raw, 500),
			})
			highCPUCount++
			if highCPUCount >= 5 {
				break
			}
		}
	}

	annotated := make([]AuditFinding, 0, len(findings))
	activeCount := 0
	riskLevel := "info"
	for _, f := range findings {
		for _, ignore := range input.Ignores {
			if ignore.ExpiresAt != nil && !ignore.ExpiresAt.UTC().After(now) {
				continue
			}
			if ignore.RuleID != nil && *ignore.RuleID != "" && *ignore.RuleID != f.RuleID {
				continue
			}
			if ignore.TargetType != nil && *ignore.TargetType != "" && *ignore.TargetType != f.TargetType {
				continue
			}
			if ignore.MatchText != nil && *ignore.MatchText != "" {
				hay := strings.ToLower(f.Title + " " + f.Detail + " " + f.Evidence)
				if !strings.Contains(hay, strings.ToLower(*ignore.MatchText)) {
					continue
				}
			}
			f.Ignored = true
			f.IgnoreReason = ignore.Reason
			break
		}
		if !f.Ignored {
			activeCount++
			if activeCount == 1 {
				riskLevel = "low"
			}
			if auditSeverityRank(f.Severity) > auditSeverityRank(riskLevel) {
				riskLevel = f.Severity
			}
		}
		annotated = append(annotated, f)
	}

	sort.SliceStable(annotated, func(i, j int) bool {
		if annotated[i].Ignored != annotated[j].Ignored {
			return !annotated[i].Ignored
		}
		return auditSeverityRank(annotated[i].Severity) > auditSeverityRank(annotated[j].Severity)
	})

	return AuditAnalysisResult{
		Findings: annotated,
		Summary: AuditSummary{
			RiskLevel:        riskLevel,
			ProcessCount:     len(input.Processes),
			ConnectionCount:  len(input.Connections),
			ListeningCount:   listeningCount,
			StartupItemCount: len(input.StartupItems),
			FindingCount:     activeCount,
		},
	}
}

// BuildProcessAuditCommand mirrors buildProcessAuditCommand.
func BuildProcessAuditCommand() string {
	return "ps -eo pid,ppid,user,stat,pcpu,pmem,etime,comm,args --no-headers 2>/dev/null | head -n 300 || true"
}

// BuildConnectionAuditCommand mirrors buildConnectionAuditCommand.
func BuildConnectionAuditCommand() string {
	return "(ss -H -tunap 2>/dev/null || netstat -tunap 2>/dev/null || true) | head -n 300"
}

// BuildStartupAuditCommand mirrors buildStartupAuditCommand.
func BuildStartupAuditCommand() string {
	return strings.Join([]string{
		`printf "[systemd]\n"`,
		`(systemctl list-units --type=service --state=running --no-legend --no-pager 2>/dev/null | head -n 80 || true)`,
		`printf "[cron]\n"`,
		`(crontab -l 2>/dev/null || true)`,
		`printf "[rc-local]\n"`,
		`(test -f /etc/rc.local && sed -n "1,120p" /etc/rc.local || true)`,
		`printf "[supervisor]\n"`,
		`(command -v supervisorctl >/dev/null 2>&1 && supervisorctl status 2>/dev/null || true)`,
		`printf "[pm2]\n"`,
		`(command -v pm2 >/dev/null 2>&1 && pm2 list --no-color 2>/dev/null || true)`,
	}, "; ")
}
