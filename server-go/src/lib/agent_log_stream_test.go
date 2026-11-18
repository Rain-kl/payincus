package lib

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentLogStreamManagerSubscribeBroadcastAndUnsubscribe(t *testing.T) {
	tunnel := NewTunnelManager(nil)
	mgr := NewAgentLogStreamManager(tunnel)
	mgr.keepalive = 20 * time.Millisecond
	mgr.probeAgent = func(context.Context, int32) bool { return true }

	sock := newTunnelTestSocket()
	tunnel.RegisterTunnel(8801, sock)

	var (
		mu     sync.Mutex
		chunks []string
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := mgr.Subscribe(ctx, 8801, func(chunk string) bool {
		mu.Lock()
		chunks = append(chunks, chunk)
		mu.Unlock()
		return true
	}); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	line := "systemd[1]: Started foo.service"
	tunnel.HandleTunnelMessage(8801, sock, TunnelEncodeFrame(TunnelFrameLogData, 0, []byte(line)))
	tunnel.HandleTunnelMessage(8801, sock, TunnelEncodeFrame(TunnelFrameLogData, 0, nil))
	time.Sleep(35 * time.Millisecond)

	mu.Lock()
	joined := strings.Join(chunks, "")
	mu.Unlock()

	if !strings.Contains(joined, "event: status\ndata: ") ||
		!strings.Contains(joined, `"agentOnline":true`) ||
		!strings.Contains(joined, `"tunnelOnline":true`) {
		t.Fatalf("missing initial status event: %q", joined)
	}
	if !strings.Contains(joined, `event: log`+"\n"+`data: "systemd[1]: Started foo.service"`) {
		t.Fatalf("missing log event: %q", joined)
	}
	if !strings.Contains(joined, "event: end\ndata: {}\n\n") {
		t.Fatalf("missing end event: %q", joined)
	}
	if !strings.Contains(joined, ": ping\n\n") {
		t.Fatalf("missing keepalive ping: %q", joined)
	}

	// Cancel context -> unsubscribes and clears session + sends stop control.
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(mgr.sessionClients(8801)) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(mgr.sessionClients(8801)) != 0 {
		t.Fatal("expected session cleanup after cancel")
	}
	tunnel.UnregisterTunnel(8801, nil)
}
