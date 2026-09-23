package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Config struct {
	Port string
	// PublicHost is the externally reachable address joiners and hosts use to
	// reach the relay. Falls back to the request Host header when empty.
	PublicHost string

	RelayPortMin int
	RelayPortMax int

	MaxRooms  int
	MaxRelays int

	MaxConnsPerRelay int
	RelayIdleTimeout time.Duration
	RelayMaxBytes    int64
	RelayConnRPM     int
	RelayConnBurst   int

	TrustProxy         bool
	AllowPrivateHostIP bool

	RateLimitRPM   int
	RateLimitBurst int

	BcryptCost int

	RequireJoinAuth bool
	JoinAuthTTL     time.Duration

	// AdminToken enables the /admin endpoints when non-empty.
	AdminToken string
}

func LoadConfig() Config {
	var cfg Config

	cfg.Port = envString("PORT", "8080")
	cfg.PublicHost = os.Getenv("PUBLIC_HOST")

	cfg.RelayPortMin = envInt("RELAY_PORT_MIN", 10000, 1024, 65535)
	cfg.RelayPortMax = envInt("RELAY_PORT_MAX", 10099, 1024, 65535)
	if cfg.RelayPortMin > cfg.RelayPortMax {
		log.Fatalf("RELAY_PORT_MIN (%d) must be <= RELAY_PORT_MAX (%d)", cfg.RelayPortMin, cfg.RelayPortMax)
	}

	cfg.MaxRooms = envInt("MAX_ROOMS", 100, 1, 10000)
	cfg.MaxRelays = envInt("MAX_RELAYS", 50, 1, 10000)

	// Each relay uses two ports (one for the host, one for joiners).
	if ports := cfg.RelayPortMax - cfg.RelayPortMin + 1; cfg.MaxRelays*2 > ports {
		log.Printf("Warning: each relay uses 2 ports, so MAX_RELAYS (%d) needs %d ports but the range has %d; relay requests will fail once ports run out",
			cfg.MaxRelays, cfg.MaxRelays*2, ports)
	}

	cfg.MaxConnsPerRelay = envInt("MAX_CONNS_PER_RELAY", 8, 1, 1000)
	cfg.RelayIdleTimeout = time.Duration(envInt("RELAY_IDLE_TIMEOUT_SECONDS", 300, 5, 86400)) * time.Second
	cfg.RelayMaxBytes = int64(envInt("RELAY_MAX_BYTES", 1<<30, 0, 1<<50))
	cfg.RelayConnRPM = envInt("RELAY_CONN_RPM", 120, 0, 1000000)
	cfg.RelayConnBurst = envInt("RELAY_CONN_BURST", 30, 1, 100000)

	cfg.TrustProxy = envBool("TRUST_PROXY", false)
	cfg.AllowPrivateHostIP = envBool("ALLOW_PRIVATE_HOST_IP", false)

	cfg.RateLimitRPM = envInt("RATE_LIMIT_RPM", 200, 0, 1000000)
	cfg.RateLimitBurst = envInt("RATE_LIMIT_BURST", 30, 1, 100000)

	cfg.BcryptCost = envInt("BCRYPT_COST", bcrypt.DefaultCost, 10, 14)

	cfg.RequireJoinAuth = envBool("REQUIRE_JOIN_AUTH", true)
	cfg.JoinAuthTTL = time.Duration(envInt("JOIN_AUTH_TTL_SECONDS", 120, 5, 3600)) * time.Second

	cfg.AdminToken = os.Getenv("ADMIN_TOKEN")
	if cfg.AdminToken == "" {
		log.Printf("Note: ADMIN_TOKEN is not set; the /admin endpoints are disabled")
	}

	return cfg
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def, min, max int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		log.Fatalf("Invalid %s (must be %d-%d)", key, min, max)
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "":
		return def
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		log.Fatalf("Invalid %s (must be true or false)", key)
		return def
	}
}
