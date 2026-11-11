// Command server is the Go backend entrypoint, the counterpart of server/src/app.ts.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	app "github.com/rain-kl/payincus/server-go/src"
	"github.com/rain-kl/payincus/server-go/src/config"
	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/services"
	"github.com/rain-kl/payincus/server-go/src/workers"
)

func main() {
	config.LoadEnv()
	lib.LogCookieConfig()

	initCtx, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelInit()

	if err := db.Init(initCtx); err != nil {
		log.Fatalf("database init failed: %v", err)
	}
	defer db.Close()

	if err := db.NormalizeLegacyNetworkModes(initCtx); err != nil {
		log.Printf("network mode normalization skipped: %v", err)
	}

	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3001"
	}

	address := host + ":" + port
	router := app.NewRouter()
	server := &http.Server{Addr: address, Handler: router}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("PayIncus Go backend listening on %s", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// Background work starts after the listener, mirroring app.ts (which starts
	// schedulers and workers after `listen`). Both entry points honour
	// DISABLE_SCHEDULERS / DISABLE_WORKERS and return a stop function that waits
	// for in-flight work.
	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	stopSchedulers := services.StartSchedulers(runCtx)
	stopWorkers := workers.StartWorkers(runCtx)

	shutdown := func(reason string) {
		log.Printf("shutting down: %s", reason)
		stopSchedulers()
		stopWorkers()
		stopRun()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			stopSchedulers()
			stopWorkers()
			stopRun()
			log.Fatalf("server error: %v", err)
		}
		shutdown("server closed")
	case received := <-signals:
		shutdown("signal " + received.String())
	}
}
