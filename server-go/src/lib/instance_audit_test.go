package lib

import "testing"

func TestAuditParseAndAnalyze(t *testing.T) {
	psOut := "  123     1 root     Ss    92.5  0.1 01:00 xmrig /usr/bin/xmrig --url pool\n  456     1 www      Ss     0.1  0.2 02:00 nginx nginx: worker\n"
	ssOut := "tcp   LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=456,fd=6))\n"
	stOut := "[systemd]\nfoo.service loaded active running xray\n"

	procs := ParseAuditProcesses(psOut, 300)
	conns := ParseAuditConnections(ssOut, 300)
	starts := ParseAuditStartupItems(stOut, 200)
	if len(procs) != 2 || len(conns) != 1 || len(starts) != 1 {
		t.Fatalf("parsed counts = %d %d %d", len(procs), len(conns), len(starts))
	}
	if conns[0].PID == nil || *conns[0].PID != 456 {
		t.Fatalf("conn pid = %v", conns[0].PID)
	}

	res := AnalyzeAuditData(AuditAnalysisInput{
		Processes:    procs,
		Connections:  conns,
		StartupItems: starts,
	})
	if res.Summary.RiskLevel != "high" || res.Summary.FindingCount != 3 || res.Summary.ListeningCount != 1 {
		t.Fatalf("summary = %#v findings=%#v", res.Summary, res.Findings)
	}

	// Test ignore rule suppresses active finding count.
	ruleID := "miner"
	resIgnored := AnalyzeAuditData(AuditAnalysisInput{
		Processes:    ParseAuditProcesses(psOut, 300),
		Connections:  conns,
		StartupItems: starts,
		Ignores: []AuditIgnoreDefinition{
			{ID: 1, RuleID: &ruleID},
		},
	})
	if resIgnored.Summary.FindingCount != 2 {
		t.Fatalf("expected miner ignored -> active count 2, got %d", resIgnored.Summary.FindingCount)
	}
}
