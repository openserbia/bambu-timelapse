// Package config reads the service's environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for every tunable. Named so the zero-config behaviour is readable
// in one place rather than scattered through Load as literals.
const (
	defaultFPS            = 20
	defaultMinFrames      = 30
	defaultFinalDelaySecs = 45
	defaultCaptureSecs    = 25
	defaultMinFreeMB      = 2048
	defaultFailedTTLDays  = 7

	mib = 1024 * 1024
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Host       string
	Serial     string
	AccessCode string
	// PrinterName is an operator alias used in the posted filename. Never the
	// serial: that is the MQTT topic key and the cloud-binding identifier, and
	// the filename is published to a Telegram channel.
	PrinterName string

	APIURL   string
	APIToken string
	ChatID   int64
	TopicID  int
	Silent   bool

	StagingDir     string
	FPS            int
	MinFrames      int
	FinalDelay     time.Duration
	CaptureTimeout time.Duration
	MinFree        uint64
	FailedTTL      time.Duration
	ListenAddr     string
}

// Load reads the environment, returning every problem at once rather than the
// first: a misconfigured deploy should need one restart to diagnose, not five.
func Load() (*Config, error) {
	var errs []error
	req := func(key string) string {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	num := func(key string, def int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a number", key, raw))
			return def
		}
		return n
	}
	str := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}

	cfg := &Config{
		Host:           req("PRINTER_HOST"),
		Serial:         req("PRINTER_SERIAL"),
		AccessCode:     req("PRINTER_ACCESS_CODE"),
		PrinterName:    str("PRINTER_NAME", "printer"),
		APIURL:         req("MEDIA_API_URL"),
		APIToken:       req("MEDIA_API_TOKEN"),
		TopicID:        num("TELEGRAM_TOPIC_ID", 0),
		Silent:         truthy(str("TELEGRAM_SILENT", "true")),
		StagingDir:     str("STAGING_DIR", "/staging"),
		FPS:            num("TIMELAPSE_FPS", defaultFPS),
		MinFrames:      num("MIN_FRAMES", defaultMinFrames),
		FinalDelay:     time.Duration(num("FINAL_FRAME_DELAY", defaultFinalDelaySecs)) * time.Second,
		CaptureTimeout: time.Duration(num("CAPTURE_TIMEOUT", defaultCaptureSecs)) * time.Second,
		MinFree:        megabytes(num("MIN_FREE_MB", defaultMinFreeMB)),
		FailedTTL:      time.Duration(num("FAILED_TTL_DAYS", defaultFailedTTLDays)) * 24 * time.Hour,
		ListenAddr:     str("LISTEN_ADDR", ":8092"),
	}

	if raw := req("TELEGRAM_CHAT_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("TELEGRAM_CHAT_ID: %q is not a number", raw))
		}
		cfg.ChatID = id
	}
	if cfg.FPS <= 0 {
		errs = append(errs, errors.New("TIMELAPSE_FPS must be positive"))
	}
	return cfg, errors.Join(errs...)
}

// megabytes converts a configured MB value to bytes, clamping a negative
// setting to zero rather than wrapping it into a huge uint64 — a typo'd
// "-1" would otherwise disable capture entirely by making every disk look
// full.
func megabytes(n int) uint64 {
	if n <= 0 {
		return 0
	}
	return uint64(n) * mib
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
