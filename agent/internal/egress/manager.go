package egress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type savedNetwork struct {
	Instruction Instruction `json:"instruction"`
	Inventory   inventory   `json:"inventory"`
	Rules       string      `json:"rules"`
}
type Manager struct {
	mu           sync.Mutex
	dir          string
	resultsReady chan struct{}
	desired      *Instruction
	status       Status
	results      map[string]JobResult
	running      map[string]bool
	jobs         chan ProbeJob
	lastHash     string
	lastHealth   time.Time
	active       int
	failures     map[int]int
	successes    map[int]int
}

func NewManager() *Manager {
	return newManager(stateDir)
}
func newManager(dir string) *Manager {
	m := &Manager{dir: dir, resultsReady: make(chan struct{}, 1), status: Status{State: "idle", Error: "none", XrayAvailable: Available(), ManagedNetworks: []string{}, HealthyNodeIDs: []int{}}, results: map[string]JobResult{}, running: map[string]bool{}, jobs: make(chan ProbeJob, 3), failures: map[int]int{}, successes: map[int]int{}}
	var saved savedNetwork
	if b, e := os.ReadFile(filepath.Join(dir, "applied.json")); e == nil && json.Unmarshal(b, &saved) == nil {
		m.desired = &saved.Instruction
		m.status.AppliedRevision = saved.Instruction.Revision
	}
	var pendingTarget Instruction
	if b, e := os.ReadFile(filepath.Join(dir, "desired.json")); e == nil && json.Unmarshal(b, &pendingTarget) == nil && pendingTarget.Revision > 0 && (m.desired == nil || pendingTarget.Revision >= m.desired.Revision) {
		m.desired = &pendingTarget
	}
	var pending []JobResult
	if b, e := os.ReadFile(filepath.Join(dir, "probe-results.json")); e == nil && json.Unmarshal(b, &pending) == nil {
		for _, r := range pending {
			m.results[r.JobID] = r
		}
	}
	return m
}

// ResultsReady wakes the heartbeat loop after a manual probe finishes. Results
// remain persisted until a successful heartbeat acknowledges them.
func (m *Manager) ResultsReady() <-chan struct{} { return m.resultsReady }
func (m *Manager) Report() (Status, []JobResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	results := []JobResult{}
	for _, r := range m.results {
		results = append(results, r)
	}
	return m.status, results
}
func (m *Manager) AckResults(sent []JobResult, acknowledged []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	accepted := map[string]bool{}
	for _, id := range acknowledged {
		accepted[id] = true
	}
	for _, r := range sent {
		if accepted[r.JobID] {
			delete(m.results, r.JobID)
		}
	}
	m.saveResults()
	if len(acknowledged) > 0 && len(m.results) > 0 {
		select {
		case m.resultsReady <- struct{}{}:
		default:
		}
	}
}
func (m *Manager) saveResults() {
	list := []JobResult{}
	for _, r := range m.results {
		list = append(list, r)
	}
	b, _ := json.Marshal(list)
	_ = atomicFile(filepath.Join(m.dir, "probe-results.json"), b, 0600)
}
func (m *Manager) Sync(in *Instruction) {
	if in == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.desired == nil || in.Revision >= m.desired.Revision {
		copy := *in
		copy.Probes = nil
		copy.AcknowledgedProbeJobs = nil
		// Persist the target before any host mutation. An Agent restart midway
		// through applying a proxy must not revert to an earlier unmanaged mode.
		if copy.Revision > 0 && (m.desired == nil || copy.Revision != m.desired.Revision) {
			body, err := json.Marshal(copy)
			if err == nil {
				err = atomicFile(filepath.Join(m.dir, "desired.json"), body, 0600)
			}
			if err != nil {
				m.status.State = "error"
				m.status.Error = "restore"
				return
			}
		}
		m.desired = &copy
	}
	for _, job := range in.Probes {
		if m.running[job.JobID] {
			continue
		}
		if _, done := m.results[job.JobID]; done {
			continue
		}
		if len(job.Nodes) == 0 || len(job.Nodes) > 10 || len(m.results) >= 20 {
			continue
		}
		select {
		case m.jobs <- job:
			m.running[job.JobID] = true
		default:
		}
	}
}
func (m *Manager) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-m.jobs:
				log.Printf("[egress] probe started job=%s nodes=%d", job.JobID, len(job.Nodes))
				jctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				result := executeJob(jctx, m.dir, job)
				cancel()
				m.mu.Lock()
				m.results[job.JobID] = result
				delete(m.running, job.JobID)
				m.saveResults()
				m.mu.Unlock()
				successes := 0
				for _, item := range result.Items {
					if item.Success {
						successes++
					}
				}
				log.Printf("[egress] probe finished job=%s successes=%d total=%d", job.JobID, successes, len(result.Items))
				select {
				case m.resultsReady <- struct{}{}:
				default:
				}
			}
		}
	}()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *Manager) setError(code string) {
	m.setErrorAt(code, code, nil)
}
func (m *Manager) setErrorAt(code, stage string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	detail := ""
	if err != nil {
		detail = "command_failed"
		var core *coreValidationError
		switch {
		case errors.As(err, &core):
			detail = core.reason
		case errors.Is(err, context.DeadlineExceeded):
			detail = "timeout"
		case os.IsPermission(err):
			detail = "permission_denied"
		case os.IsNotExist(err):
			detail = "missing_file"
		}
	}
	if m.status.State != "error" || m.status.Error != code || m.status.ErrorStage != stage || m.status.ErrorDetail != detail {
		revision := 0
		if m.desired != nil {
			revision = m.desired.Revision
		}
		log.Printf("[egress] apply failed revision=%d stage=%s error=%s reason=%s", revision, stage, code, detail)
	}
	m.status.State = "error"
	m.status.Error = code
	m.status.ErrorStage = stage
	m.status.ErrorDetail = detail
}
func (m *Manager) reconcile(parent context.Context) {
	m.mu.Lock()
	if m.desired == nil {
		m.mu.Unlock()
		return
	}
	in := *m.desired
	m.mu.Unlock()
	if in.Revision == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if in.Egress.Mode != "off" && in.Egress.Mode != "direct" && in.Egress.Mode != "proxy" {
		m.setError("preflight")
		return
	}
	if in.Egress.Mode != "off" && !Available() {
		m.setError("xray")
		return
	}
	for _, name := range []string{"ip", "nft", "systemctl", "sysctl"} {
		if _, e := exec.LookPath(name); e != nil {
			m.setError("preflight")
			return
		}
	}
	c, err := localIncus()
	if err != nil {
		m.setError("incus")
		return
	}
	inv, err := c.inventory(ctx)
	if err != nil {
		m.setError("unsupported-nic")
		return
	}
	if err = ensureOwnership(ctx); err != nil {
		m.setError("preflight")
		return
	}
	encoded, _ := json.Marshal(struct {
		In        Instruction
		Inventory inventory
	}{in, inv})
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	if hash != m.lastHash {
		log.Printf("[egress] applying revision=%d mode=%s proxyNodes=%d", in.Revision, in.Egress.Mode, len(in.Nodes))
		m.mu.Lock()
		m.status.State = "applying"
		m.status.Error = "none"
		m.mu.Unlock()
		if in.Egress.Mode != "off" {
			if e := checkTransparentProxy(ctx); e != nil {
				m.setError("preflight")
				return
			}
		}
		rules, e := hostRules(in, inv)
		if e != nil {
			m.setError("preflight")
			return
		}
		binaryPath := ""
		configPath := filepath.Join(m.dir, "xray.json")
		coreChanged := false
		coreActive := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "incudal-xray.service").Run() == nil
		if in.Egress.Mode != "off" {
			binaryPath, e = extractBinary(m.dir)
			if e != nil {
				m.setErrorAt("xray", "extract", e)
				return
			}
			config, e := xrayConfig(in, nil, true)
			if e != nil {
				m.setErrorAt("xray", "config", e)
				return
			}
			previous, _ := os.ReadFile(configPath)
			unit, _ := os.ReadFile("/etc/systemd/system/incudal-xray.service")
			coreChanged = !bytes.Equal(previous, config) || !bytes.Equal(unit, []byte(gatewayUnit(binaryPath, configPath, in.Egress.Mode == "proxy" && in.Egress.Strategy == "failover"))) || !coreActive
			if e = atomicFile(configPath+".next", config, 0600); e != nil {
				m.setErrorAt("xray", "config", e)
				return
			}
			if e = validateCore(ctx, binaryPath, configPath+".next"); e != nil {
				m.setErrorAt("xray", "config", e)
				return
			}
		}
		if e = c.applyACL(ctx, inv, in.Firewall, in.Egress.Mode != "off"); e != nil {
			m.setError("acl")
			return
		}
		if in.Egress.Mode != "off" {
			if e = prepareNamespace(ctx); e != nil {
				m.setError("network")
				return
			}
		}
		transactionRules := rules
		if in.Egress.Mode != "off" && !coreChanged && coreActive {
			transactionRules += "add element inet incudal_egress ready { 0x420000 }\n"
		}
		if e = replaceHostRules(ctx, transactionRules); e != nil {
			m.setError("network")
			return
		}
		if in.Egress.Mode == "off" {
			if e = cleanGateway(ctx); e != nil {
				m.setError("network")
				return
			}
		} else {
			if e = os.Rename(configPath+".next", configPath); e != nil {
				m.setError("xray")
				return
			}
			unit := gatewayUnit(binaryPath, configPath, in.Egress.Mode == "proxy" && in.Egress.Strategy == "failover")
			if e = atomicFile("/etc/systemd/system/incudal-xray.service", []byte(unit), 0644); e != nil {
				m.setError("xray")
				return
			}
			if coreChanged {
				if e = run(ctx, "systemctl", "daemon-reload"); e == nil {
					e = run(ctx, "systemctl", "enable", "incudal-xray.service")
				}
				if e == nil {
					e = run(ctx, "systemctl", "restart", "incudal-xray.service")
				}
				if e != nil {
					m.setErrorAt("xray", "start", e)
					return
				}
				m.active = 0
				m.lastHealth = time.Time{}
			}
		}
		saved := savedNetwork{Instruction: in, Inventory: inv, Rules: rules}
		body, _ := json.Marshal(saved)
		if e = atomicFile(filepath.Join(m.dir, "applied.json"), body, 0600); e != nil {
			m.setError("restore")
			return
		}
		if in.Egress.Mode == "off" && !in.Firewall.Enabled {
			e = removeRestoreUnit(ctx)
		} else {
			exe, err := os.Executable()
			e = err
			if e == nil {
				e = installRestoreUnit(ctx, exe)
			}
		}
		if e != nil {
			m.setError("restore")
			return
		}
		m.lastHash = hash
		m.mu.Lock()
		m.status.AppliedRevision = in.Revision
		m.status.State = "applied"
		m.status.Error = "none"
		m.status.ErrorStage = ""
		m.status.ErrorDetail = ""
		m.status.ManagedNetworks = inv.Bridges
		m.status.RoutedFallback = len(inv.Routed) > 0 || len(inv.RoutedSources) > 0
		m.mu.Unlock()
		log.Printf("[egress] applied revision=%d networks=%d", in.Revision, len(inv.Bridges))
	}
	if in.Egress.Mode != "off" && exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "incudal-xray.service").Run() != nil {
		m.setErrorAt("xray", "start", nil)
		m.lastHash = ""
		return
	}
	if in.Egress.Mode == "proxy" && time.Since(m.lastHealth) >= time.Duration(in.Egress.ProbeIntervalSeconds)*time.Second {
		m.health(ctx, in)
		m.lastHealth = time.Now()
	}
}
func (m *Manager) health(ctx context.Context, in Instruction) {
	healthy := []int{}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	available := make([]bool, len(in.Nodes))
	var probes sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i := range in.Nodes {
		probes.Add(1)
		go func(index int) {
			defer probes.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			probeCtx, cancel := context.WithTimeout(ctx, 14*time.Second)
			defer cancel()
			out, e := exec.CommandContext(probeCtx, "ip", "netns", "exec", namespace, exe, "-network-probe-port", strconv.Itoa(socksBasePort+index), "-network-probe-url", in.Egress.ProbeURL).Output()
			var result ProbeResult
			available[index] = e == nil && json.Unmarshal(out, &result) == nil && result.Success
		}(i)
	}
	probes.Wait()
	for i, n := range in.Nodes {
		if available[i] {
			m.failures[n.ID] = 0
			m.successes[n.ID]++
		} else {
			m.successes[n.ID] = 0
			m.failures[n.ID]++
		}
		// Never promote an untested node. Active nodes tolerate two misses;
		// recovering preferred nodes require two consecutive successful probes.
		if m.successes[n.ID] > 0 || (m.active == n.ID && m.failures[n.ID] < 3) {
			healthy = append(healthy, n.ID)
		}
	}
	if in.Egress.Strategy == "failover" {
		selected := chooseFailover(in.Nodes, m.active, m.successes, m.failures)
		// Re-assert each round: systemd may have restarted Xray since the last
		// probe. Existing healthy connections stay on their original outbound.
		binaryPath, e := extractBinary(m.dir)
		target := "blocked"
		if selected != 0 {
			target = nodeTag(selected)
		}
		if e == nil {
			e = run(ctx, "ip", "netns", "exec", namespace, binaryPath, "api", "bo", fmt.Sprintf("--server=127.0.0.1:%d", apiPort), "-b", "egress", target)
		}
		if e != nil {
			m.setErrorAt("xray", "health", e)
			return
		}
		m.active = selected
	}
	m.mu.Lock()
	m.status.ActiveNodeID = m.active
	m.status.HealthyNodeIDs = healthy
	if m.status.AppliedRevision == in.Revision {
		m.status.State = "applied"
		m.status.Error = "none"
		m.status.ErrorStage = ""
		m.status.ErrorDetail = ""
	}
	m.mu.Unlock()
}

func chooseFailover(nodes []Node, active int, successes, failures map[int]int) int {
	for _, node := range nodes {
		if successes[node.ID] > 0 && (active == 0 || active == node.ID || successes[node.ID] >= 2) {
			return node.ID
		}
		if active == node.ID && failures[node.ID] < 3 {
			return node.ID
		}
	}
	return 0
}

// RestoreCLI runs before Incus at boot. Failure leaves Incus stopped, rather than
// starting tenant containers with a missing ACL or a direct Internet bypass.
func RestoreCLI() error {
	body, err := os.ReadFile(filepath.Join(stateDir, "applied.json"))
	if err != nil {
		return err
	}
	var saved savedNetwork
	if err = json.Unmarshal(body, &saved); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = replaceHostRules(ctx, saved.Rules); err != nil {
		return err
	}
	if saved.Instruction.Egress.Mode != "off" {
		if err = prepareNamespace(ctx); err != nil {
			return err
		}
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "incudal-xray.service").Run() == nil {
			return nft(ctx, "add element inet incudal_egress ready { 0x420000 }", false)
		}
	}
	return nil
}
