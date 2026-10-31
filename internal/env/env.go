// Package env reads settings from the environment, with defaults.
package env

import (
	"log"
	"os"
	"strconv"
	"time"
)

func Get(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Duration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("[proxy] %s=%q is not a duration, using %s", key, v, def)
	}
	return def
}

func Float(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		log.Printf("[proxy] %s=%q is not a number, using %g", key, v, def)
	}
	return def
}

func Must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}
