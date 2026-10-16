package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"incudal-agent/internal/config"
	"incudal-agent/internal/egress"
	"incudal-agent/internal/panel"
	"incudal-agent/internal/protocol"
)

// The installer runs -once while the previous service can still be reporting.
// A probe must not overwrite the same binary's capabilities with a smaller list.
func TestHeartbeatCapabilitiesStableAcrossInstallProbe(t *testing.T) {
	const secret = "local-heartbeat-fixture"
	reports := make(chan map[string]any, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		hash := protocol.BodySHA256(body)
		signature := protocol.Signature(secret, protocol.SigningPayload(r.Method, r.URL.Path, r.Header.Get("x-incudal-timestamp"), r.Header.Get("x-incudal-nonce"), hash))
		if hash != r.Header.Get("x-incudal-body-sha256") || signature != r.Header.Get("x-incudal-signature") {
			t.Error("invalid heartbeat signature")
			w.WriteHeader(401)
			return
		}
		var report map[string]any
		if err := json.Unmarshal(body, &report); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		reports <- report
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := panel.New(config.Config{PanelURL: server.URL, AgentID: "local-test-agent", AgentSecret: secret, RequestTimeout: 5 * time.Second})
	worker := &egress.Manager{}
	var previous []any
	for _, once := range []bool{false, true, false} {
		var err error
		if once {
			_, err = sendHeartbeat(context.Background(), client, 60)
		} else {
			_, err = sendHeartbeat(context.Background(), client, 60, worker)
		}
		if err != nil {
			t.Fatal(err)
		}
		report := <-reports
		capabilities, ok := report["capabilities"].([]any)
		if !ok {
			t.Fatal("missing capabilities")
		}
		for _, required := range []string{"xray-egress", "network-control"} {
			count := 0
			for _, value := range capabilities {
				if value == required {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("once=%v: capability %s occurs %d times", once, required, count)
			}
		}
		if previous != nil && !reflect.DeepEqual(previous, capabilities) {
			t.Fatal("install probe changed reported capabilities")
		}
		previous = capabilities
		_, hasStatus := report["networkStatus"]
		if hasStatus == once {
			t.Fatalf("once=%v: incorrect runtime network status", once)
		}
	}
}
