// Command server is the Go backend entrypoint, the counterpart of server/src/app.ts.
package main

import (
	"context"
	"log"
	"os"
	"time"

	app "github.com/rain-kl/payincus/server-go/src"
	"github.com/rain-kl/payincus/server-go/src/config"
	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

func main() {
	config.LoadEnv()
	lib.LogCookieConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.Init(ctx); err != nil {
		log.Fatalf("database init failed: %v", err)
	}
	defer db.Close()

	if err := db.NormalizeLegacyNetworkModes(ctx); err != nil {
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

	router := app.NewRouter()
	address := host + ":" + port
	log.Printf("PayIncus Go backend listening on %s", address)
	if err := router.Run(address); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
