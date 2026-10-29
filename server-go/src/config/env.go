// Port of server/src/config/env.ts — load .env without overriding real env vars.
package config

import (
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// LoadEnv loads the first existing .env from a candidate list. godotenv.Load does
// not override variables already present in the environment, matching the Node
// behaviour of `override: false`.
func LoadEnv() {
	candidates := make([]string, 0, 8)
	if envFile := os.Getenv("ENV_FILE"); envFile != "" {
		candidates = append(candidates, envFile)
	}

	cwd, err := os.Getwd()
	if err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, ".env"),
			filepath.Join(cwd, "server", ".env"),
			filepath.Join(cwd, "..", ".env"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, ".env"),
			filepath.Join(exeDir, "..", ".env"),
			filepath.Join(exeDir, "..", "..", ".env"),
		)
	}

	seen := map[string]bool{}
	for _, path := range candidates {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err == nil {
			_ = godotenv.Load(path)
		}
	}
}
