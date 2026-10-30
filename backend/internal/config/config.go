// Package config tüm ayarları ortam değişkenlerinden okur.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	JWTSecret       []byte
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	LogLevel        string
	TimeZone        *time.Location
	SeedValue       int64
	SeedDate        string // YYYY-MM-DD, boşsa bugün
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:        env("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		JWTSecret:       []byte(os.Getenv("JWT_SECRET")),
		AccessTTL:       envDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTTL:      envDuration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
		RequestTimeout:  envDuration("REQUEST_TIMEOUT", 15*time.Second),
		ShutdownTimeout: envDuration("SHUTDOWN_TIMEOUT", 20*time.Second),
		LogLevel:        env("LOG_LEVEL", "info"),
		SeedValue:       envInt("SEED", 42),
		SeedDate:        os.Getenv("SEED_DATE"),
	}
	loc, err := time.LoadLocation(env("AIRPORT_TZ", "Europe/Istanbul"))
	if err != nil {
		return c, fmt.Errorf("AIRPORT_TZ: %w", err)
	}
	c.TimeZone = loc
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL zorunlu")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET en az 32 karakter olmalı")
	}
	return c, nil
}

// OpDate seed/simülasyon için operasyon gününü verir.
func (c Config) OpDate() (time.Time, error) {
	if c.SeedDate == "" {
		now := time.Now().In(c.TimeZone)
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.TimeZone), nil
	}
	return time.ParseInLocation("2006-01-02", c.SeedDate, c.TimeZone)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(k string, def int64) int64 {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
