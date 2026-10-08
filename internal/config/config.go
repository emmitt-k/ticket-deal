// Package config loads and validates runtime configuration for the API
// server. All values are validated at Load time so the caller can fail
// fast at startup, before any goroutines, pools, or sockets are open.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds every runtime knob the API server needs.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// JWTSecret is the HS256 signing key. Must be at least 32 bytes so
	// the key space matches the algorithm's stated strength.
	JWTSecret []byte
}

// Load reads configuration from the process environment (after
// overlaying a .env file, if one is present in the working directory)
// and validates the result.
//
// The .env file is best-effort: it is ignored when missing because
// production containers typically inject env vars via Docker / k8s.
// All required values come from the OS environment.
func Load() (*Config, error) {
	// Best-effort .env overlay. ErrNotExist is normal in prod.
	_ = godotenv.Load()

	portStr := getEnv("API_PORT", "8080")
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("API_PORT must be a valid port (1-65535), got %q", portStr)
	}

	secret := os.Getenv("JWT_SECRET")
	if len(secret) < 32 {
		return nil, errors.New("JWT_SECRET must be at least 32 bytes (use `openssl rand -hex 32`)")
	}

	return &Config{
		Addr:      ":" + strconv.Itoa(port),
		JWTSecret: []byte(secret),
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
