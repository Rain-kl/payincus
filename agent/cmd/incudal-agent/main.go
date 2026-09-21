package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"incudal-agent/internal/caddy"
	"incudal-agent/internal/config"
	"incudal-agent/internal/panel"
	"incudal-agent/internal/report"
	"incudal-agent/internal/tunnel"
	"incudal-agent/internal/upgrade"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/incudal-agent/config.yaml", "agent config file")
	once := flag.Bool("once", false, "send one heartbeat and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := panel.New(cfg)
	if *once {
		if _, err := sendHeartbeat(ctx, client, cfg.HeartbeatIntervalSeconds); err != nil {
			log.Fatalf("heartbeat: %v", err)
		}
		return
	}

	tunnelWorker := tunnel.NewWorker(tunnel.WorkerConfig{
		PanelURL:    cfg.PanelURL,
		AgentID:     cfg.AgentID,
		AgentSecret: cfg.AgentSecret,
	})
	defer tunnelWorker.SyncConfig(false, "", 0)

	log.Printf("incudal-agent started: panel=%s interval=%s", cfg.PanelURL, cfg.HeartbeatInterval)
	upgradeRunner := upgrade.DefaultRunner(cfg)
	var upgradeInProgress atomic.Bool
	var caddyOpInProgress atomic.Bool
	heartbeatLogState := newHeartbeatLogState()
	if result, err := sendHeartbeat(ctx, client, cfg.HeartbeatIntervalSeconds); err != nil {
		heartbeatLogState.logFailure(err)
	} else {
		heartbeatLogState.logSuccess(result)
		if result.Tunnel != nil {
			tunnelWorker.SyncConfig(result.Tunnel.Enabled, result.Tunnel.TargetHost, result.Tunnel.TargetPort)
		}
		handleCaddyInstruction(ctx, result, &caddyOpInProgress)
		scheduleAgentUpgrade(ctx, upgradeRunner, result, &upgradeInProgress)
	}

	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("incudal-agent stopped")
			return
		case <-ticker.C:
			if result, err := sendHeartbeat(ctx, client, cfg.HeartbeatIntervalSeconds); err != nil {
				heartbeatLogState.logFailure(err)
			} else {
				heartbeatLogState.logSuccess(result)
				if result.Tunnel != nil {
					tunnelWorker.SyncConfig(result.Tunnel.Enabled, result.Tunnel.TargetHost, result.Tunnel.TargetPort)
				}
				handleCaddyInstruction(ctx, result, &caddyOpInProgress)
				scheduleAgentUpgrade(ctx, upgradeRunner, result, &upgradeInProgress)
			}
		}
	}
}

// handleCaddyInstruction 处理面板下发的 Caddy 指令（install / uninstall）。
// 幂等：对应操作进行中跳过；操作系统为原子互斥（安装/卸载不会并发）。
func handleCaddyInstruction(ctx context.Context, result panel.HeartbeatResult, inProgress *atomic.Bool) {
	if result.Caddy == nil {
		return
	}
	log.Printf("[caddy] instruction: command=%q port=%d", result.Caddy.Command, result.Caddy.Port)
	if inProgress.Load() {
		return
	}
	if result.Caddy.Command != "install" && result.Caddy.Command != "uninstall" {
		return
	}
	if !inProgress.CompareAndSwap(false, true) {
		return
	}
	switch result.Caddy.Command {
	case "install":
		if caddy.Detect().Available {
			inProgress.Store(false)
			return
		}
		log.Printf("[caddy] install command received; deploying Caddy on loopback")
		go func() {
			defer inProgress.Store(false)
			installCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if err := caddy.Install(installCtx); err != nil {
				log.Printf("[caddy] install failed: %v", err)
				return
			}
			log.Printf("[caddy] install complete")
		}()

	case "uninstall":
		if !caddy.Detect().Available {
			inProgress.Store(false)
			return
		}
		log.Printf("[caddy] uninstall command received; removing Caddy from host")
		go func() {
			defer inProgress.Store(false)
			uninstallCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := caddy.Uninstall(uninstallCtx); err != nil {
				log.Printf("[caddy] uninstall failed: %v", err)
				return
			}
			log.Printf("[caddy] uninstall complete")
		}()
	}
}

func sendHeartbeat(ctx context.Context, client *panel.Client, heartbeatIntervalSeconds int) (panel.HeartbeatResult, error) {
	result, err := client.Heartbeat(ctx, report.HeartbeatPayload(version, heartbeatIntervalSeconds))
	if err != nil {
		return result, err
	}
	return result, nil
}

type heartbeatLogState struct {
	successCount   int64
	failureCount   int64
	lastSuccessLog time.Time
	lastFailureLog time.Time
}

func newHeartbeatLogState() *heartbeatLogState {
	return &heartbeatLogState{}
}

func (state *heartbeatLogState) logSuccess(result panel.HeartbeatResult) {
	state.successCount++
	upgradeAvailable := result.Upgrade != nil && result.Upgrade.Available
	now := time.Now()
	if state.successCount == 1 || upgradeAvailable || now.Sub(state.lastSuccessLog) >= 10*time.Minute {
		log.Printf("heartbeat ok: status=%d latencyMs=%d upgrade=%t count=%d", result.StatusCode, result.LatencyMs, upgradeAvailable, state.successCount)
		state.lastSuccessLog = now
	}
}

func (state *heartbeatLogState) logFailure(err error) {
	state.failureCount++
	now := time.Now()
	if state.failureCount == 1 || now.Sub(state.lastFailureLog) >= time.Minute {
		log.Printf("heartbeat failed: %v count=%d", err, state.failureCount)
		state.lastFailureLog = now
	}
}

func scheduleAgentUpgrade(ctx context.Context, runner *upgrade.Runner, result panel.HeartbeatResult, upgradeInProgress *atomic.Bool) {
	if result.Upgrade == nil || !result.Upgrade.Available {
		return
	}
	if !upgradeInProgress.CompareAndSwap(false, true) {
		log.Printf("agent upgrade already scheduled: version=%s", result.Upgrade.Version)
		return
	}

	instruction := *result.Upgrade
	log.Printf("agent upgrade scheduled: version=%s", instruction.Version)
	go func() {
		defer upgradeInProgress.Store(false)

		if delay := upgrade.RandomJitter(5 * time.Minute); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		upgradeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := runner.Apply(upgradeCtx, instruction, version); err != nil {
			log.Printf("agent upgrade failed: version=%s error=%v", instruction.Version, err)
			return
		}
		log.Printf("agent upgrade applied: version=%s", instruction.Version)
	}()
}
