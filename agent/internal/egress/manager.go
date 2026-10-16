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
	desired      *Instruction
	status       Status
	applyGate    chan struct{}
	probeGate    chan struct{}
	wake         chan struct{}
	healthCancel context.CancelFunc
	lastHash     string
	lastHealth   time.Time
	active       int
	failures     map[int]int
	successes    map[int]int
}

func NewManager() *Manager { return newManager(stateDir) }
func newManager(dir string) *Manager {
	m := &Manager{dir: dir, status: Status{State: "idle", Error: "none", XrayAvailable: Available(), ManagedNetworks: []string{}, HealthyNodeIDs: []int{}}, applyGate: make(chan struct{}, 1), probeGate: make(chan struct{}, 1), wake: make(chan struct{}, 1), failures: map[int]int{}, successes: map[int]int{}}
	var saved savedNetwork
	if b, e := os.ReadFile(filepath.Join(dir, "applied.json")); e == nil && json.Unmarshal(b, &saved) == nil {
		m.desired = &saved.Instruction
		m.status.AppliedRevision = saved.Instruction.Revision
	}
	var pending Instruction
	if b, e := os.ReadFile(filepath.Join(dir, "desired.json")); e == nil && json.Unmarshal(b, &pending) == nil && pending.Revision > 0 && (m.desired == nil || pending.Revision >= m.desired.Revision) {
		m.desired = &pending
	}
	// Manual tests are request-scoped; discard the retired worker's result cache.
	_ = os.Remove(filepath.Join(dir, "probe-results.json"))
	return m
}
func (m *Manager) Report() Status { m.mu.Lock(); defer m.mu.Unlock(); return m.status }
func (m *Manager) Sync(in *Instruction) error {
	if in == nil || in.Revision <= 0 {
		return fmt.Errorf("invalid_request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.desired != nil && in.Revision < m.desired.Revision {
		return nil
	}
	copy := *in
	body, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	if m.desired == nil || in.Revision != m.desired.Revision {
		if err = atomicFile(filepath.Join(m.dir, "desired.json"), body, 0600); err != nil {
			return err
		}
	}
	m.desired = &copy
	if m.healthCancel != nil {
		m.healthCancel()
	}
	return nil
}
func (m *Manager) Apply(ctx context.Context, in Instruction) (Status, error) {
	if err := ctx.Err(); err != nil {
		return m.Report(), err
	}
	if err := m.Sync(&in); err != nil {
		m.setErrorAt("restore", "restore", err)
		return m.Report(), nil
	}
	if err := m.reconcile(ctx, false); err != nil {
		return m.Report(), err
	}
	status := m.Report()
	if status.State == "applied" && in.Egress.Mode == "proxy" {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
	return status, nil
}
func (m *Manager) Probe(ctx context.Context, request ProbeRequest) ([]ProbeResult, error) {
	if len(request.Nodes) == 0 || len(request.Nodes) > 10 {
		return nil, fmt.Errorf("invalid_request")
	}
	select {
	case m.probeGate <- struct{}{}:
		defer func() { <-m.probeGate }()
	default:
		return nil, fmt.Errorf("busy")
	}
	log.Printf("[egress] realtime probe started nodes=%d", len(request.Nodes))
	result := executeProbe(ctx, m.dir, request)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	successes := 0
	for _, item := range result {
		if item.Success {
			successes++
		}
	}
	log.Printf("[egress] realtime probe finished successes=%d total=%d", successes, len(result))
	return result, nil
}
func (m *Manager) HandleControl(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "network.apply":
		var in Instruction
		if json.Unmarshal(params, &in) != nil || in.Revision <= 0 || (in.Egress.Mode != "off" && in.Egress.Mode != "direct" && in.Egress.Mode != "proxy") || len(in.Nodes) > 16 || len(in.Firewall.Rules) > 200 {
			return nil, fmt.Errorf("invalid_request")
		}
		return m.Apply(ctx, in)
	case "proxy.probe":
		var in ProbeRequest
		if json.Unmarshal(params, &in) != nil || len(in.ProbeURL) > 2048 {
			return nil, fmt.Errorf("invalid_request")
		}
		items, err := m.Probe(ctx, in)
		return map[string]any{"items": items}, err
	default:
		return nil, fmt.Errorf("invalid_request")
	}
}
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		_ = m.reconcile(ctx, true)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.wake:
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
func (m *Manager) reconcile(parent context.Context, withHealth bool) error {
	select {
	case m.applyGate <- struct{}{}:
	case <-parent.Done():
		return parent.Err()
	}
	defer func() { <-m.applyGate }()
	m.reconcileLocked(parent, withHealth)
	return nil
}
func (m *Manager) reconcileLocked(parent context.Context, withHealth bool) {
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
	for _, name := range []string{"ip", "nft", "systemctl", "sysctl", "nsenter", "mount"} {
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
	var resolvers []string
	if in.Egress.Mode != "off" {
		resolvers, err = hostBootstrapResolvers(inv)
		if err != nil {
			log.Printf("[egress] preflight failed: host DNS upstreams unavailable")
			m.setError("preflight")
			return
		}
	}
	encoded, _ := json.Marshal(struct {
		In        Instruction
		Inventory inventory
		Resolvers []string
	}{in, inv, resolvers})
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
			previousResolver, _ := os.ReadFile(namespaceResolverPath)
			unit, _ := os.ReadFile("/etc/systemd/system/incudal-xray.service")
			// ip netns exec bind-mounts resolv.conf. Atomic replacement cannot update
			// that existing mount; restart the core when bootstrap upstreams change.
			coreChanged = !bytes.Equal(previousResolver, namespaceResolverConfig(resolvers)) || !bytes.Equal(previous, config) || !bytes.Equal(unit, []byte(gatewayUnit(binaryPath, configPath, in.Egress.Mode == "proxy" && in.Egress.Strategy == "failover"))) || !coreActive
			if e = atomicFile(configPath+".next", config, 0600); e != nil {
				m.setErrorAt("xray", "config", e)
				return
			}
			if e = validateCore(ctx, binaryPath, configPath+".next"); e != nil {
				m.setErrorAt("xray", "config", e)
				return
			}
		}
		// In direct mode preserve Incus's native host DNS upstreams. Only proxy
		// mode forwards bridge DNS over the selected remote proxy outbound.
		if e = c.applyACL(ctx, inv, in.Firewall, in.Egress.Mode == "proxy"); e != nil {
			m.setError("acl")
			return
		}
		if in.Egress.Mode != "off" {
			if e = prepareNamespace(ctx, resolvers...); e != nil {
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
	if withHealth && in.Egress.Mode == "proxy" && time.Since(m.lastHealth) >= time.Duration(in.Egress.ProbeIntervalSeconds)*time.Second {
		healthCtx, stopHealth := context.WithCancel(ctx)
		m.mu.Lock()
		if m.desired.Revision != in.Revision {
			m.mu.Unlock()
			stopHealth()
			return
		}
		m.healthCancel = stopHealth
		m.mu.Unlock()
		m.health(healthCtx, in)
		stopHealth()
		m.mu.Lock()
		m.healthCancel = nil
		m.mu.Unlock()
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
			out, e := networkCommand(probeCtx, "ip", "netns", "exec", namespace, exe, "-network-probe-port", strconv.Itoa(socksBasePort+index), "-network-probe-url", in.Egress.ProbeURL).Output()
			var result ProbeResult
			available[index] = e == nil && json.Unmarshal(out, &result) == nil && result.Success
		}(i)
	}
	probes.Wait()
	if ctx.Err() != nil {
		return
	}
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
	if m.lastHealth.IsZero() || m.status.ActiveNodeID != m.active || len(m.status.HealthyNodeIDs) != len(healthy) {
		log.Printf("[egress] proxy health healthy=%d total=%d activeNode=%d", len(healthy), len(in.Nodes), m.active)
		if len(healthy) == 0 {
			log.Printf("[egress] no healthy proxy outbounds; new proxied connections remain blocked")
		}
	}
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
		resolvers, resolverErr := hostBootstrapResolvers(saved.Inventory)
		if resolverErr != nil {
			return resolverErr
		}
		previousResolver, _ := os.ReadFile(namespaceResolverPath)
		if err = prepareNamespace(ctx, resolvers...); err != nil {
			return err
		}
		if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "incudal-xray.service").Run() == nil {
			if !bytes.Equal(previousResolver, namespaceResolverConfig(resolvers)) {
				// Restore's unit is ordered Before=Xray. Waiting for a restart here
				// would make both jobs wait on each other. Keep readiness empty until
				// the queued core restart completes its own ExecStartPost hook.
				return run(ctx, "systemctl", "--no-block", "restart", "incudal-xray.service")
			}
			return nft(ctx, "add element inet incudal_egress ready { 0x420000 }", false)
		}
	}
	return nil
}
